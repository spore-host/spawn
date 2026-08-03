package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Local is a service running as a child process on this machine.
//
// It exists so the readiness → connect → drive → reap cycle can be exercised at
// $0, without EC2. That matters more than convenience: the Substrate emulator is
// control-plane-only (instances never boot, there is no SSH), so it cannot test
// the stdout or tunnel paths at all. Without a loopback mode the first real
// spawn would be the first test of this code.
//
// The address a Local reports is the one the child actually bound, verified by
// dialing it — the same contract and the same guards as the remote path.
type Local struct {
	// Ready is the verified readiness line the child printed.
	Ready *ReadyEvent

	cmd    *exec.Cmd
	stdout io.ReadCloser
	// done is closed when the child has exited and exitErr is set. Closed
	// (rather than sent to) so any number of observers can wait on it.
	done     chan struct{}
	exitErr  error
	stopOnce sync.Once
}

// LocalOptions configures RunLocal.
type LocalOptions struct {
	// Path is the executable to run. Required.
	Path string
	// Args are passed after the readiness-enabling address flag.
	Args []string
	// AddrFlag is the flag the child takes to choose its listen address, and
	// AddrValue what to pass. Defaults to "--addr" and "127.0.0.1:0".
	//
	// Port 0 is the point: the child picks a free port and announces it, so
	// nothing has to guess or poll, and two concurrent runs can't collide.
	AddrFlag  string
	AddrValue string
	// Env is the child's environment. Nil inherits the parent's.
	Env []string
	// Dir is the child's working directory. Empty means the parent's.
	Dir string
	// BootTimeout bounds the readiness wait. Zero means DefaultBootTimeout.
	BootTimeout time.Duration
	// Stdout, if non-nil, receives the child's stdout AFTER the readiness line
	// (a workload keeps printing; an undrained pipe eventually blocks it).
	Stdout io.Writer
	// Stderr, if non-nil, receives the child's stderr as it is produced. Note
	// that a workload may print its own access token here — do not wire this to
	// anything that persists without considering that.
	Stderr io.Writer
	// Dial overrides the address verification. Nil means a real TCP dial. Only
	// tests should set this.
	Dial Dialer
}

// RunLocal starts a service as a local child process and waits for it to
// announce readiness.
//
// On any failure the child is killed before returning, so a failed RunLocal
// never leaks a process. The caller must Stop a successful one.
func RunLocal(ctx context.Context, opts LocalOptions) (*Local, error) {
	if opts.Path == "" {
		return nil, errors.New("service: LocalOptions.Path is required")
	}
	addrFlag := opts.AddrFlag
	if addrFlag == "" {
		addrFlag = "--addr"
	}
	addrValue := opts.AddrValue
	if addrValue == "" {
		addrValue = "127.0.0.1:0"
	}

	args := append([]string{addrFlag, addrValue}, opts.Args...)
	cmd := exec.Command(opts.Path, args...)
	cmd.Env = opts.Env
	cmd.Dir = opts.Dir

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("service: stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("service: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("service: starting %s: %w", opts.Path, err)
	}

	// The exit is observed in two places — WaitForReady (for diagnosis) and
	// Stop/Wait (for reaping) — so it is published as a CLOSED channel plus a
	// stored error rather than a single value on a channel. A one-shot channel
	// would deadlock the second observer on a value the first already took.
	exitedC := make(chan error, 1)
	l := &Local{cmd: cmd, stdout: stdout, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		l.exitErr = err
		close(l.done)
		// Non-blocking: WaitForReady may or may not still be listening.
		select {
		case exitedC <- err:
		default:
		}
	}()

	// Tee stderr: WaitForReady needs it for diagnostics, and the caller may want
	// to see it live. Without the tee, one consumer would starve the other.
	var stderrForWait io.Reader = stderrPipe
	if opts.Stderr != nil {
		stderrForWait = io.TeeReader(stderrPipe, opts.Stderr)
	}

	// AfterReady rather than reading `stdout` ourselves once WaitForReady
	// returns: its scanner has buffered ahead, so a second reader on the same
	// pipe would lose data and race it. One reader owns the pipe for the child's
	// whole life.
	afterReady := opts.Stdout
	if afterReady == nil {
		// Still drained, just discarded — a chatty workload blocks forever on a
		// full pipe nobody is reading.
		afterReady = io.Discard
	}

	ev, err := WaitForReady(ctx, stdout, WaitOptions{
		Timeout:    opts.BootTimeout,
		Dial:       opts.Dial,
		Stderr:     stderrForWait,
		Exited:     exitedC,
		AfterReady: afterReady,
	})
	if err != nil {
		// Kill before returning: a child that never reported readiness may still
		// be running, and a failed launch must not leave one behind.
		_ = cmd.Process.Kill()
		<-l.done
		return nil, err
	}

	// stdout keeps being drained by WaitForReady's scanner into afterReady, for
	// the child's whole life. Nothing else may read it.
	l.Ready = ev
	return l, nil
}

// Addr is the address the child actually bound.
func (l *Local) Addr() string { return l.Ready.Addr }

// URL is the base HTTP URL for the service, including the access token as a
// query parameter when the child requires one.
//
// Callers that log or persist the result must use Addr instead: this may carry
// a live credential.
func (l *Local) URL() string {
	u := "http://" + l.Ready.Addr
	if l.Ready.Token != "" {
		u += "/?token=" + l.Ready.Token
	}
	return u
}

// Wait blocks until the child exits and returns its exit error.
func (l *Local) Wait() error {
	<-l.done
	return l.exitErr
}

// Stop terminates the child and reaps it. Safe to call more than once, and safe
// to call on an already-exited child — a stop that finds the work already done
// is a success, not an error.
func (l *Local) Stop() error {
	l.stopOnce.Do(func() {
		if l.cmd.Process == nil {
			return
		}
		// Ask politely first so the workload can shut down cleanly; escalate if
		// it doesn't. A service that ignores SIGTERM must not become immortal.
		if err := l.cmd.Process.Signal(os.Interrupt); err != nil {
			_ = l.cmd.Process.Kill()
		}
		select {
		case <-l.done:
		case <-time.After(5 * time.Second):
			_ = l.cmd.Process.Kill()
			<-l.done
		}
	})
	return nil
}
