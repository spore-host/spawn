package cmd

import (
	"context"
	"errors"
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

// sshRegisterRetryable: the predicate used to be `err != nil || <curl checks>`,
// so EVERY error was transient and an unreachable host burned the full
// four-minute budget on ~16 attempts that could not have succeeded (#741).
func TestSSHRegisterRetryable(t *testing.T) {
	errExit := errors.New("exit status 255")

	for _, tc := range []struct {
		name   string
		err    error
		output string
		want   bool
		why    string
	}{
		{"success", nil, `{"success":true}`, false, "nothing to retry"},
		{
			"cloud-init has not written authorized_keys yet",
			errExit, "Permission denied (publickey).", true,
			"a fresh session later DOES recover this — it is the race the retry exists for",
		},
		{"instance resolver not up (curl 6)", errExit, "curl exit 6", true, "early-boot DNS race"},
		{"instance cannot connect (curl 7)", errExit, "curl exit 7", true, "early-boot DNS race"},
		{"instance request timed out (curl 28)", errExit, "curl exit 28", true, "early-boot DNS race"},
		{
			"curl race reported WITHOUT a process error",
			nil, "curl exit 6", true,
			"the remote script can report the race on a zero exit; must still retry",
		},
		{
			"host refuses the connection",
			errExit, "ssh: connect to host 10.0.0.1 port 22: Connection refused", false,
			"a fresh session reaches the same refusing host — this is #741's four wasted minutes",
		},
		{"connect times out", errExit, "ssh: connect to host 10.0.0.1 port 22: Connection timed out", false, "unreachable"},
		{"no route", errExit, "ssh: connect to host 10.0.0.1 port 22: No route to host", false, "unreachable"},
		{"network unreachable", errExit, "connect: Network is unreachable", false, "unreachable"},
		{
			"an unrecognised failure stays retryable",
			errExit, "ssh: some message we have never seen", true,
			"degrade to the old behaviour rather than giving up early on an unknown",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sshRegisterRetryable(tc.err, []byte(tc.output)); got != tc.want {
				t.Errorf("sshRegisterRetryable = %v, want %v — %s", got, tc.want, tc.why)
			}
		})
	}
}
