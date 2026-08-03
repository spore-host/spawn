// Package service implements the readiness contract for long-lived HTTP
// workloads: a spawned binary announces the address it actually bound by
// printing one JSON line to stdout, and a launcher reads that line to learn
// where to tunnel (spore-host/spawn#409).
//
// The contract is deliberately tiny — one line, one discriminator — so any HTTP
// binary that prints it is spawnable by one generic verb. Nothing here knows
// what the workload does.
//
//	{"event":"ready","addr":"127.0.0.1:54321","provenance":{"sourceHash":"…"}}
//
// Reading that line is harder than it looks, and every guard below exists
// because the naive version was observed to fail against a real binary:
//
//   - The child's stdout is SHARED with arbitrary application output. A cell,
//     a package `init()`, or a top-level `var` initializer can print whatever
//     it likes, before or after the readiness line. So neither "first line",
//     "last line", nor "unmarshal the whole stream" works — only a per-line
//     scan that skips what doesn't parse.
//   - "First line where event==ready" is FORGEABLE. `init()` runs before
//     `main`, so a child that prints a well-formed readiness line of its own
//     choosing always wins the race — a launcher that trusts it tunnels to a
//     port the workload picked rather than the one it bound. Verify by dialing:
//     a forged address refuses the connection.
//   - A child that starts and never becomes ready is indistinguishable from a
//     slow one, so the boot timeout is the only detector for that class and its
//     error must carry the evidence (stderr, plus the stdout lines that didn't
//     match) or there is nothing left to debug with.
package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

// ReadyEvent is the readiness line a spawned service prints to stdout.
//
// Unknown fields are ignored rather than rejected: the contract is expected to
// grow additively (an optional access token arrived after the first consumer
// shipped), and a launcher that fails on a field it doesn't know would break on
// every such addition.
type ReadyEvent struct {
	// Event discriminates the readiness line from other JSON a workload may
	// print. Must be "ready".
	Event string `json:"event"`
	// Addr is the address the child ACTUALLY bound, after any :0 resolution.
	// The child picks the port — only it knows what's free — but announces it
	// so the launcher never has to poll and hope.
	Addr string `json:"addr"`
	// Token, when non-empty, is an access credential the child requires. It is
	// optional: a workload with an open endpoint omits it entirely.
	//
	// Treat it as a secret. It must not reach a log sink, a tag, or any output
	// spawn persists — the value is live for the life of the service.
	Token string `json:"token,omitempty"`
	// Provenance is opaque build identity, for logging WHAT was spawned. Left
	// as raw JSON on purpose: it is the workload's shape, not ours, and every
	// field in it is optional to us.
	Provenance json.RawMessage `json:"provenance,omitempty"`
}

// ErrNotReady is the sentinel for "no readiness line arrived". Callers
// distinguish the two causes via ReadinessError.
var ErrNotReady = errors.New("service never reported readiness")

// ReadinessError explains a failed wait and carries the evidence needed to
// debug it. Both fields matter: a hung child often writes nothing to stderr,
// leaving its unmatched stdout as the only clue about what it was doing.
type ReadinessError struct {
	// Cause distinguishes a crash from a hang. They present identically to a
	// reader — no readiness line — but the remedies differ, so a caller that
	// can't tell them apart gives the wrong advice.
	Cause ReadinessFailure
	// Waited is how long the reader waited before giving up.
	Waited time.Duration
	// ExitErr is the child's exit error, when it exited (Cause == FailureExited).
	ExitErr error
	// Stderr is whatever the child wrote to stderr, verbatim and untruncated by
	// this type. A bind failure puts an actionable message here.
	Stderr string
	// UnmatchedStdout holds the stdout lines that were not the readiness line,
	// in order, capped by the reader. This is the evidence a bare "timed out"
	// throws away.
	UnmatchedStdout []string
}

// ReadinessFailure is why a readiness wait ended without a readiness line.
type ReadinessFailure int

const (
	// FailureExited means the child process ended before announcing readiness:
	// a crash, a bad flag, a port collision. The exit code and stderr say why.
	FailureExited ReadinessFailure = iota
	// FailureTimeout means the child is still running but hasn't announced
	// readiness within the deadline — a hang, or a boot slower than the budget.
	FailureTimeout
	// FailureStdoutClosed means the child closed stdout without announcing
	// readiness, so no line can ever arrive even though the process may live.
	FailureStdoutClosed
)

func (e *ReadinessError) Unwrap() error { return ErrNotReady }

func (e *ReadinessError) Error() string {
	var b strings.Builder
	switch e.Cause {
	case FailureExited:
		b.WriteString("service exited before reporting readiness")
		if e.ExitErr != nil {
			fmt.Fprintf(&b, " (%v)", e.ExitErr)
		}
	case FailureStdoutClosed:
		b.WriteString("service closed stdout before reporting readiness")
	default:
		fmt.Fprintf(&b, "service did not report readiness within %s", e.Waited.Round(time.Millisecond))
	}
	if s := strings.TrimSpace(e.Stderr); s != "" {
		fmt.Fprintf(&b, "\nstderr: %s", s)
	}
	if len(e.UnmatchedStdout) > 0 {
		fmt.Fprintf(&b, "\nstdout (no readiness line among %d line(s)):", len(e.UnmatchedStdout))
		for _, l := range e.UnmatchedStdout {
			fmt.Fprintf(&b, "\n  %s", l)
		}
	}
	if e.Cause == FailureTimeout {
		b.WriteString("\nif the service is merely slow to boot, raise --boot-timeout")
	}
	return b.String()
}

// maxUnmatchedLines caps the stdout kept for diagnostics. A chatty workload can
// print without bound, and an error message is not a log file.
const maxUnmatchedLines = 20

// Dialer reports whether addr is actually accepting connections. It exists so
// the forgery check is testable without real sockets.
type Dialer func(ctx context.Context, addr string) error

// TCPDialer verifies an address by connecting to it and hanging up.
//
// This is what makes a forged readiness line harmless: a child can claim any
// address, but it cannot make an address it doesn't own accept a connection.
// The check is nearly free — a launcher wants to know the endpoint is live
// before it prints a URL anyway.
func TCPDialer(timeout time.Duration) Dialer {
	return func(ctx context.Context, addr string) error {
		d := net.Dialer{Timeout: timeout}
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return err
		}
		return conn.Close()
	}
}

// WaitOptions configures WaitForReady.
type WaitOptions struct {
	// Timeout bounds the wait. Zero means DefaultBootTimeout.
	//
	// This is load-bearing, not a safety net: a child that starts and hangs
	// looks exactly like one that is slow, so this deadline is the ONLY thing
	// that detects that class of failure. Surface it as a user-facing flag —
	// legitimate boot time is unbounded (a workload may do real work before it
	// serves).
	Timeout time.Duration
	// Dial verifies a candidate address before it is accepted. Nil means
	// TCPDialer with a short timeout. Setting this to a func that always
	// returns nil disables forgery protection — don't, outside tests.
	Dial Dialer
	// Stderr, if non-nil, is drained concurrently and its contents attached to
	// any ReadinessError. Draining also prevents a child from blocking on a
	// full stderr pipe, which would masquerade as a hang.
	Stderr io.Reader
	// AfterReady, if non-nil, receives the stdout lines that follow the
	// readiness line, for as long as the child keeps writing.
	//
	// This exists because the caller MUST NOT read stdout itself after a
	// successful wait: this package's scanner has buffered ahead, so a second
	// reader on the same pipe silently loses whatever the scanner already holds
	// and races it for the rest. Draining here keeps exactly one reader on the
	// pipe for its whole life — which also stops a chatty workload from
	// blocking on a full pipe once nobody is listening.
	AfterReady io.Writer
	// Exited, if non-nil, delivers the child's exit before readiness. It turns
	// a full-timeout wait into an immediate, better-diagnosed failure.
	//
	// WaitForReady may RECEIVE from this channel, so it must have a dedicated
	// producer — a caller that also waits on the same channel for its own
	// cleanup will block forever on a value already consumed here. Wire the exit
	// to a channel that is closed (not sent to) if you need to observe it in
	// more than one place; see RunLocal for the pattern.
	Exited <-chan error
}

// DefaultBootTimeout is the default readiness budget. Generous, because a
// workload may legitimately do heavy work before it serves, and a false
// "not ready" costs a whole launch.
const DefaultBootTimeout = 2 * time.Minute

// WaitForReady scans stdout for the readiness line and returns it.
//
// It reads line by line, ignoring anything that isn't a readiness line, until
// one both parses AND passes the dial check. Callers pass the child's stdout
// pipe; WaitForReady does not close it, because the caller usually wants to
// keep draining it (an undrained pipe eventually blocks the child).
func WaitForReady(ctx context.Context, stdout io.Reader, opts WaitOptions) (*ReadyEvent, error) {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultBootTimeout
	}
	dial := opts.Dial
	if dial == nil {
		dial = TCPDialer(2 * time.Second)
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Drain stderr concurrently. Two reasons: its contents are the best
	// diagnostic when readiness never comes, and an undrained pipe can fill
	// and block the child, which would look like a hang we caused ourselves.
	var (
		errMu  sync.Mutex
		errBuf strings.Builder
	)
	// Closed when stderr reaches EOF. The exit path waits on this: a child that
	// died explains itself on stderr, and reporting the failure before that text
	// has been read produces an error with the diagnosis stripped out.
	stderrDone := make(chan struct{})
	if opts.Stderr == nil {
		close(stderrDone)
	} else {
		go func() {
			defer close(stderrDone)
			buf := make([]byte, 4096)
			for {
				n, err := opts.Stderr.Read(buf)
				if n > 0 {
					errMu.Lock()
					// Bound it: an error message is not a log file.
					if errBuf.Len() < 8192 {
						errBuf.Write(buf[:n])
					}
					errMu.Unlock()
				}
				if err != nil {
					return
				}
			}
		}()
	}
	stderrText := func() string {
		errMu.Lock()
		defer errMu.Unlock()
		return errBuf.String()
	}
	// awaitStderr gives the drain a bounded chance to finish. Bounded because a
	// child can hold stderr open forever (or share it with a grandchild), and
	// hanging a launch to collect a diagnostic would trade a bad error message
	// for a worse bug.
	awaitStderr := func() {
		select {
		case <-stderrDone:
		case <-time.After(2 * time.Second):
		}
	}

	type lineResult struct {
		ev   *ReadyEvent
		line string
		err  error
	}
	lines := make(chan lineResult)

	// forward is set by the success path below to tell the scan goroutine to
	// stop reporting lines and start copying them to opts.AfterReady. Guarded
	// because the two goroutines touch it. It is only ever set once, at the
	// moment WaitForReady returns a readiness event.
	var (
		fwdMu   sync.Mutex
		forward bool
	)
	forwarding := func() bool {
		fwdMu.Lock()
		defer fwdMu.Unlock()
		return forward
	}

	// The scan runs in a goroutine so the select below can honor the deadline
	// even while a Read blocks on a child that has gone quiet. It owns `stdout`
	// for the child's whole life — after readiness it keeps draining, because a
	// second reader would race it and lose whatever it had buffered.
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(stdout)
		// Readiness lines carry provenance and may exceed the 64KB default.
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if forwarding() {
				if opts.AfterReady != nil {
					_, _ = fmt.Fprintln(opts.AfterReady, line)
				}
				continue
			}
			ev, ok := parseReady(line)
			if !ok {
				select {
				case lines <- lineResult{line: line}:
				case <-ctx.Done():
					// The wait is over. Keep draining rather than exiting: an
					// unread pipe eventually blocks the child, and on the
					// success path there is still a live service on the far end.
					continue
				}
				continue
			}
			select {
			case lines <- lineResult{ev: ev, line: line}:
			case <-ctx.Done():
				continue
			}
		}
		if err := sc.Err(); err != nil {
			select {
			case lines <- lineResult{err: err}:
			case <-ctx.Done():
			}
		}
	}()

	start := time.Now()
	var unmatched []string
	noteUnmatched := func(line string) {
		if line == "" || len(unmatched) >= maxUnmatchedLines {
			return
		}
		unmatched = append(unmatched, line)
	}

	for {
		select {
		case <-ctx.Done():
			return nil, &ReadinessError{
				Cause:           FailureTimeout,
				Waited:          time.Since(start),
				Stderr:          stderrText(),
				UnmatchedStdout: unmatched,
			}

		case err := <-opts.Exited:
			// The child is gone. Drain whatever stdout it left, so a readiness
			// line that raced the exit isn't lost and the diagnostics are
			// complete.
			for res := range lines {
				if res.ev == nil {
					noteUnmatched(res.line)
					continue
				}
				if derr := dial(ctx, res.ev.Addr); derr == nil {
					return res.ev, nil
				}
				noteUnmatched(res.line)
			}
			// The child's own stderr is usually the ONLY actionable part of this
			// error ("bind: address already in use"), so wait for the drain
			// rather than racing it.
			awaitStderr()
			return nil, &ReadinessError{
				Cause:           FailureExited,
				Waited:          time.Since(start),
				ExitErr:         err,
				Stderr:          stderrText(),
				UnmatchedStdout: unmatched,
			}

		case res, ok := <-lines:
			if !ok {
				// stdout closed with no readiness line: nothing can arrive now,
				// so fail immediately rather than burning the whole timeout.
				//
				// But a crashing child closes stdout AND exits, and those arrive
				// as two events in an arbitrary order. "Exited" is the strictly
				// better diagnosis — it carries the exit code — so give it a
				// moment to land rather than reporting whichever won the race.
				if opts.Exited != nil {
					select {
					case err := <-opts.Exited:
						awaitStderr()
						return nil, &ReadinessError{
							Cause:           FailureExited,
							Waited:          time.Since(start),
							ExitErr:         err,
							Stderr:          stderrText(),
							UnmatchedStdout: unmatched,
						}
					case <-time.After(2 * time.Second):
						// Still running with stdout closed: a real (if odd)
						// state, and genuinely not an exit.
					case <-ctx.Done():
					}
				}
				awaitStderr()
				return nil, &ReadinessError{
					Cause:           FailureStdoutClosed,
					Waited:          time.Since(start),
					Stderr:          stderrText(),
					UnmatchedStdout: unmatched,
				}
			}
			if res.err != nil {
				return nil, fmt.Errorf("reading service stdout: %w", res.err)
			}
			if res.ev == nil {
				noteUnmatched(res.line)
				continue
			}
			// A well-formed readiness line is a CANDIDATE, not an answer. Only
			// the address that actually accepts a connection is real; a forged
			// line (trivial from init(), which runs before main) cannot.
			if err := dial(ctx, res.ev.Addr); err != nil {
				noteUnmatched(fmt.Sprintf("%s  [rejected: %s is not accepting connections: %v]",
					res.line, res.ev.Addr, err))
				continue
			}
			// Hand the rest of the stream to the caller's sink. The scanner
			// keeps ownership of the pipe — see WaitOptions.AfterReady for why a
			// second reader would lose data.
			fwdMu.Lock()
			forward = true
			fwdMu.Unlock()
			return res.ev, nil
		}
	}
}

// parseReady reports whether line is a readiness line, and decodes it.
//
// Everything that isn't one is skipped silently: application chatter, framework
// logs that landed on stdout, and — importantly — the OTHER JSON document a
// workload may print (a batch-mode result envelope), which is why the `event`
// discriminator is checked rather than "does it parse as JSON".
func parseReady(line string) (*ReadyEvent, bool) {
	trimmed := strings.TrimSpace(line)
	// Cheap pre-filter: skip the common case (plain text) without invoking the
	// JSON decoder on every log line a chatty workload emits.
	if !strings.HasPrefix(trimmed, "{") {
		return nil, false
	}
	var ev ReadyEvent
	if err := json.Unmarshal([]byte(trimmed), &ev); err != nil {
		return nil, false
	}
	if ev.Event != "ready" || ev.Addr == "" {
		return nil, false
	}
	// A readiness line whose address doesn't parse as host:port can't be
	// tunneled to, so it isn't a usable readiness line.
	if _, _, err := net.SplitHostPort(ev.Addr); err != nil {
		return nil, false
	}
	return &ev, true
}

// Port extracts the port from a readiness address.
func (e *ReadyEvent) Port() (string, error) {
	_, port, err := net.SplitHostPort(e.Addr)
	if err != nil {
		return "", fmt.Errorf("readiness addr %q: %w", e.Addr, err)
	}
	return port, nil
}
