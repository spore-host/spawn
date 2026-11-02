package launcher

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/spore-host/spawn/internal/shellmin"
	"github.com/spore-host/spawn/pkg/userdata"
)

// These are the #1484 gates. Once spored is up the bootstrap's EXIT guard
// (fatalRetireFunc) no longer retires the instance, which is right: spored
// enforces its TTL. But the body runs on under set -e (user, key, sudoers,
// session timeout, array, hostname and sweep setup, the MOTD) before it
// launches --command or hands over to the user data after it, and a failure
// there exited with no completion record, so --on-complete never fired and the
// instance billed to its TTL or idle timeout. Owner principle: a failure must
// never leave an instance billing silently.
//
// Each case runs the whole inline bootstrap in the boot sandbox (fullBoot) with
// one step forced to fail.

const bootFailedNote = "after spored started; recording it (#1484)"

type completionRecord struct {
	Status   string `json:"status"`
	ExitCode *int   `json:"exit_code"`
	Source   string `json:"source"`
}

func readRecord(t *testing.T, sb *bootSandbox) (completionRecord, bool) {
	t.Helper()
	b := sb.read("tmp/SPAWN_COMPLETE")
	if b == "" {
		return completionRecord{}, false
	}
	var r completionRecord
	if err := json.Unmarshal([]byte(b), &r); err != nil {
		t.Fatalf("SPAWN_COMPLETE is not a completion record: %v: %q", err, b)
	}
	return r, true
}

func exitStatus(err error) int {
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return -1
}

// failWhen is a stand-in for tool that exits rc when its arguments contain
// match and otherwise runs the real tool. before runs first in either case.
func failWhen(t *testing.T, tool, match string, rc int, before string) string {
	t.Helper()
	real, err := exec.LookPath(tool)
	if err != nil {
		t.Skipf("%s not available", tool)
	}
	return fmt.Sprintf("#!/bin/bash\ncase \" $* \" in *%s*) %s\n  echo \"%s: forced failure\" >&2; exit %d ;; esac\nexec %s \"$@\"\n",
		match, before, tool, rc, real)
}

func exitWith(rc int) string { return fmt.Sprintf("#!/bin/bash\nexit %d\n", rc) }

// TestBootstrapFailureAfterSporedWritesAFailedRecord: a step failing after
// spored has started and before the body hands off writes
// {"status": "failed", "exit_code": <its status>, "source": "bootstrap"}, so
// spored acts on --on-complete, and does not retire the instance (spored is
// there to enforce the TTL). With --command the command never starts; without
// one the same record is written (the decided behaviour: the user data or
// workload that would follow never runs, so --on-complete, when set, acts).
func TestBootstrapFailureAfterSporedWritesAFailedRecord(t *testing.T) {
	// The MOTD is written after --command launches, so it is in the window only
	// without one.
	motd := func(t *testing.T) map[string]string {
		return map[string]string{"chmod": failWhen(t, "chmod", "etc/motd", 7, "")}
	}
	cases := []struct {
		name     string
		command  string
		standIns func(t *testing.T) map[string]string
		rc       int
	}{
		{"user creation", "echo WORKLOAD_RAN", func(*testing.T) map[string]string { return map[string]string{"useradd": exitWith(9)} }, 9},
		{"ssh key ownership", "echo WORKLOAD_RAN", func(*testing.T) map[string]string { return map[string]string{"chown": exitWith(5)} }, 5},
		{"command script, just before the launch", "echo WORKLOAD_RAN", func(t *testing.T) map[string]string {
			return map[string]string{"chmod": failWhen(t, "chmod", "+x*spawn-command.sh", 6, "")}
		}, 6},
		{"no --command: user creation", "", func(*testing.T) map[string]string { return map[string]string{"useradd": exitWith(9)} }, 9},
		{"no --command: the MOTD, the body's last steps", "", motd, 7},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sb, out, err := fullBoot(t, BootstrapConfig{Username: "tester", Command: c.command,
				CustomUserData: "echo post > /tmp/POST_RAN\n"}, "", bootOpts{standIns: c.standIns(t)})
			if !sporedStarted(sb) {
				t.Fatalf("spored never started, so this is not the #1484 window\n%s", out)
			}
			if got := exitStatus(err); got != c.rc {
				t.Errorf("the bootstrap exited %d, want the failed step's %d\n%s", got, c.rc, out)
			}
			rec, ok := readRecord(t, sb)
			if !ok {
				t.Fatalf("no SPAWN_COMPLETE after the bootstrap failed with spored up, so --on-complete never fires and the instance bills to its TTL (#1484)\n%s", out)
			}
			if rec.Status != "failed" || rec.ExitCode == nil || *rec.ExitCode != c.rc || rec.Source != "bootstrap" {
				t.Errorf("record = %+v, want status=failed exit_code=%d source=bootstrap", rec, c.rc)
			}
			if !strings.Contains(sb.read("var/log/spawn-command.log"), fmt.Sprintf("bootstrap failed (status %d, at line ", c.rc)) {
				t.Errorf("the failure and its step are not in /var/log/spawn-command.log: %q", sb.read("var/log/spawn-command.log"))
			}
			if retired(sb) {
				t.Errorf("the instance was terminated by the bootstrap although spored is running to act on the record")
			}
			if strings.Contains(sb.read("var/log/spawn-command.log"), "WORKLOAD_RAN") || fileExists(sb.dir+"/tmp/POST_RAN") {
				t.Errorf("the workload or user data ran after the bootstrap failed")
			}
		})
	}
}

// TestBootstrapFailureAfterTheCommandLaunchedLeavesItsRecord: once the
// --command subshell is launched it owns the outcome. A later failure in the
// body (the MOTD) must not write a bootstrap record over the command's. The
// failing step waits for the command's record first, so a guard that wrote
// anyway would overwrite it here, deterministically.
func TestBootstrapFailureAfterTheCommandLaunchedLeavesItsRecord(t *testing.T) {
	wait := `i=0; while [ ! -s "$HOME/tmp/SPAWN_COMPLETE" ] && [ $i -lt 400 ]; do /bin/sleep 0.05; i=$((i + 1)); done;`
	sb, out, err := fullBoot(t, BootstrapConfig{Username: "tester", Command: "echo WORKLOAD_RAN; exit 3"}, "",
		bootOpts{standIns: map[string]string{"chmod": failWhen(t, "chmod", "etc/motd", 7, wait)}})
	if got := exitStatus(err); got != 7 {
		t.Fatalf("the bootstrap exited %d, want the MOTD step's 7\n%s", got, out)
	}
	rec, ok := readRecord(t, sb)
	if !ok || rec.Source != "command" || rec.ExitCode == nil || *rec.ExitCode != 3 {
		t.Errorf("record = %+v (exists=%v), want the command's own: source=command exit_code=3", rec, ok)
	}
	if strings.Contains(out, bootFailedNote) {
		t.Errorf("the guard recorded a bootstrap failure after --command had launched\n%s", out)
	}
}

// TestBootstrapHandsOffAtTheCommandAndAtItsEnd: the guard's window ends at
// SPAWN_HANDED_OFF=1, set right after the --command subshell is launched and as
// the body's last statement before the Spot-replacement stop, and nowhere else.
func TestBootstrapHandsOffAtTheCommandAndAtItsEnd(t *testing.T) {
	body := BootstrapBody()
	if n := strings.Count(body, "SPAWN_HANDED_OFF=1"); n != 2 {
		t.Fatalf("SPAWN_HANDED_OFF=1 appears %d times in the body, want 2", n)
	}
	if !strings.Contains(body, "    ) &\n    SPAWN_HANDED_OFF=1\n") {
		t.Error("the body does not hand off immediately after launching the --command subshell")
	}
	if !strings.HasSuffix(strings.TrimRight(shellmin.Strip(linuxBootstrapBody), "\n"), "\nSPAWN_HANDED_OFF=1") {
		t.Error("the body's last statement is not SPAWN_HANDED_OFF=1")
	}
}

// TestNoRecordAfterTheBodyHandsOff: what runs after the body owns its outcome.
// A healthy boot writes nothing; custom user data whose last command fails
// (it runs under set +e, so the script's status is that command's) writes
// nothing; and the MPI user data, whose EXIT trap (#1483) writes its own
// record and then calls this guard, gets exactly that one record.
func TestNoRecordAfterTheBodyHandsOff(t *testing.T) {
	t.Run("healthy boot", func(t *testing.T) {
		sb, out, err := fullBoot(t, BootstrapConfig{Username: "tester"}, "")
		if err != nil {
			t.Fatalf("the bootstrap failed: %v\n%s", err, out)
		}
		if rec, ok := readRecord(t, sb); ok {
			t.Errorf("a healthy boot with no workload wrote %+v", rec)
		}
	})
	t.Run("custom user data ending in a failure", func(t *testing.T) {
		sb, out, err := fullBoot(t, BootstrapConfig{Username: "tester", CustomUserData: "echo post > /tmp/POST_RAN\nfalse\n"}, "")
		if exitStatus(err) != 1 || !fileExists(sb.dir+"/tmp/POST_RAN") {
			t.Fatalf("want the user data to run and the script to exit 1: err=%v\n%s", err, out)
		}
		if rec, ok := readRecord(t, sb); ok || strings.Contains(out, bootFailedNote) {
			t.Errorf("the guard recorded the user data's own failure as the bootstrap's: %+v\n%s", rec, out)
		}
	})
	mpi, err := userdata.GenerateMPIUserData(userdata.MPIConfig{Region: "eu-west-1", JobArrayID: "arr-1",
		JobArrayIndex: 1, JobArraySize: 2, ReadyGate: MPIReadyGate})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("MPI setup fails: one mpi-bootstrap record", func(t *testing.T) {
		// mpirun is absent and the sandbox's yum exits 137: the MPI install fails.
		sb, out, err := fullBoot(t, BootstrapConfig{Username: "tester"}, "", bootOpts{appended: "\n" + mpi})
		if got := exitStatus(err); got != 137 {
			t.Fatalf("the member script exited %d, want the MPI install's 137\n%s", got, out)
		}
		rec, ok := readRecord(t, sb)
		if !ok || rec.Source != "mpi-bootstrap" || rec.Status != "failed" || rec.ExitCode == nil || *rec.ExitCode != 137 {
			t.Errorf("record = %+v (exists=%v), want the MPI trap's: failed, 137, mpi-bootstrap", rec, ok)
		}
		if strings.Contains(out, bootFailedNote) {
			t.Errorf("the bootstrap guard wrote a second record after the MPI trap's\n%s", out)
		}
		if retired(sb) {
			t.Error("the instance was terminated by the bootstrap")
		}
	})
	t.Run("bootstrap fails before the MPI script: one bootstrap record", func(t *testing.T) {
		sb, out, err := fullBoot(t, BootstrapConfig{Username: "tester"}, "",
			bootOpts{appended: "\n" + mpi, standIns: map[string]string{"useradd": exitWith(9)}})
		if got := exitStatus(err); got != 9 {
			t.Fatalf("the member script exited %d, want 9\n%s", got, out)
		}
		rec, ok := readRecord(t, sb)
		if !ok || rec.Source != "bootstrap" || rec.ExitCode == nil || *rec.ExitCode != 9 {
			t.Errorf("record = %+v (exists=%v), want the bootstrap's: failed, 9, bootstrap", rec, ok)
		}
		if strings.Contains(out, "MPI setup failed") || strings.Count(out, bootFailedNote) != 1 {
			t.Errorf("want exactly one record, the bootstrap's, and no MPI step run\n%s", out)
		}
		if retired(sb) {
			t.Error("the instance was terminated by the bootstrap")
		}
	})
}
