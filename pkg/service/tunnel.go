package service

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Tunnel is a live `ssh -L` port-forward from a local port to an address on the
// remote host.
//
// It generalizes the two hardcoded forwards spawn already had (both pinned to
// remote port 7777 with a one-shot HTTP callback) into a forward to an arbitrary
// remote address that stays open until it is closed — which is what a long-lived
// service needs, since the port the service bound is only known at runtime.
type Tunnel struct {
	// LocalAddr is the local address the forward is listening on, with the port
	// resolved. This is what a caller builds a URL from.
	LocalAddr string
	// RemoteAddr is the address on the far side, for diagnostics.
	RemoteAddr string

	cmd      *exec.Cmd
	stderr   *boundedBuffer
	done     chan struct{}
	exitErr  error
	stopOnce sync.Once
}

// SSHTarget identifies the host to forward through. It mirrors what
// `spawn connect` resolves for an instance (user from the spawn:local-username
// tag, public IP, and the launch key), so the two agree about how spawn reaches
// a box.
type SSHTarget struct {
	// User is the remote login user. Required.
	User string
	// Host is the remote hostname or IP. Required.
	Host string
	// Port is the SSH port. Zero means 22.
	Port int
	// KeyPath is the private key to authenticate with. Empty relies on the
	// caller's ambient SSH setup (agent, ssh_config).
	KeyPath string
}

// TunnelOptions configures OpenTunnel.
type TunnelOptions struct {
	// Target is the host to forward through. Ignored when Start is set.
	Target SSHTarget
	// RemoteAddr is the address on the remote host to forward to, host:port.
	// Required — for a service, the addr off its readiness line.
	RemoteAddr string
	// LocalPort is the local port to listen on. Zero picks a free one, which is
	// the right default: a fixed local port collides with a second concurrent
	// tunnel, and there is no reason for the local port to match the remote one.
	LocalPort int
	// ReadyTimeout bounds the wait for the forward to start accepting
	// connections. Zero means DefaultTunnelReadyTimeout.
	ReadyTimeout time.Duration
	// Dial overrides the readiness probe. Nil means a real TCP dial.
	Dial Dialer
	// Start overrides process creation, receiving the resolved local port and
	// remote address. Only tests should set this; it is how the tunnel path is
	// exercised without a remote host.
	Start func(ctx context.Context, localPort int, remoteAddr string) (*exec.Cmd, error)
}

// DefaultTunnelReadyTimeout bounds the wait for a forward to come up. Generous
// enough for a cold SSH connection to a fresh instance (host-key exchange plus
// authentication), short enough that a broken forward fails visibly.
const DefaultTunnelReadyTimeout = 20 * time.Second

// OpenTunnel starts an SSH port-forward and waits until it is actually accepting
// connections.
//
// It returns an error rather than a half-open tunnel when the forward never comes
// up. Both pre-existing copies of this logic polled for readiness and then fell
// through to their HTTP call regardless of the result, so a forward that never
// opened surfaced as a confusing connection-refused from the request instead of
// "the tunnel didn't open, here is ssh's complaint".
//
// The caller must Close a successful tunnel.
func OpenTunnel(ctx context.Context, opts TunnelOptions) (*Tunnel, error) {
	if opts.RemoteAddr == "" {
		return nil, errors.New("service: TunnelOptions.RemoteAddr is required")
	}
	if _, _, err := net.SplitHostPort(opts.RemoteAddr); err != nil {
		return nil, fmt.Errorf("service: remote addr %q: %w", opts.RemoteAddr, err)
	}
	timeout := opts.ReadyTimeout
	if timeout <= 0 {
		timeout = DefaultTunnelReadyTimeout
	}
	dial := opts.Dial
	if dial == nil {
		dial = TCPDialer(time.Second)
	}

	localPort := opts.LocalPort
	if localPort == 0 {
		p, err := freeLocalPort()
		if err != nil {
			return nil, fmt.Errorf("service: choose local port: %w", err)
		}
		localPort = p
	}

	start := opts.Start
	if start == nil {
		if opts.Target.User == "" || opts.Target.Host == "" {
			return nil, errors.New("service: TunnelOptions.Target needs a User and Host")
		}
		start = func(ctx context.Context, localPort int, remoteAddr string) (*exec.Cmd, error) {
			return exec.CommandContext(ctx, "ssh", SSHTunnelArgs(opts.Target, localPort, remoteAddr)...), nil
		}
	}

	// The tunnel outlives this call, so it gets its own cancelable context —
	// bound to Close, not to the caller's ctx deadline.
	tunnelCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))

	cmd, err := start(tunnelCtx, localPort, opts.RemoteAddr)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("service: build tunnel command: %w", err)
	}
	// ssh's complaints ("bind: Address already in use", "Permission denied",
	// "channel setup failed") are the entire diagnosis when a forward fails, and
	// both previous copies discarded them.
	stderr := &boundedBuffer{limit: 8192}
	if cmd.Stderr == nil {
		cmd.Stderr = stderr
	}

	t := &Tunnel{
		LocalAddr:  net.JoinHostPort("127.0.0.1", strconv.Itoa(localPort)),
		RemoteAddr: opts.RemoteAddr,
		cmd:        cmd,
		stderr:     stderr,
		done:       make(chan struct{}),
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("service: start tunnel: %w", err)
	}
	// Cancel on exit so the context isn't leaked; Close cancels too.
	go func() {
		t.exitErr = cmd.Wait()
		close(t.done)
		cancel()
	}()

	if err := t.waitReady(ctx, dial, timeout); err != nil {
		_ = t.Close()
		return nil, err
	}
	return t, nil
}

// waitReady polls the local end until it accepts a connection, the tunnel
// process dies, or the deadline passes.
func (t *Tunnel) waitReady(ctx context.Context, dial Dialer, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if err := dial(ctx, t.LocalAddr); err == nil {
			return nil
		}
		select {
		case <-t.done:
			// ssh exited. ExitOnForwardFailure makes this the normal way a bad
			// forward reports itself, so its stderr is the actionable part.
			return &TunnelError{
				RemoteAddr: t.RemoteAddr,
				LocalAddr:  t.LocalAddr,
				ExitErr:    t.exitErr,
				Stderr:     t.stderr.String(),
			}
		case <-ctx.Done():
			return &TunnelError{
				RemoteAddr: t.RemoteAddr,
				LocalAddr:  t.LocalAddr,
				ExitErr:    ctx.Err(),
				Stderr:     t.stderr.String(),
			}
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return &TunnelError{
				RemoteAddr: t.RemoteAddr,
				LocalAddr:  t.LocalAddr,
				Waited:     timeout,
				Stderr:     t.stderr.String(),
			}
		}
	}
}

// TunnelError explains a forward that never came up, carrying ssh's own stderr —
// which is where the reason actually lives.
type TunnelError struct {
	RemoteAddr string
	LocalAddr  string
	// Waited is non-zero when the failure was a timeout rather than an exit.
	Waited time.Duration
	// ExitErr is the tunnel process's exit error, when it exited.
	ExitErr error
	Stderr  string
}

func (e *TunnelError) Error() string {
	var b strings.Builder
	if e.Waited > 0 {
		fmt.Fprintf(&b, "ssh tunnel %s → %s did not start accepting connections within %s",
			e.LocalAddr, e.RemoteAddr, e.Waited.Round(time.Millisecond))
	} else {
		fmt.Fprintf(&b, "ssh tunnel %s → %s exited before it was usable", e.LocalAddr, e.RemoteAddr)
		if e.ExitErr != nil {
			fmt.Fprintf(&b, " (%v)", e.ExitErr)
		}
	}
	if s := strings.TrimSpace(e.Stderr); s != "" {
		fmt.Fprintf(&b, "\nssh: %s", s)
	}
	return b.String()
}

// Close tears the tunnel down and reaps the process. Safe to call more than once
// and safe on an already-dead tunnel.
func (t *Tunnel) Close() error {
	t.stopOnce.Do(func() {
		if t.cmd.Process == nil {
			return
		}
		_ = t.cmd.Process.Kill()
		select {
		case <-t.done:
		case <-time.After(5 * time.Second):
		}
	})
	return nil
}

// Wait blocks until the tunnel process exits.
//
// A long-lived service holds a tunnel for as long as it serves, so its death is
// a user-visible event (the URL stops working) that a holding loop must be able
// to notice rather than discover by failed request.
func (t *Tunnel) Wait() error {
	<-t.done
	return t.exitErr
}

// Done is closed when the tunnel process exits, for callers that select on it
// alongside other events.
func (t *Tunnel) Done() <-chan struct{} { return t.done }

// Stderr is whatever the tunnel process wrote to stderr so far.
func (t *Tunnel) Stderr() string { return t.stderr.String() }

// SSHTunnelArgs builds the ssh argument vector for a port-forward. Pure and
// testable; no exec.
//
// Beyond the forward itself, the options here are the ones whose absence caused
// real problems elsewhere in spawn:
//
//   - ExitOnForwardFailure: without it ssh happily connects with NO forward, so
//     a port collision looks like a working tunnel until the first request.
//   - ServerAliveInterval/CountMax: a long-lived forward through a NAT or an idle
//     firewall dies silently otherwise. No forward in the tree set these, which
//     is survivable for a one-shot POST and not for a session held for a TTL.
//   - ControlMaster=no/ControlPath=none: keep spawn's SSH independent of the
//     user's connection multiplexing, so concurrent spawn commands don't
//     serialize on one shared control socket (#56) and closing a tunnel can't
//     take down an unrelated multiplexed session.
func SSHTunnelArgs(target SSHTarget, localPort int, remoteAddr string) []string {
	args := []string{
		"-N", // no remote command: this connection exists only to forward
		"-L", fmt.Sprintf("127.0.0.1:%d:%s", localPort, remoteAddr),
		"-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		"-o", "ControlMaster=no",
		"-o", "ControlPath=none",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ConnectTimeout=10",
		"-o", "BatchMode=yes",
	}
	if target.KeyPath != "" {
		args = append(args, "-i", target.KeyPath)
	}
	if target.Port != 0 && target.Port != 22 {
		args = append(args, "-p", strconv.Itoa(target.Port))
	}
	args = append(args, target.User+"@"+target.Host)
	return args
}

// freeLocalPort returns a probably-free local port by binding one and releasing
// it. Racy in principle: something else can claim the port in the gap. The
// alternative — a fixed port — collides deterministically with any second tunnel,
// which is worse, and ExitOnForwardFailure turns a lost race into a clean error
// rather than a silent misroute.
func freeLocalPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		return 0, err
	}
	return port, nil
}

// boundedBuffer is a concurrency-safe io.Writer that keeps at most limit bytes.
// Diagnostics from a subprocess are unbounded in principle; an error message is
// not a log file.
type boundedBuffer struct {
	mu    sync.Mutex
	b     strings.Builder
	limit int
}

func (w *boundedBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if room := w.limit - w.b.Len(); room > 0 {
		if len(p) > room {
			w.b.Write(p[:room])
		} else {
			w.b.Write(p)
		}
	}
	// Always report the full write: the caller is a process's stderr, and a short
	// write would be reported to it as an error on output it can't do anything
	// about.
	return len(p), nil
}

func (w *boundedBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}
