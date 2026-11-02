//go:build unix

package userdata

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// These are spawn#1479, the --mpi-command twin of #1459. mpirun ran each rank
// as `bash /etc/spawn/mpi-command`, and Open MPI keeps a rank, and so the job,
// alive until the I/O-forwarding channels it gave the rank close. A background
// job the command never waited for inherits them, so mpirun, SPAWN_EXITCODE
// and every member's --on-complete waited out its whole lifetime (600 s live).
// The owner's decision, as for #1459: completion follows the command's own
// process; a rank's stragglers are not waited for.
//
// They run whatever the rendered mpirun line hands each rank, not the wrapper
// by name, so a revert of the mpirun line is caught here. The drain is counted,
// not timed: a stub `sleep` on PATH records each poll, so "bounded" is exactly
// the wrapper's poll count and "no added delay" is far fewer. Only the drain
// sleeps through PATH; the stragglers use /bin/sleep.

// rankDrainPolls is the wrapper's drain bound, in polls. The launcher test
// TestMPIRankDrainMatchesCommandDrain ties it to --command's (#1459).
func rankDrainPolls(t *testing.T, body string) int {
	t.Helper()
	m := regexp.MustCompile(`\[ \$N -lt (\d+) \]`).FindAllStringSubmatch(body, -1)
	if len(m) != 1 {
		t.Fatalf("want one `[ $N -lt <polls> ]` drain bound in the rank wrapper, found %d:\n%s", len(m), body)
	}
	n, _ := strconv.Atoi(m[0][1])
	return n
}

// rankDrainPoll is the wrapper's poll interval, the argument of its one sleep.
func rankDrainPoll(t *testing.T, body string) string {
	t.Helper()
	m := regexp.MustCompile(`\n\s*sleep (\S+)\n`).FindAllStringSubmatch(body, -1)
	if len(m) != 1 {
		t.Fatalf("want one drain sleep in the rank wrapper, found %d:\n%s", len(m), body)
	}
	return m[0][1]
}

// rankProgram is what the rendered mpirun invocation runs on each rank: the
// words after the hostfile, before the output redirection.
func rankProgram(t *testing.T, script string) string {
	t.Helper()
	line := mpirunLine(t, script)
	m := regexp.MustCompile(`-hostfile /tmp/mpi-hostfile (.+?) 2>&1`).FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("cannot find the program mpirun runs per rank in:\n%s", line)
	}
	return m[1]
}

// rankSandbox holds the rendered files under a temp dir and the counting sleep.
type rankSandbox struct {
	dir     string
	program []string // argv of the per-rank program, paths rewritten
	polls   int      // the rendered drain bound
	poll    string   // the rendered poll interval
}

func newRankSandbox(t *testing.T, command string) *rankSandbox {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	script := mpiScriptWithCommand(t, command)
	dir := t.TempDir()
	rw := strings.NewReplacer(
		"/etc/spawn/", dir+"/",
		"/tmp/spawn-mpi-rank.", dir+"/spawn-mpi-rank.",
	)
	body := rankWrapperBody(t, script)
	for name, text := range map[string]string{
		"mpi-rank":    rw.Replace(body),
		"mpi-command": commandFileBody(t, script) + "\n",
		"bin/sleep": fmt.Sprintf("#!/bin/bash\necho \"$*\" >> %q\nexec /bin/sleep 0.02\n",
			filepath.Join(dir, "sleep-calls")),
	} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o755); err != nil { //nolint:gosec // test fixture scripts
			t.Fatal(err)
		}
	}
	return &rankSandbox{
		dir:     dir,
		program: strings.Fields(rw.Replace(rankProgram(t, script))),
		polls:   rankDrainPolls(t, body),
		poll:    rankDrainPoll(t, body),
	}
}

func (sb *rankSandbox) env() []string {
	env := []string{"PATH=" + filepath.Join(sb.dir, "bin") + ":" + os.Getenv("PATH"), "HOME=" + sb.dir}
	if os.Geteuid() == 0 { // CI containers: mpirun refuses root without these
		env = append(env, "OMPI_ALLOW_RUN_AS_ROOT=1", "OMPI_ALLOW_RUN_AS_ROOT_CONFIRM=1")
	}
	return env
}

func (sb *rankSandbox) read(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(sb.dir, name))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

// drainPolls counts the drain's sleeps.
func (sb *rankSandbox) drainPolls(t *testing.T) int {
	n := 0
	for _, l := range strings.Split(sb.read(t, "sleep-calls"), "\n") {
		if l == sb.poll {
			n++
		}
	}
	return n
}

// rankStraggler leaves a /bin/sleep holding the rank's output, records its
// PID in straggler.<rank>, writes the rank's last line and exits with exit.
func rankStraggler(dir, exit string) string {
	return fmt.Sprintf("/bin/sleep 30 &\necho $! > %q.${OMPI_COMM_WORLD_RANK:-0}\necho %s-${OMPI_COMM_WORLD_RANK:-0}\n%s",
		filepath.Join(dir, "straggler"), rankLastLine, exit)
}

// stragglerPIDs reads the stragglers' PIDs and kills them at cleanup, so the
// test leaves nothing running.
func stragglerPIDs(t *testing.T, dir string, ranks int) []int {
	t.Helper()
	var pids []int
	for r := 0; r < ranks; r++ {
		b, err := os.ReadFile(filepath.Join(dir, "straggler."+strconv.Itoa(r)))
		if err != nil {
			t.Fatalf("rank %d's straggler did not record its PID: %v", r, err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
		pids = append(pids, pid)
	}
	return pids
}

// The straggler is written before the sandbox exists, so its PID file goes in
// a directory made first and the command text names it.
func stragglerDir(t *testing.T) string { return t.TempDir() }

// TestMPIRankReleasesItsOutputAtTheCommandsExit runs the per-rank program the
// way orted does, with its stdout and stderr on a pipe the test reads, which
// stands in for Open MPI's forwarding channel: the rank is not over until that
// pipe reaches EOF. With a straggler holding the command's output, EOF must
// come while the straggler is still alive, after exactly the drain bound, with
// the command's exit code (plain and signal) and its last line ahead of the
// wrapper's notice naming the straggler. Against `bash /etc/spawn/mpi-command`
// EOF waits for the straggler (30 s) and this fails.
func TestMPIRankReleasesItsOutputAtTheCommandsExit(t *testing.T) {
	for name, tc := range map[string]struct {
		exit   string
		wantRC int
	}{
		"exit 0":            {"exit 0", 0},
		"exit 7":            {"exit 7", 7},
		"killed by SIGTERM": {"kill -TERM $$", 143},
	} {
		t.Run(name, func(t *testing.T) {
			pd := stragglerDir(t)
			sb := newRankSandbox(t, rankStraggler(pd, tc.exit))
			out := sb.runRank(t)
			pid := stragglerPIDs(t, pd, 1)[0]

			if err := syscall.Kill(pid, 0); err != nil {
				t.Fatalf("the rank's output reached EOF only once the straggler (pid %d) was gone: the rank waited for it: %v\n%s", pid, err, out.text)
			}
			if out.rc != tc.wantRC {
				t.Errorf("the rank exited %d, want the command's own %d", out.rc, tc.wantRC)
			}
			if got := sb.drainPolls(t); got != sb.polls {
				t.Errorf("the drain polled %d times with a straggler holding the output, want exactly the bound (%d)", got, sb.polls)
			}
			last, note := strings.Index(out.text, rankLastLine), strings.Index(out.text, rankStragglerNote)
			if last < 0 || note < 0 || last > note {
				t.Errorf("want the command's last line, then the wrapper's straggler notice (last=%d notice=%d):\n%s", last, note, out.text)
			}
			if _, err := os.Stat("/proc/self/fd"); err == nil && note >= 0 {
				line := out.text[note:]
				line = line[:strings.IndexByte(line+"\n", '\n')]
				if !regexp.MustCompile(` ` + strconv.Itoa(pid) + ` [^ ]*.*sleep 30 `).MatchString(line) {
					t.Errorf("the straggler notice does not name pid %d (/bin/sleep 30): %s", pid, line)
				}
				if strings.Contains(line, " cat") {
					t.Errorf("the straggler notice lists the wrapper's own reader: %s", line)
				}
			}
		})
	}
}

// TestMPIRankWithoutStragglersAddsNoDelay: with nothing holding the output the
// reader ends at the command's exit and the rank ends at once, not after the
// drain bound.
func TestMPIRankWithoutStragglersAddsNoDelay(t *testing.T) {
	sb := newRankSandbox(t, "echo "+rankLastLine+" >&2\nexit 3")
	out := sb.runRank(t)
	if got := sb.drainPolls(t); got >= sb.polls/4 {
		t.Errorf("the drain polled %d times with no straggler (bound %d): the rank waited when nothing held its output", got, sb.polls)
	}
	if out.rc != 3 {
		t.Errorf("the rank exited %d, want the command's own 3", out.rc)
	}
	if !strings.Contains(out.text, rankLastLine) {
		t.Errorf("the command's stderr is missing from the rank's output:\n%s", out.text)
	}
	if strings.Contains(out.text, rankStragglerNote) {
		t.Errorf("a straggler notice with no straggler:\n%s", out.text)
	}
}

type rankOutput struct {
	text string
	rc   int
}

// runRank starts the per-rank program with stdout and stderr on a pipe and
// reads it to EOF, which is when Open MPI would consider the rank's output
// done. No deadline: a straggler outlives the test only by its 30 s.
func (sb *rankSandbox) runRank(t *testing.T) rankOutput {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	cmd := exec.Command(sb.program[0], sb.program[1:]...) //nolint:gosec // nosemgrep
	cmd.Env = sb.env()
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	text, _ := io.ReadAll(r)
	_ = cmd.Wait()
	return rankOutput{text: string(text), rc: cmd.ProcessState.ExitCode()}
}

// mpirunBinary finds a real Open MPI mpirun: on PATH, or where the AL2/AL2023
// openmpi package puts it. The test is skipped without one.
func mpirunBinary(t *testing.T) string {
	t.Helper()
	if p, err := exec.LookPath("mpirun"); err == nil {
		return p
	}
	if _, err := os.Stat("/usr/lib64/openmpi/bin/mpirun"); err == nil {
		return "/usr/lib64/openmpi/bin/mpirun"
	}
	t.Skip("no mpirun installed (brew install open-mpi, or the openmpi package): the real-mpirun half of #1479 needs one")
	return ""
}

// runMPI runs the rendered per-rank program under a real `mpirun -np 2` on
// this host. Output goes to a file: Open MPI's own forwarding, not a pipe the
// test holds, is what decides when mpirun returns.
func (sb *rankSandbox) runMPI(t *testing.T, mpirun string) (string, int) {
	t.Helper()
	f, err := os.Create(filepath.Join(sb.dir, "mpirun.out"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	args := []string{"--oversubscribe", "-np", "2"}
	if os.Geteuid() == 0 { // Open MPI before 4.0.3 ignores the env vars
		args = append(args, "--allow-run-as-root")
	}
	args = append(args, sb.program...)
	cmd := exec.Command(mpirun, args...) //nolint:gosec // nosemgrep
	cmd.Env = sb.env()
	cmd.Stdout, cmd.Stderr = f, f
	cmd.Stdin = nil
	_ = cmd.Run()
	return sb.read(t, "mpirun.out"), cmd.ProcessState.ExitCode()
}

// TestMPIRunEndsAtTheRanksExitWithARealMPIRun is #1479 against Open MPI
// itself. With each rank leaving a straggler on the forwarded output, mpirun
// must return while both stragglers are alive, after each rank's full drain,
// with both ranks' last lines. Against `bash /etc/spawn/mpi-command` mpirun
// returns only when the stragglers exit, which is the live 600 s delay. The
// exit-code cases have no straggler: Open MPI aborts the job on a failing
// rank, which kills its stragglers either way.
func TestMPIRunEndsAtTheRanksExitWithARealMPIRun(t *testing.T) {
	mpirun := mpirunBinary(t)
	t.Run("stragglers", func(t *testing.T) {
		pd := stragglerDir(t)
		sb := newRankSandbox(t, rankStraggler(pd, "exit 0"))
		out, rc := sb.runMPI(t, mpirun)
		for r, pid := range stragglerPIDs(t, pd, 2) {
			if err := syscall.Kill(pid, 0); err != nil {
				t.Errorf("mpirun returned only once rank %d's straggler (pid %d) was gone: it waited for it\n%s", r, pid, out)
			}
		}
		if rc != 0 {
			t.Errorf("mpirun exited %d, want 0\n%s", rc, out)
		}
		if got := sb.drainPolls(t); got != 2*sb.polls {
			t.Errorf("the drains polled %d times in all, want each of the 2 ranks' full bound (%d)", got, 2*sb.polls)
		}
		for r := 0; r < 2; r++ {
			if !strings.Contains(out, fmt.Sprintf("%s-%d", rankLastLine, r)) {
				t.Errorf("rank %d's last line is missing:\n%s", r, out)
			}
		}
		if n := strings.Count(out, rankStragglerNote); n != 2 {
			t.Errorf("want a straggler notice from each rank, got %d:\n%s", n, out)
		}
	})
	for name, tc := range map[string]struct {
		exit   string
		wantRC int
	}{
		"exit 0":            {"exit 0", 0},
		"exit 7":            {"exit 7", 7},
		"killed by SIGTERM": {"kill -TERM $$", 143},
	} {
		t.Run(name, func(t *testing.T) {
			sb := newRankSandbox(t, "echo "+rankLastLine+"-${OMPI_COMM_WORLD_RANK}\n"+tc.exit)
			out, rc := sb.runMPI(t, mpirun)
			if rc != tc.wantRC {
				t.Errorf("mpirun exited %d, want %d\n%s", rc, tc.wantRC, out)
			}
			if !strings.Contains(out, rankLastLine+"-0") {
				t.Errorf("rank 0's last line is missing:\n%s", out)
			}
			if got := sb.drainPolls(t); got >= sb.polls/4 {
				t.Errorf("the drains polled %d times with no straggler (bound %d)", got, sb.polls)
			}
		})
	}
}
