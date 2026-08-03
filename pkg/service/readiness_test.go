package service

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// okDial accepts any address. Used where the test is about parsing, not
// forgery — every forgery test uses a real listener instead.
func okDial(context.Context, string) error { return nil }

// listen starts a real listener and returns its address plus a cleanup. Forgery
// tests need a genuinely bound port, because the whole point of the dial check
// is that it can't be satisfied by a claim.
func listen(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return ln.Addr().String()
}

func TestParseReady(t *testing.T) {
	tests := []struct {
		name string
		line string
		want bool
	}{
		{"canonical", `{"event":"ready","addr":"127.0.0.1:54321"}`, true},
		{
			// Key order is sorted (encoding/json sorts map keys), so `addr`
			// precedes `event` in what a real workload emits. A reader that
			// assumed `event` came first would break on every real line.
			name: "addr before event, as a real binary emits it",
			line: `{"addr":"127.0.0.1:56219","event":"ready","provenance":{"sourceHash":"abc"}}`,
			want: true,
		},
		{
			// The token arrived after the contract's first consumer shipped;
			// unknown/new fields must not break the parse.
			name: "with an access token",
			line: `{"addr":"127.0.0.1:1024","event":"ready","token":"6becc7e0d5eebf8e9130cd61759693cb"}`,
			want: true,
		},
		{
			name: "field we have never seen is ignored, not rejected",
			line: `{"event":"ready","addr":"127.0.0.1:1024","somethingNew":{"a":1}}`,
			want: true,
		},
		{"plain application chatter", "cell chatter: computing for 3", false},
		{"framework log that landed on stdout", "2026/08/02 17:23:59 notebook serving on http://127.0.0.1:1", false},
		{"empty", "", false},
		{"not json", "{not json", false},
		{
			// Batch mode emits a DIFFERENT JSON document on stdout. This is why
			// the discriminator is checked rather than "does it parse as JSON".
			name: "a different JSON document",
			line: `{"provenance":{"sourceHash":"abc"},"values":{"c":40}}`,
			want: false,
		},
		{"wrong event", `{"event":"done","addr":"127.0.0.1:1"}`, false},
		{"ready but no addr", `{"event":"ready"}`, false},
		{"addr is not host:port", `{"event":"ready","addr":"not-an-addr"}`, false},
		{"leading whitespace is tolerated", `  {"event":"ready","addr":"127.0.0.1:9"}`, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, got := parseReady(tc.line)
			if got != tc.want {
				t.Errorf("parseReady(%q) = %v, want %v", tc.line, got, tc.want)
			}
		})
	}
}

func TestWaitForReadySkipsChatterBeforeAndAfter(t *testing.T) {
	// Both orderings were observed against a real binary: a package init() or
	// top-level var can print BEFORE the readiness line, and demand-driven work
	// prints AFTER it. So neither the first line nor the whole stream parses.
	addr := listen(t)
	stdout := strings.NewReader(strings.Join([]string{
		"package init chatter — before the listener binds",
		fmt.Sprintf(`{"addr":%q,"event":"ready","provenance":{"sourceHash":"abc"}}`, addr),
		"cell chatter: computing for 3",
	}, "\n") + "\n")

	ev, err := WaitForReady(context.Background(), stdout, WaitOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("WaitForReady: %v", err)
	}
	if ev.Addr != addr {
		t.Errorf("addr = %q, want %q", ev.Addr, addr)
	}
}

func TestWaitForReadyRejectsForgedLine(t *testing.T) {
	// The attack: init() runs before main, so a workload can always print a
	// well-formed readiness line FIRST, naming a port of its choosing. A
	// "first match wins" reader would tunnel there. Only the real listener
	// accepts a connection.
	real := listen(t)
	forged := unusedAddr(t)
	stdout := strings.NewReader(
		fmt.Sprintf(`{"event":"ready","addr":%q,"provenance":{"sourceHash":"forged"}}`, forged) + "\n" +
			fmt.Sprintf(`{"addr":%q,"event":"ready","provenance":{"sourceHash":"real"}}`, real) + "\n")

	ev, err := WaitForReady(context.Background(), stdout, WaitOptions{
		Timeout: 5 * time.Second,
		Dial:    TCPDialer(500 * time.Millisecond),
	})
	if err != nil {
		t.Fatalf("WaitForReady: %v", err)
	}
	if ev.Addr != real {
		t.Fatalf("accepted %q, want the address that actually listens (%q) — the forged line won", ev.Addr, real)
	}
}

func TestWaitForReadyForgedOnlyIsNotReady(t *testing.T) {
	// A forged line and nothing else must fail, not succeed with a bad addr.
	forged := unusedAddr(t)
	stdout := strings.NewReader(fmt.Sprintf(`{"event":"ready","addr":%q}`, forged) + "\n")

	_, err := WaitForReady(context.Background(), stdout, WaitOptions{
		Timeout: 5 * time.Second,
		Dial:    TCPDialer(300 * time.Millisecond),
	})
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("err = %v, want ErrNotReady", err)
	}
	// The rejection reason must survive into the diagnostics, or a user sees
	// "not ready" with no hint that a line was present but unreachable.
	var re *ReadinessError
	if !errors.As(err, &re) {
		t.Fatalf("err is not a *ReadinessError: %T", err)
	}
	if !strings.Contains(err.Error(), "not accepting connections") {
		t.Errorf("error does not explain the rejection:\n%s", err)
	}
}

func TestWaitForReadyStdoutClosedFailsFast(t *testing.T) {
	// stdout closing with no readiness line means none can ever arrive. Failing
	// immediately (rather than burning the full timeout) is the difference
	// between a 1s error and a 2min one.
	start := time.Now()
	_, err := WaitForReady(context.Background(), strings.NewReader("just chatter\n"), WaitOptions{
		Timeout: 30 * time.Second,
		Dial:    okDial,
	})
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("err = %v, want ErrNotReady", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %s — should fail as soon as stdout closes, not wait for the timeout", elapsed)
	}
	var re *ReadinessError
	if errors.As(err, &re) && re.Cause != FailureStdoutClosed {
		t.Errorf("Cause = %v, want FailureStdoutClosed", re.Cause)
	}
}

func TestWaitForReadyTimeoutCarriesEvidence(t *testing.T) {
	// The hang case: the child lives, stdout stays open, nothing arrives. This
	// is indistinguishable from a slow boot, so the timeout is the only
	// detector — and its error is the only evidence a user gets.
	pr, pw := net.Pipe()
	defer pw.Close()
	go func() {
		// Chatter, then silence with the stream still open.
		fmt.Fprintln(pw, "init: about to hang")
		time.Sleep(10 * time.Second)
	}()

	_, err := WaitForReady(context.Background(), pr, WaitOptions{
		Timeout: 700 * time.Millisecond,
		Dial:    okDial,
	})
	var re *ReadinessError
	if !errors.As(err, &re) {
		t.Fatalf("err is not a *ReadinessError: %v", err)
	}
	if re.Cause != FailureTimeout {
		t.Errorf("Cause = %v, want FailureTimeout", re.Cause)
	}
	// A bare "timed out" throws away the one clue about what the child was
	// doing. Observed live: the hung child wrote NOTHING to stderr, so its
	// stdout chatter was the only evidence.
	if !strings.Contains(err.Error(), "init: about to hang") {
		t.Errorf("timeout error dropped the unmatched stdout, which is the only evidence:\n%s", err)
	}
	if !strings.Contains(err.Error(), "--boot-timeout") {
		t.Errorf("timeout error should point at the flag that fixes a slow boot:\n%s", err)
	}
}

func TestWaitForReadyExitedCarriesStderr(t *testing.T) {
	// Bind failure, observed live: exit 1, empty stdout, an actionable message
	// on stderr ("address already in use"). Surfacing that verbatim is the
	// difference between a fixable error and "never became ready".
	exited := make(chan error, 1)
	exited <- errors.New("exit status 1")

	_, err := WaitForReady(context.Background(), strings.NewReader(""), WaitOptions{
		Timeout: 30 * time.Second,
		Dial:    okDial,
		Stderr:  strings.NewReader("server: listening on 127.0.0.1:56454: bind: address already in use\n"),
		Exited:  exited,
	})
	var re *ReadinessError
	if !errors.As(err, &re) {
		t.Fatalf("err is not a *ReadinessError: %v", err)
	}
	if re.Cause != FailureExited {
		t.Errorf("Cause = %v, want FailureExited (a crash, not a hang)", re.Cause)
	}
	if !strings.Contains(err.Error(), "address already in use") {
		t.Errorf("error dropped the actionable stderr:\n%s", err)
	}
	// A crash must NOT advise raising the boot timeout — that would send the
	// reader down the wrong path entirely.
	if strings.Contains(err.Error(), "--boot-timeout") {
		t.Errorf("a crash should not suggest a longer timeout:\n%s", err)
	}
}

func TestWaitForReadyReadyLineRacingExit(t *testing.T) {
	// A child can print readiness and exit immediately after. The exit must not
	// discard a valid line that was already in the pipe.
	addr := listen(t)
	exited := make(chan error, 1)
	exited <- errors.New("exit status 0")

	ev, err := WaitForReady(context.Background(), strings.NewReader(
		fmt.Sprintf(`{"event":"ready","addr":%q}`, addr)+"\n"), WaitOptions{
		Timeout: 5 * time.Second,
		Dial:    TCPDialer(500 * time.Millisecond),
		Exited:  exited,
	})
	if err != nil {
		t.Fatalf("readiness line lost to the exit: %v", err)
	}
	if ev.Addr != addr {
		t.Errorf("addr = %q, want %q", ev.Addr, addr)
	}
}

func TestWaitForReadyUnmatchedStdoutIsBounded(t *testing.T) {
	// A chatty workload can print without bound; an error message is not a log
	// file.
	var b strings.Builder
	for i := 0; i < maxUnmatchedLines*5; i++ {
		fmt.Fprintf(&b, "chatter line %d\n", i)
	}
	_, err := WaitForReady(context.Background(), strings.NewReader(b.String()), WaitOptions{
		Timeout: 5 * time.Second,
		Dial:    okDial,
	})
	var re *ReadinessError
	if !errors.As(err, &re) {
		t.Fatalf("err is not a *ReadinessError: %v", err)
	}
	if len(re.UnmatchedStdout) > maxUnmatchedLines {
		t.Errorf("kept %d lines, want at most %d", len(re.UnmatchedStdout), maxUnmatchedLines)
	}
}

func TestReadyEventPort(t *testing.T) {
	ev := &ReadyEvent{Addr: "127.0.0.1:54321"}
	port, err := ev.Port()
	if err != nil {
		t.Fatalf("Port: %v", err)
	}
	if port != "54321" {
		t.Errorf("port = %q, want 54321", port)
	}
	if _, err := (&ReadyEvent{Addr: "bogus"}).Port(); err == nil {
		t.Error("Port on a malformed addr should fail")
	}
}

// TestWaitForReadyAgainstRealChild runs the reader against an actual process,
// not a string. The failure modes this package guards against were all found
// this way — a stub reader passes tests a real pipe defeats (buffering, a
// process that holds stdout open, chatter interleaved with the real line).
func TestWaitForReadyAgainstRealChild(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a child process; skipped in -short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell")
	}
	addr := listen(t)
	// A child that behaves like the real thing: forges a readiness line from
	// "init" first, prints the true one, then keeps running and chattering.
	script := fmt.Sprintf(`
echo 'init chatter, before anything binds'
echo '{"event":"ready","addr":"127.0.0.1:1","provenance":{"sourceHash":"forged"}}'
echo '{"addr":"%s","event":"ready","provenance":{"sourceHash":"real"}}'
echo 'post-readiness cell output'
sleep 30
`, addr)
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command -- test fixture; script is the literal above with a loopback address from listen(t) interpolated. Running a shell is the point: this reproduces a workload that forges a readiness line before the real one.
	cmd := exec.Command("/bin/sh", "-c", script)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	ev, err := WaitForReady(context.Background(), stdout, WaitOptions{
		Timeout: 10 * time.Second,
		Dial:    TCPDialer(time.Second),
		Stderr:  stderr,
		Exited:  exited,
	})
	if err != nil {
		t.Fatalf("WaitForReady against a real child: %v", err)
	}
	if ev.Addr != addr {
		t.Fatalf("accepted %q, want %q — the forged line won against a real process", ev.Addr, addr)
	}
}

// unusedAddr returns an address that nothing is listening on, by binding a port
// and immediately releasing it. Racy in principle; the window is tiny and the
// alternative (a hardcoded port) is worse because it can collide with a real
// service on a developer's machine.
func unusedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return addr
}
