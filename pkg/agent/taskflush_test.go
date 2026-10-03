package agent

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/taskproto"
)

// installFakeHook points the hook path at a script that records the argv it was
// called with, and returns the path of that record.
func installFakeHook(t *testing.T, body string) (callLog string) {
	t.Helper()
	dir := t.TempDir()
	callLog = filepath.Join(dir, "calls.txt")
	hook := filepath.Join(dir, "task-flush.sh")
	script := "#!/bin/bash\nprintf '%s\\n' \"$*\" >> " + callLog + "\n" + body
	if err := os.WriteFile(hook, []byte(script), 0o700); err != nil { //nolint:gosec // test fixture, needs +x
		t.Fatalf("write fake hook: %v", err)
	}
	t.Cleanup(taskproto.SetFlushScriptPathForTest(hook))
	return callLog
}

func hookCalls(t *testing.T, callLog string) []string {
	t.Helper()
	b, err := os.ReadFile(callLog)
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// TestFlushTaskRecordPassesTheReasonToken is the core of the agent half: the
// hook's retry-class mapping is driven entirely by this argument.
func TestFlushTaskRecordPassesTheReasonToken(t *testing.T) {
	callLog := installFakeHook(t, "exit 0\n")
	a := &Agent{}

	a.flushTaskRecord(taskproto.ExitTTLExpired)

	calls := hookCalls(t, callLog)
	if len(calls) != 1 {
		t.Fatalf("expected exactly one hook invocation, got %d: %v", len(calls), calls)
	}
	if calls[0] != string(taskproto.ExitTTLExpired) {
		t.Errorf("hook argv = %q, want %q", calls[0], taskproto.ExitTTLExpired)
	}
}

// TestFlushTaskRecordSkipsTheCompletedPath. A normally-finished task already has
// its authoritative record, written by the wrapper. Invoking the hook here would
// be a pointless root exec on the hot path, and it is the one case where getting
// the interlock wrong would corrupt a good record — so it is refused twice, here
// and in the hook itself.
func TestFlushTaskRecordSkipsTheCompletedPath(t *testing.T) {
	callLog := installFakeHook(t, "exit 0\n")
	a := &Agent{}

	a.flushTaskRecord(taskproto.ExitCompleted)

	if calls := hookCalls(t, callLog); len(calls) != 0 {
		t.Errorf("the hook must not run on the completed path, got: %v", calls)
	}
}

// TestFlushTaskRecordNoOpsWithoutAHook: a plain `spawn launch` installs no hook
// (there is no task identity or results prefix to flush to), and that must be
// silent rather than an error on every shutdown.
func TestFlushTaskRecordNoOpsWithoutAHook(t *testing.T) {
	t.Cleanup(taskproto.SetFlushScriptPathForTest(
		filepath.Join(t.TempDir(), "definitely-absent.sh")))

	a := &Agent{}
	a.flushTaskRecord(taskproto.ExitTTLExpired) // must not panic

	if a.taskFlushDone {
		t.Error("a missing hook must not consume the once-guard — it never ran")
	}
}

// TestFlushTaskRecordRunsAtMostOnce. runPreStop is reachable from more than one
// path in a single shutdown (a spot interruption that then terminates), and a
// second run would re-upload and could race the first.
func TestFlushTaskRecordRunsAtMostOnce(t *testing.T) {
	callLog := installFakeHook(t, "exit 0\n")
	a := &Agent{}

	a.flushTaskRecord(taskproto.ExitSpotInterruption)
	a.flushTaskRecord(taskproto.ExitTTLExpired)

	if calls := hookCalls(t, callLog); len(calls) != 1 {
		t.Errorf("expected one invocation across two calls, got %d: %v", len(calls), calls)
	}
}

// TestFlushTaskRecordSurvivesAFailingHook. This runs immediately before
// provider.Terminate on the path that protects the user's bill. A hook that
// exits non-zero must be logged and swallowed — never allowed to abort or hang
// the termination.
func TestFlushTaskRecordSurvivesAFailingHook(t *testing.T) {
	callLog := installFakeHook(t, "echo 'hook broke' >&2\nexit 9\n")
	a := &Agent{}

	a.flushTaskRecord(taskproto.ExitCostLimitExceeded) // must return, not panic

	if calls := hookCalls(t, callLog); len(calls) != 1 {
		t.Errorf("the hook should still have been attempted, got: %v", calls)
	}
}

// TestFlushTaskRecordBoundsAHangingHook. The timeout is the reason a wedged
// `aws` call cannot delay the shutdown it is attached to. 45s is well inside the
// ~2-minute spot window, since this runs ahead of any user pre-stop hook there.
func TestFlushTaskRecordBoundsAHangingHook(t *testing.T) {
	if taskFlushTimeout <= 0 {
		t.Fatal("the flush hook must be bounded; an unbounded hook can delay termination indefinitely")
	}
	if taskFlushTimeout >= 90*1e9 {
		t.Errorf("taskFlushTimeout = %v, too long: it must fit inside the ~2-minute spot window "+
			"ahead of the user pre-stop hook", taskFlushTimeout)
	}
}

// TestEveryLifecycleExitPathCarriesAReasonToken is a drift gate.
//
// The flush is hung off runPreStop precisely because every path that ends an
// instance's life already calls it — but that only holds if each of those calls
// passes a real token. A future exit path that passes nothing (or reuses
// ExitCompleted by copy-paste) would silently stop writing the record, which is
// exactly the class of bug #632 is: a lifecycle action that quietly produces no
// diagnostics. A reviewer cannot see that from the diff, so assert it.
func TestEveryLifecycleExitPathCarriesAReasonToken(t *testing.T) {
	src, err := os.ReadFile("agent.go")
	if err != nil {
		t.Fatalf("read agent.go: %v", err)
	}

	// Every call to the three lifecycle actions, with its argument list.
	call := regexp.MustCompile(`a\.(terminate|stop|hibernate)\(([^\n]*)\)`)
	matches := call.FindAllStringSubmatch(string(src), -1)
	if len(matches) < 5 {
		t.Fatalf("expected to find the lifecycle call sites, found %d — has the shape changed?", len(matches))
	}

	for _, m := range matches {
		fn, args := m[1], m[2]
		if !strings.Contains(args, "taskproto.Exit") {
			t.Errorf("a.%s(%s) passes no taskproto.Exit* reason token.\n"+
				"Every lifecycle exit must name the limit that fired, or the task's terminal "+
				"record (#632) is silently not written for that path.", fn, args)
		}
	}

	// And runPreStop itself must take the token — if someone drops the parameter,
	// the loop above would still pass while the flush lost its input.
	if !regexp.MustCompile(`func \(a \*Agent\) runPreStop\([^)]*taskproto\.ExitReason\)`).Match(src) {
		t.Error("runPreStop must accept a taskproto.ExitReason; the flush hook's retry-class " +
			"mapping is driven entirely by it")
	}

	// The flush must be invoked from runPreStop, not from each exit site — that is
	// what makes a newly added exit path get the record for free.
	if !strings.Contains(string(src), "a.flushTaskRecord(why)") {
		t.Error("runPreStop must call a.flushTaskRecord(why) so every exit path is covered by construction")
	}
}

// TestFlushRunsBeforeTheUserPreStopHook. The user's hook may take minutes
// (PreStopTimeout) and may itself fail or hang; the diagnostics should already
// be in S3 before it is given the chance.
func TestFlushRunsBeforeTheUserPreStopHook(t *testing.T) {
	src, err := os.ReadFile("agent.go")
	if err != nil {
		t.Fatalf("read agent.go: %v", err)
	}
	s := string(src)

	fnIdx := strings.Index(s, "func (a *Agent) runPreStop(")
	if fnIdx < 0 {
		t.Fatal("runPreStop not found")
	}
	body := s[fnIdx:]
	flushIdx := strings.Index(body, "a.flushTaskRecord(why)")
	gateIdx := strings.Index(body, "a.config.PreStop == \"\"")
	if flushIdx < 0 || gateIdx < 0 {
		t.Fatalf("expected both the flush call and the PreStop gate (flush@%d gate@%d)", flushIdx, gateIdx)
	}
	if flushIdx > gateIdx {
		t.Error("flushTaskRecord must run BEFORE the `PreStop == \"\"` early return — otherwise an " +
			"instance with no user pre-stop hook configured (the common case) never flushes at all")
	}
}
