package cmd

import (
	"context"
	"strings"
	"testing"
	"time"
)

// waitForSSHReady used to return nothing, which made the timeout path and the
// success path the same statement — the failure was not merely dropped, it was
// unrepresentable (#740). The caller then marked the step complete
// unconditionally, so an unreachable instance printed
// `✅ Waiting for SSH (120.0s)`: the timeout, to the tenth of a second, rendered
// as success.
func TestWaitForSSHReady_TimeoutReturnsAnError(t *testing.T) {
	// 203.0.113.1 is TEST-NET-3 (RFC 5737) and unroutable, so port 22 never
	// answers. Deliberately NOT 127.0.0.1: the probe hardcodes port 22, and a
	// developer machine with Remote Login enabled has a real sshd there — the
	// first version of this test passed for that reason, which is a good
	// reminder that "unreachable" has to be unreachable for the probe's port,
	// not just for a port I chose.
	const unreachable = "203.0.113.1"

	start := time.Now()
	err := waitForSSHReady(context.Background(), unreachable, 1500*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("waitForSSHReady returned nil for an unreachable host — the caller cannot " +
			"tell this from success, which is exactly #740")
	}
	// The message has to be actionable: it is what the user sees instead of a ✅.
	if !strings.Contains(err.Error(), "did not become reachable") {
		t.Errorf("error does not say what failed: %v", err)
	}
	if !strings.Contains(err.Error(), unreachable) {
		t.Errorf("error does not name the host: %v", err)
	}
	// Bounded: it must honour the deadline rather than the dialer's own timeout.
	if elapsed > 5*time.Second {
		t.Errorf("took %s for a 1.5s timeout — the deadline is not binding", elapsed)
	}
}

// Note: waitForSSHReady probes port 22 specifically, so a positive test needs a
// listener on 22, which requires privileges. The negative case above is the one
// that regressed, and it is the one that matters: a false ✅ is worse than a
// missed ✓.
func TestWaitForSSHReady_RespectsCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	start := time.Now()
	err := waitForSSHReady(ctx, "203.0.113.1", 30*time.Second) // TEST-NET-1, unroutable
	if err == nil {
		t.Fatal("a cancelled context produced a nil error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("ignored the cancelled context for %s", elapsed)
	}
}
