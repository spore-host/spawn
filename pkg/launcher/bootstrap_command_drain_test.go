//go:build unix

package launcher

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// These run the real --command block under bash in the spot sandbox, for
// #1459: the block used to run the command as `su ... 2>&1 | tee -a LOG` and
// read ${PIPESTATUS[0]}, and a pipeline ends only when tee sees EOF. A
// descendant still holding the command's stdout (an un-waited `cmd &`) kept
// SPAWN_EXITCODE, the completion record and so --on-complete waiting for as
// long as it lived. The owner's decision: complete on the command's own exit,
// drain the log for a bounded time, then stop waiting and say who was holding
// the output.
//
// The drain is counted, not timed: a stub `sleep` on PATH records each poll, so
// "bounded" means exactly commandDrainPolls polls and "no added delay" means
// far fewer. Only the drain sleeps through PATH; the straggler uses /bin/sleep.

const (
	lastLineMarker  = "LAST_LINE_BEFORE_EXIT_MARKER"
	stragglerNotice = "not waiting for processes still holding --command's output"
)

// drainSandbox is the spot sandbox plus the counting sleep and, optionally, a
// tee that writes each line 0.2 s late: a log writer that lags the command, so
// a block that records completion without draining is caught deterministically.
type drainSandbox struct {
	*spotSandbox
	script string
}

func newDrainSandbox(t *testing.T, command string, slowTee bool) *drainSandbox {
	t.Helper()
	sb := newSpotSandbox(t, sporedStub{code: 1}) // an original instance: the command runs
	writeExec(t, filepath.Join(sb.dir, "bin", "sleep"), fmt.Sprintf(
		"#!/bin/bash\necho \"$*\" >> %q\nexec /bin/sleep 0.02\n", filepath.Join(sb.dir, "sleep-calls")))
	if slowTee {
		writeExec(t, filepath.Join(sb.dir, "bin", "tee"),
			"#!/bin/bash\n[ \"$1\" = -a ] && shift\n"+
				"while IFS= read -r l || [ -n \"$l\" ]; do /bin/sleep 0.2; printf '%s\\n' \"$l\" >> \"$1\"; printf '%s\\n' \"$l\"; done\n")
	}
	if err := os.WriteFile(filepath.Join(sb.dir, "etc", "spawn", "command"), []byte(command+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script, err := BuildLinuxBootstrap(BootstrapConfig{Username: "tester", Command: command})
	if err != nil {
		t.Fatal(err)
	}
	return &drainSandbox{spotSandbox: sb, script: script}
}

// runBlock runs the --command block to the end of its backgrounded subshell
// (the trailing `wait`). Output goes to a file, not a pipe: the tee a straggler
// keeps alive holds its stdout, and a pipe would make the test wait for it too.
func (sb *drainSandbox) runBlock(t *testing.T) {
	t.Helper()
	block := between(t, sb.script, "if [ -s /etc/spawn/command ]; then\n    WORKFLOW_COMMAND=", "\necho \"Creating login banner...\"")
	out, err := os.Create(filepath.Join(sb.dir, "console"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = out.Close() }()
	cmd := exec.Command("bash", "-c", sb.rw.Replace("set -e\nLOCAL_USERNAME=tester\n"+ //nolint:gosec // nosemgrep
		completionAssignment(t, sb.script)+checkFunc(t, sb.script)+block+"\nwait\n"))
	cmd.Env = []string{"PATH=" + filepath.Join(sb.dir, "bin") + ":/usr/bin:/bin", "HOME=" + sb.dir}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		t.Fatalf("command block failed: %v\nconsole:\n%s", err, sb.read("console"))
	}
}

// drainPolls counts the drain loop's sleeps.
func (sb *drainSandbox) drainPolls() int {
	n := 0
	for _, l := range strings.Split(sb.read("sleep-calls"), "\n") {
		if l == commandDrainPoll {
			n++
		}
	}
	return n
}

// stragglerCommand is a command that leaves a descendant holding its stdout,
// records the descendant's PID, writes its last line and exits with exit.
func stragglerCommand(pidFile, exit string) string {
	return fmt.Sprintf("/bin/sleep 30 &\necho $! > %q\necho %s\n%s", pidFile, lastLineMarker, exit)
}

func stragglerPID(t *testing.T, pidFile string) int {
	t.Helper()
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("the straggler did not record its PID: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("straggler PID %q: %v", b, err)
	}
	// Killing it ends the tee too (EOF), so the test leaves nothing running.
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	return pid
}

// TestCommandCompletesWhileAStragglerHoldsItsOutput is the #1459 gate. With a
// descendant holding the command's stdout, the block must record completion
// after exactly commandDrainPolls polls with the straggler still alive, keep
// the command's exit code (a plain non-zero and a signal), keep the line the
// command wrote just before exiting ahead of what the block appends after the
// drain, and name the straggler.
func TestCommandCompletesWhileAStragglerHoldsItsOutput(t *testing.T) {
	for name, tc := range map[string]struct {
		exit   string
		wantRC string
	}{
		"exit 7":            {"exit 7", "7"},
		"killed by SIGTERM": {"kill -TERM $$", "143"},
	} {
		t.Run(name, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "straggler.pid")
			sb := newDrainSandbox(t, stragglerCommand(pidFile, tc.exit), true)
			sb.runBlock(t)
			pid := stragglerPID(t, pidFile)

			if err := syscall.Kill(pid, 0); err != nil {
				t.Fatalf("the straggler (pid %d) is gone, so the block waited for it before recording completion: %v", pid, err)
			}
			if got := sb.drainPolls(); got != commandDrainPolls {
				t.Errorf("the drain polled %d times with a straggler holding the output, want exactly commandDrainPolls (%d)", got, commandDrainPolls)
			}
			if got := strings.TrimSpace(sb.read("tmp/SPAWN_EXITCODE")); got != tc.wantRC {
				t.Errorf("SPAWN_EXITCODE = %q, want the command's own %s", got, tc.wantRC)
			}
			if rec := sb.read("tmp/SPAWN_COMPLETE"); !strings.Contains(rec, `"exit_code": `+tc.wantRC) || !strings.Contains(rec, `"failed"`) {
				t.Errorf("completion record = %q, want status failed and exit_code %s", rec, tc.wantRC)
			}

			log := sb.read("var/log/spawn-command.log")
			last := strings.Index(log, lastLineMarker)
			notice := strings.Index(log, "spawn: --command exited "+tc.wantRC)
			if last < 0 || notice < 0 || last > notice {
				t.Errorf("the command's last line must be in the log ahead of the errexit notice appended after the drain (last=%d notice=%d):\n%s", last, notice, log)
			}
			if !strings.Contains(log, stragglerNotice) {
				t.Errorf("the log does not say the block stopped waiting for a straggler:\n%s", log)
			}
			// The PIDs come from /proc, so only where there is one. The block's
			// own tee holds the FIFO too, and is not a straggler.
			if _, err := os.Stat("/proc/self/fd"); err == nil && strings.Contains(log, stragglerNotice) {
				line := log[strings.Index(log, stragglerNotice):]
				line = line[:strings.Index(line, "\n")]
				// "<pid> <args>": args is /bin/sleep 30, or on AL2023 coreutils'
				// multi-call form ending in it.
				if !regexp.MustCompile(` ` + strconv.Itoa(pid) + ` [^ ]*.*sleep 30 `).MatchString(line) {
					t.Errorf("the straggler notice does not name pid %d (/bin/sleep 30): %s", pid, line)
				}
				if strings.Contains(line, "/bin/tee") {
					t.Errorf("the straggler notice lists the block's own tee: %s", line)
				}
			}
		})
	}
}

// TestCommandWithoutStragglersAddsNoDelay: with nothing holding the output,
// tee ends at the command's exit and the block records completion at once,
// not after the drain bound. A handful of polls covers tee's own exit; a
// quarter of the bound is far beyond it.
func TestCommandWithoutStragglersAddsNoDelay(t *testing.T) {
	sb := newDrainSandbox(t, "echo "+lastLineMarker, false)
	sb.runBlock(t)
	if got := sb.drainPolls(); got >= commandDrainPolls/4 {
		t.Errorf("the drain polled %d times with no straggler (bound %d): completion waited when nothing held the output", got, commandDrainPolls)
	}
	if got := strings.TrimSpace(sb.read("tmp/SPAWN_EXITCODE")); got != "0" {
		t.Errorf("SPAWN_EXITCODE = %q, want 0", got)
	}
	log := sb.read("var/log/spawn-command.log")
	if !strings.Contains(log, lastLineMarker) {
		t.Errorf("the command's output is missing from the log:\n%s", log)
	}
	if strings.Contains(log, stragglerNotice) {
		t.Errorf("a straggler notice with no straggler:\n%s", log)
	}
}
