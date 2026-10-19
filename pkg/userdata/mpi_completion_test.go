package userdata

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// These tests are spawn#894. The documented intent is the issue's "Expected"
// and the --command contract in cmd/launch_flags.go: --mpi-command behaves like
// --command on rank 0's node. Its output goes to /var/log/spawn-command.log,
// its exit code goes to /tmp/SPAWN_EXITCODE, and --on-complete fires on EVERY
// member once rank 0 finishes. spored fires --on-complete when
// /tmp/SPAWN_COMPLETE appears (pkg/agent checkCompletion).
//
// They EXECUTE rank 0's part of the rendered script under real bash, with
// stand-ins for mpirun, ssh and hostname on PATH. A text assertion would pass
// on a script that wrote the marker in a branch that never runs. This one
// catches the #566 failure as well: the bootstrap runs under 'set -e', so a
// failing command can abort the script before its exit code is recorded.

const (
	rank0IP = "10.0.0.1"
	peerA   = "10.0.0.2"
	peerB   = "10.0.0.3"
)

type mpiRun struct {
	root string
}

func (r mpiRun) read(t *testing.T, rel string) (string, bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.root, rel))
	if os.IsNotExist(err) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b), true
}

type completionRecord struct {
	Status   string `json:"status"`
	ExitCode *int   `json:"exit_code"`
	Source   string `json:"source"`
}

func (r mpiRun) record(t *testing.T, rel string) (completionRecord, bool) {
	t.Helper()
	s, ok := r.read(t, rel)
	if !ok {
		return completionRecord{}, false
	}
	var rec completionRecord
	if err := json.Unmarshal([]byte(s), &rec); err != nil {
		t.Fatalf("%s is not a JSON completion record: %v\n%s", rel, err, s)
	}
	return rec, true
}

// runRank0 renders rank 0's script, takes the rank-0 block (the last
// `if [ "0" -eq 0 ]`, which holds the readiness wait and the mpirun), and runs
// it under `set -e` the way the bootstrap does. Absolute paths are moved
// under a temp root. The ssh stand-in runs the remote command with that host's
// /tmp redirected to hosts/<ip>/, so each peer's files are separate.
func runRank0(t *testing.T, mpiRC int, unreachable string) mpiRun {
	t.Helper()
	return runRank0With(t, mpiRC, unreachable, "")
}

// runRank0With is runRank0 with a --completion-file (#1170). A path under /tmp/
// is relocated by the same rewrite as the default, so rank 0's record lands
// under <root>/tmp/ and each peer's under hosts/<ip>/.
func runRank0With(t *testing.T, mpiRC int, unreachable, completionFile string) mpiRun {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Linux-only: executes the Linux MPI user-data block (/tmp, /var/log) under bash (#1002)")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	script, err := GenerateMPIUserData(MPIConfig{
		Region: "us-east-1", JobArrayID: "arr-1",
		JobArrayIndex: 0, JobArraySize: 3, MPICommand: "hostname",
		CompletionFile: completionFile,
	})
	if err != nil {
		t.Fatalf("GenerateMPIUserData: %v", err)
	}
	i := strings.LastIndex(script, `if [ "0" -eq 0 ]; then`)
	if i < 0 {
		t.Fatalf("no rank-0 block in:\n%s", relevant(script))
	}
	block := script[i:]

	root := t.TempDir()
	for _, d := range []string{"tmp", "var/log", "etc/spawn", "bin", "hosts"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{"/tmp/", "/var/log/", "/etc/spawn/"} {
		block = strings.ReplaceAll(block, p, root+p)
	}
	hostfile := rank0IP + " slots=1\n" + peerA + " slots=1\n" + peerB + " slots=1\n"
	if err := os.WriteFile(filepath.Join(root, "tmp/mpi-hostfile"), []byte(hostfile), 0o644); err != nil {
		t.Fatal(err)
	}

	stubs := map[string]string{
		"mpirun": "#!/bin/bash\necho rank-output-from-mpirun\necho rank-stderr >&2\nexit " + strconv.Itoa(mpiRC) + "\n",
		// Rank 0's own private address is among the hostfile entries.
		"hostname": "#!/bin/bash\necho '" + rank0IP + " 172.17.0.1'\n",
		"sleep":    "#!/bin/bash\nexit 0\n",
		"ssh": `#!/bin/bash
host=""
while [ $# -gt 0 ]; do
  case "$1" in
    -n) shift ;;
    -o) shift 2 ;;
    *) host="$1"; shift; break ;;
  esac
done
echo "$host $*" >> "$STUB_ROOT/ssh.log"
[ "$host" = "$STUB_UNREACHABLE" ] && exit 255
[ "$*" = "true" ] && exit 0
mkdir -p "$STUB_ROOT/hosts/$host"
cmd="$*"
exec bash -c "${cmd//$STUB_ROOT\/tmp\//$STUB_ROOT/hosts/$host/}"
`,
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(root, "bin", name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	rank0 := filepath.Join(root, "rank0.sh")
	if err := os.WriteFile(rank0, []byte("set -e\nSLOTS=1\n"+block), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", rank0)
	cmd.Env = append(os.Environ(),
		"PATH="+filepath.Join(root, "bin")+":"+os.Getenv("PATH"),
		"STUB_ROOT="+root,
		"STUB_UNREACHABLE="+unreachable,
	)
	out, _ := cmd.CombinedOutput()
	t.Logf("rank-0 block output:\n%s", out)
	return mpiRun{root: root}
}

func TestMPICommandSignalsCompletion(t *testing.T) {
	for _, tc := range []struct {
		name   string
		rc     int
		status string
	}{
		{"success", 0, "completed"},
		// A failed job is still a finished job. Under the bootstrap's set -e a
		// failing mpirun must not abort the script before the record is written.
		{"failure", 3, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runRank0(t, tc.rc, "")

			// Output where 'spawn array logs' reads it, stderr included.
			log, ok := r.read(t, "var/log/spawn-command.log")
			if !ok || !strings.Contains(log, "rank-output-from-mpirun") || !strings.Contains(log, "rank-stderr") {
				t.Errorf("mpirun's output did not reach /var/log/spawn-command.log (exists=%v):\n%s", ok, log)
			}

			// Exit code, as --command records it.
			code, ok := r.read(t, "tmp/SPAWN_EXITCODE")
			if !ok || strings.TrimSpace(code) != strconv.Itoa(tc.rc) {
				t.Errorf("/tmp/SPAWN_EXITCODE = %q (exists=%v), want %d", code, ok, tc.rc)
			}

			// Rank 0's own spored must see completion.
			rec, ok := r.record(t, "tmp/SPAWN_COMPLETE")
			if !ok {
				t.Fatal("rank 0 has no /tmp/SPAWN_COMPLETE, so its --on-complete never fires")
			}
			if rec.Status != tc.status || rec.ExitCode == nil || *rec.ExitCode != tc.rc {
				t.Errorf("rank 0 record = %+v, want status=%s exit_code=%d", rec, tc.status, tc.rc)
			}

			// And so must every other member's.
			for _, ip := range []string{peerA, peerB} {
				prec, ok := r.record(t, "hosts/"+ip+"/SPAWN_COMPLETE")
				if !ok {
					t.Errorf("peer %s was never signalled, so it rides the job to idle or TTL", ip)
					continue
				}
				if prec.Status != rec.Status || prec.ExitCode == nil || *prec.ExitCode != *rec.ExitCode {
					t.Errorf("peer %s record = %+v, want rank 0's %+v", ip, prec, rec)
				}
			}
		})
	}
}

// When rank 0 gives up on a peer that never accepts SSH, mpirun never starts.
// The reachable members must still stop, not ride to TTL.
func TestMPIPeerReadinessFailureSignalsReachablePeers(t *testing.T) {
	r := runRank0(t, 0, peerB)

	if log, _ := r.read(t, "var/log/spawn-command.log"); strings.Contains(log, "rank-output-from-mpirun") {
		t.Fatal("mpirun ran although a peer never accepted SSH")
	}
	rec, ok := r.record(t, "tmp/SPAWN_COMPLETE")
	if !ok || rec.Status != "failed" {
		t.Fatalf("rank 0 record = %+v (exists=%v), want status=failed", rec, ok)
	}
	if code, _ := r.read(t, "tmp/SPAWN_EXITCODE"); strings.TrimSpace(code) != "1" {
		t.Errorf("/tmp/SPAWN_EXITCODE = %q, want 1", code)
	}
	prec, ok := r.record(t, "hosts/"+peerA+"/SPAWN_COMPLETE")
	if !ok || prec.Status != "failed" {
		t.Errorf("reachable peer %s record = %+v (exists=%v), want status=failed", peerA, prec, ok)
	}
	if log, _ := r.read(t, "var/log/spawn-command.log"); !strings.Contains(log, "never accepted SSH") {
		t.Errorf("the readiness failure is not reported in /var/log/spawn-command.log:\n%s", log)
	}
}
