package userdata

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/security"
)

// These are the #1170 gates. spored watches the launch's --completion-file
// (LaunchConfig.CompletionFile, carried by the spawn:completion-file tag), and
// the --mpi-command script wrote every completion record to a hardcoded
// /tmp/SPAWN_COMPLETE. With a custom path no member fired --on-complete and the
// cohort billed until its TTL. Same bug as #1131, in the MPI generator.

// customCompletion holds a space, a single quote and a $, so any write that is
// not quoted for its shell either splits, breaks the quoting, or expands.
const customCompletion = "/tmp/spawn out/it's $HOME/done"

// localRecordWriteRe discovers every local write of a completion record: an
// echo of a record (a literal one with "source": "mpi", or $rec in either
// quoting) redirected to a file. Anchored on the echo-and-redirect, so a comment
// naming /tmp/SPAWN_COMPLETE cannot satisfy or trip it.
var localRecordWriteRe = regexp.MustCompile(`(?m)\becho ('\{[^']*"source": "mpi"[^']*\}'|"\$rec"|'\$rec') > (.+)$`)

// peerRecordSendRe discovers every hand-off of the record to a peer over ssh:
// an ssh invocation on a (continuation-joined) line that also carries $rec,
// whether the record goes in the remote command or on ssh's stdin.
var peerRecordSendRe = regexp.MustCompile(`(?m)^.*(\bssh -.*\$rec|\$rec.*\| ssh -).*$`)

func renderMPI(t *testing.T, cfg MPIConfig) string {
	t.Helper()
	s, err := GenerateMPIUserData(cfg)
	if err != nil {
		t.Fatalf("GenerateMPIUserData: %v", err)
	}
	return s
}

type recordWrites struct {
	local []string // redirection targets, as written in the script
	peer  []string // the ssh lines, continuation-joined
}

// discoverRecordWrites fails when it finds nothing: a script with no completion
// write has broken, not proven itself clean.
func discoverRecordWrites(t *testing.T, script string) recordWrites {
	t.Helper()
	var w recordWrites
	for _, m := range localRecordWriteRe.FindAllStringSubmatch(script, -1) {
		w.local = append(w.local, strings.TrimSpace(m[2]))
	}
	joined := strings.ReplaceAll(script, "\\\n", " ")
	w.peer = peerRecordSendRe.FindAllString(joined, -1)
	if len(w.local) == 0 {
		t.Fatal("found no local completion-record write in the MPI script")
	}
	return w
}

func mpiCfg(cmd, completion string) MPIConfig {
	return MPIConfig{
		Region: "us-east-1", JobArrayID: "arr-1", JobArrayIndex: 0, JobArraySize: 3,
		MPICommand: cmd, CompletionFile: completion,
	}
}

// TestMPIWritesTheConfiguredCompletionFile: every local write targets exactly
// the quoted configured path, and the default appears nowhere in the script.
func TestMPIWritesTheConfiguredCompletionFile(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cmd        string
		wantLocal  int
		wantRemote int
	}{
		// Peers-file timeout + spawn_mpi_done's own record; plus the peer send.
		{"with --mpi-command", "hostname", 2, 1},
		// Peers-file timeout + the bare readiness-failure record; no peer send.
		{"without --mpi-command", "", 2, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := renderMPI(t, mpiCfg(tc.cmd, customCompletion))
			w := discoverRecordWrites(t, script)
			if len(w.local) != tc.wantLocal {
				t.Errorf("found %d local completion writes, want %d: %q", len(w.local), tc.wantLocal, w.local)
			}
			if len(w.peer) != tc.wantRemote {
				t.Errorf("found %d ssh hand-offs of the record, want %d: %q", len(w.peer), tc.wantRemote, w.peer)
			}
			for _, got := range w.local {
				if got != security.ShellQuote(customCompletion) {
					t.Errorf("completion record written to %s, want exactly %s (the configured --completion-file)", got, security.ShellQuote(customCompletion))
				}
			}
			if strings.Contains(script, defaultCompletionFile) {
				t.Errorf("the script still names the default %s despite --completion-file %q", defaultCompletionFile, customCompletion)
			}
		})
	}
}

// The default stays what it was.
func TestMPICompletionFileDefault(t *testing.T) {
	for _, cmd := range []string{"hostname", ""} {
		for _, got := range discoverRecordWrites(t, renderMPI(t, mpiCfg(cmd, ""))).local {
			if got != security.ShellQuote(defaultCompletionFile) {
				t.Errorf("default completion write (cmd=%q) targets %s, want '/tmp/SPAWN_COMPLETE'", cmd, got)
			}
		}
	}
}

// TestMPICompletionWritesAreQuoted runs each discovered write under bash with a
// path that holds a space, a quote and a $, in a directory that does not exist
// yet. The local writes run as rendered. The peer hand-off is run with a stub
// ssh that captures the remote command exactly as ssh would send it, which is
// then run the way a peer's shell would (bash -c, record on stdin): the record
// must land at exactly the configured path on both sides.
func TestMPICompletionWritesAreQuoted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Linux-only: runs the Linux MPI user-data under bash")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "new dir", "it's $HOME done")
	script := renderMPI(t, mpiCfg("hostname", path))

	syntax := exec.Command(bash, "-n")
	syntax.Stdin = strings.NewReader(script)
	if out, err := syntax.CombinedOutput(); err != nil {
		t.Fatalf("MPI script with a quoted --completion-file is not valid bash: %v\n%s", err, out)
	}

	landed := func(what string) {
		t.Helper()
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: record did not land at the configured path %q: %v", what, path, err)
		}
		if !strings.Contains(string(got), `"source": "mpi"`) {
			t.Errorf("%s: unexpected record at %q: %q", what, path, got)
		}
		_ = os.RemoveAll(filepath.Join(dir, "new dir"))
	}

	w := discoverRecordWrites(t, script)
	for _, target := range w.local {
		frag := "set -e\nmkdir -p \"$(dirname -- " + target + ")\"\necho '{\"source\": \"mpi\"}' > " + target + "\n"
		if out, err := exec.Command(bash, "-c", frag).CombinedOutput(); err != nil {
			t.Fatalf("running the rendered local write failed: %v\n%s", err, out)
		}
		landed("local write " + target)
	}

	if len(w.peer) == 0 {
		t.Fatal("found no ssh hand-off of the record with --mpi-command")
	}
	for _, line := range w.peer {
		captured := filepath.Join(dir, "remote-cmd")
		frag := "set -e\n" +
			"ssh() { cat > \"$CAPTURED.stdin\"; printf '%s' \"${@: -1}\" > \"$CAPTURED\"; }\n" +
			"rec='{\"source\": \"mpi\"}'; ip=10.0.0.9\n" +
			line + "\nwait\n"
		c := exec.Command(bash, "-c", frag)
		c.Env = append(os.Environ(), "CAPTURED="+captured)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("running the rendered ssh hand-off failed: %v\n%s", err, out)
		}
		remote, err := os.ReadFile(captured)
		if err != nil {
			t.Fatalf("the ssh stub captured no remote command: %v", err)
		}
		if strings.Contains(string(remote), defaultCompletionFile) {
			t.Errorf("the peer write targets the default: %s", remote)
		}
		peer := exec.Command(bash, "-c", string(remote))
		stdin, err := os.Open(captured + ".stdin")
		if err != nil {
			t.Fatal(err)
		}
		peer.Stdin = stdin
		out, err := peer.CombinedOutput()
		_ = stdin.Close()
		if err != nil {
			t.Fatalf("the peer's shell could not run the remote write %q: %v\n%s", remote, err, out)
		}
		landed("peer write")
	}
}

// TestMPICommandSignalsCompletionAtCustomPath executes rank 0's block (see
// runRank0) with a custom --completion-file: rank 0 and every peer must get the
// record at THAT path, and none at the default.
func TestMPICommandSignalsCompletionAtCustomPath(t *testing.T) {
	r := runRank0With(t, 0, "", customCompletion)
	rel := strings.TrimPrefix(customCompletion, "/tmp/")

	rec, ok := r.record(t, "tmp/"+rel)
	if !ok {
		t.Fatalf("rank 0 has no record at --completion-file %q, so its --on-complete never fires", customCompletion)
	}
	if rec.Status != "completed" {
		t.Errorf("rank 0 record = %+v, want status=completed", rec)
	}
	if _, ok := r.read(t, "tmp/SPAWN_COMPLETE"); ok {
		t.Error("rank 0 still wrote the default /tmp/SPAWN_COMPLETE")
	}
	for _, ip := range []string{peerA, peerB} {
		if _, ok := r.record(t, "hosts/"+ip+"/"+rel); !ok {
			t.Errorf("peer %s has no record at --completion-file %q, so it rides the job to TTL", ip, customCompletion)
		}
		if _, ok := r.read(t, "hosts/"+ip+"/SPAWN_COMPLETE"); ok {
			t.Errorf("peer %s was signalled at the default /tmp/SPAWN_COMPLETE", ip)
		}
	}
}
