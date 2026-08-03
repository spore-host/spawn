package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Remote is a service running on a remote host, reachable through an SSH
// port-forward.
//
// The launcher SSH-execs the binary rather than starting it from user-data,
// because the readiness line has to reach THIS process: the user-data model sends
// command output to /var/log/spawn-command.log on the box, so a launcher would
// have to poll that file over SSH and parse it. Running it over an SSH session
// makes the child's stdout a plain pipe here, and makes the session's lifetime a
// reap signal.
type Remote struct {
	// Ready is the verified readiness line the remote service printed.
	Ready *ReadyEvent
	// Tunnel is the live forward to the address the service announced.
	Tunnel *Tunnel

	cmd *exec.Cmd
	// stdin is held open for the life of the service: the remote wrapper blocks
	// on it, so closing it is what tells the far side to shut down.
	stdin    io.WriteCloser
	done     chan struct{}
	exitErr  error
	stopOnce sync.Once
}

// RemoteOptions configures RunRemote.
type RemoteOptions struct {
	// Target is the host to run on and forward through. Required unless Exec is
	// set.
	Target SSHTarget
	// Command is the remote command line that starts the service. It must make
	// the service print a readiness line to stdout. Required.
	//
	// Passed to the remote login shell as-is, so the caller is responsible for
	// quoting — the same contract as `spawn connect -- <cmd>`.
	Command string
	// AddrFlagArgs, if non-empty, is appended to Command. Defaults to
	// "--addr 127.0.0.1:0", which is the point of the whole contract: the service
	// picks a free port and announces it, so nothing has to guess.
	//
	// Binding the remote loopback rather than 0.0.0.0 matters — the service may be
	// unauthenticated, and the tunnel is the only intended path to it.
	AddrFlagArgs string
	// BootTimeout bounds the readiness wait. Zero means DefaultBootTimeout.
	BootTimeout time.Duration
	// LocalPort is the local end of the forward. Zero picks a free port.
	LocalPort int
	// TunnelReadyTimeout bounds the wait for each forward attempt. Zero means
	// DefaultTunnelReadyTimeout.
	TunnelReadyTimeout time.Duration
	// Stdout, if non-nil, receives the service's stdout after the readiness line.
	Stdout io.Writer
	// Stderr, if non-nil, receives the service's stderr as it is produced. A
	// workload may print an access token here — don't wire it to anything durable
	// without considering that.
	Stderr io.Writer
	// Exec overrides how the remote command is started. Only tests should set
	// this; it is what lets the whole ladder run without a remote host.
	Exec func(ctx context.Context, command string) (*exec.Cmd, error)
	// StartTunnel overrides forward creation, for the same reason.
	StartTunnel func(ctx context.Context, localPort int, remoteAddr string) (*exec.Cmd, error)
	// Probe overrides the through-the-tunnel liveness check. Nil means an HTTP
	// probe. See RunRemote for why a bare TCP dial is not enough here.
	Probe Dialer
}

// RunRemote starts a service on a remote host, reads its readiness line over the
// SSH session, and opens a forward to the address it announced.
//
// The forgery check needs a different mechanism than the local path. A readiness
// line names an address on the REMOTE loopback, which this process cannot dial at
// all until a forward exists — so the candidate address is verified by forwarding
// to it and probing through the forward. That is also why the probe is an HTTP
// request and not a TCP connect: `ssh -L` binds the local end and accepts
// connections whether or not anything is listening on the far side, so a
// successful TCP dial to a forward proves only that ssh is running. A forged
// address therefore passes a TCP dial and fails an actual request.
//
// Candidates are tried in order, so a service that forges a readiness line from
// init() (which runs before main) loses to the real one that follows.
//
// On any failure the SSH session and any forward are torn down before returning.
// The caller must Stop a successful one. Note that Stop closes the SSH session,
// which is a reap signal, not a guarantee: the instance's TTL remains the
// backstop that actually ends the spend.
func RunRemote(ctx context.Context, opts RemoteOptions) (*Remote, error) {
	if opts.Command == "" {
		return nil, errors.New("service: RemoteOptions.Command is required")
	}
	addrArgs := opts.AddrFlagArgs
	if addrArgs == "" {
		addrArgs = "--addr 127.0.0.1:0"
	}
	remoteCmd := WrapRemoteCommand(strings.TrimSpace(opts.Command + " " + addrArgs))

	execFn := opts.Exec
	if execFn == nil {
		if opts.Target.User == "" || opts.Target.Host == "" {
			return nil, errors.New("service: RemoteOptions.Target needs a User and Host")
		}
		execFn = func(ctx context.Context, command string) (*exec.Cmd, error) {
			return exec.CommandContext(ctx, "ssh", SSHExecArgs(opts.Target, command)...), nil
		}
	}

	// The session outlives this call, so it gets a context tied to Stop rather
	// than to the caller's readiness deadline.
	sessionCtx, cancelSession := context.WithCancel(context.WithoutCancel(ctx))

	cmd, err := execFn(sessionCtx, remoteCmd)
	if err != nil {
		cancelSession()
		return nil, fmt.Errorf("service: build remote command: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancelSession()
		return nil, fmt.Errorf("service: stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		cancelSession()
		return nil, fmt.Errorf("service: stderr pipe: %w", err)
	}
	// The remote wrapper watches stdin for EOF to know the session is gone, so
	// stdin must be a pipe this process holds open — not the terminal (which would
	// steal the user's input) and not /dev/null (immediate EOF, instant shutdown).
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancelSession()
		return nil, fmt.Errorf("service: stdin pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		cancelSession()
		return nil, fmt.Errorf("service: start remote command: %w", err)
	}

	r := &Remote{cmd: cmd, done: make(chan struct{})}
	exitedC := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		r.exitErr = err
		close(r.done)
		cancelSession()
		select {
		case exitedC <- err:
		default:
		}
	}()

	var stderrForWait io.Reader = stderrPipe
	if opts.Stderr != nil {
		stderrForWait = io.TeeReader(stderrPipe, opts.Stderr)
	}
	afterReady := opts.Stdout
	if afterReady == nil {
		afterReady = io.Discard
	}

	probe := opts.Probe
	if probe == nil {
		probe = HTTPProbe(3 * time.Second)
	}

	// The "dial" for a remote candidate is: forward to it, then probe through the
	// forward. Reusing WaitForReady's candidate loop this way means the remote path
	// gets the same forgery protection as the local one, and the tunnel that
	// survives is by construction the one that was verified.
	var (
		tunMu sync.Mutex
		tun   *Tunnel
	)
	verify := func(ctx context.Context, addr string) error {
		t, err := OpenTunnel(ctx, TunnelOptions{
			Target:       opts.Target,
			RemoteAddr:   addr,
			LocalPort:    opts.LocalPort,
			ReadyTimeout: opts.TunnelReadyTimeout,
			Start:        opts.StartTunnel,
		})
		if err != nil {
			return err
		}
		if err := probe(ctx, t.LocalAddr); err != nil {
			// Nothing is serving on the far side, so this candidate is not the
			// service — drop the forward and let the next line be tried.
			_ = t.Close()
			return fmt.Errorf("nothing is serving through the forward to %s: %w", addr, err)
		}
		tunMu.Lock()
		if tun != nil {
			_ = tun.Close()
		}
		tun = t
		tunMu.Unlock()
		return nil
	}

	ev, err := WaitForReady(ctx, stdout, WaitOptions{
		Timeout:    opts.BootTimeout,
		Dial:       verify,
		Stderr:     stderrForWait,
		Exited:     exitedC,
		AfterReady: afterReady,
	})
	if err != nil {
		tunMu.Lock()
		if tun != nil {
			_ = tun.Close()
		}
		tunMu.Unlock()
		// Closing stdin asks the remote wrapper to shut the service down; killing
		// the local ssh only ends the session.
		_ = stdin.Close()
		r.shutdown()
		return nil, err
	}

	tunMu.Lock()
	r.Tunnel = tun
	tunMu.Unlock()
	r.Ready = ev
	r.stdin = stdin
	return r, nil
}

// LocalURL is the URL to hand the user: the local end of the forward, carrying
// the access token when the service requires one.
//
// This may contain a live credential — callers that log or persist must use
// Tunnel.LocalAddr instead.
func (r *Remote) LocalURL() string {
	u := "http://" + r.Tunnel.LocalAddr
	if r.Ready.Token != "" {
		u += "/?token=" + r.Ready.Token
	}
	return u
}

// Done is closed when the SSH session ends — which is how a holding loop notices
// that the service is gone without waiting for a failed request.
func (r *Remote) Done() <-chan struct{} { return r.done }

// Stop closes the forward and ends the SSH session. Safe to call more than once.
//
// Stopping the session is a request, not a guarantee: the remote wrapper kills
// the service on stdin EOF, but a wrapper that never runs (or a network partition)
// leaves the remote process alive. The instance's TTL is the guarantee — see the
// mandatory-TTL path the launching verb goes through.
func (r *Remote) Stop() error {
	r.stopOnce.Do(func() {
		if r.Tunnel != nil {
			_ = r.Tunnel.Close()
		}
		// Close stdin FIRST and give the remote wrapper a moment: that is the path
		// that actually stops the remote process. Killing ssh first would leave it
		// running until the TTL.
		if r.stdin != nil {
			_ = r.stdin.Close()
		}
		select {
		case <-r.done:
			return
		case <-time.After(5 * time.Second):
		}
		r.shutdown()
	})
	return nil
}

// shutdown kills the local session process and reaps it.
func (r *Remote) shutdown() {
	if r.cmd.Process != nil {
		_ = r.cmd.Process.Kill()
	}
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
	}
}

// WrapRemoteCommand wraps a remote command so the service dies when the SSH
// session does.
//
// This is necessary because closing an SSH session does NOT reliably kill what it
// started: without a TTY there is no SIGHUP, so a service that has already
// daemonized past its parent keeps serving — and keeps costing money — after the
// launcher exits. The wrapper runs the service in the background and blocks on
// stdin; when the session drops, stdin hits EOF and the service is signaled.
//
// The service's own stdin is redirected from /dev/null so it can't compete with
// the wrapper for the session's input.
func WrapRemoteCommand(command string) string {
	// Written as one line so it can be passed as a single remote command. `exec`
	// is deliberately absent: the shell has to outlive the service to do the
	// killing.
	return "sh -c " + shellQuote(
		command+" </dev/null & svc=$!; "+
			// Drain stdin until the session closes it.
			"cat >/dev/null; "+
			// TERM first so the service can shut down cleanly, then KILL: a
			// service that ignores TERM must not outlive its session.
			"kill -TERM $svc 2>/dev/null; "+
			"for _ in 1 2 3 4 5; do kill -0 $svc 2>/dev/null || exit 0; sleep 1; done; "+
			"kill -KILL $svc 2>/dev/null; exit 0")
}

// shellQuote single-quotes s for a POSIX shell, escaping embedded single quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// SSHExecArgs builds the ssh argument vector for running a command on the remote
// host with its stdout streamed back. Pure and testable; no exec.
//
// The command is appended last, as the caller's already-quoted string — the same
// contract as `spawn connect -- <cmd>`.
func SSHExecArgs(target SSHTarget, command string) []string {
	args := []string{
		// No TTY: a pseudo-terminal would merge stderr into stdout and translate
		// newlines, which corrupts the one line this whole contract depends on.
		"-T",
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
	args = append(args, target.User+"@"+target.Host, command)
	return args
}

// HTTPProbe verifies that something is actually serving HTTP at addr.
//
// It exists because a TCP dial cannot verify a forwarded port: `ssh -L` accepts
// connections on the local end regardless of whether the far side is listening,
// so a bare connect succeeds against a forward to nothing. Sending a request and
// requiring a response distinguishes them.
//
// Any HTTP response counts, including 401 and 404: the question is whether a
// server is there, not whether the caller is authorized or the path exists.
func HTTPProbe(timeout time.Duration) Dialer {
	return func(ctx context.Context, addr string) error {
		d := net.Dialer{Timeout: timeout}
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close() }()
		if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
			return err
		}
		// HTTP/1.0 so the server closes the connection itself and we never wait on
		// keep-alive. GET rather than HEAD because some servers handle HEAD poorly
		// and a probe should not be the thing that discovers that.
		if _, err := conn.Write([]byte("GET / HTTP/1.0\r\nHost: " + addr + "\r\nConnection: close\r\n\r\n")); err != nil {
			return fmt.Errorf("write probe request: %w", err)
		}
		buf := make([]byte, 12)
		n, err := io.ReadFull(conn, buf)
		if n == 0 {
			if err == nil {
				err = io.EOF
			}
			return fmt.Errorf("no response to an HTTP request (the forward accepted the connection but nothing answered): %w", err)
		}
		if !strings.HasPrefix(string(buf[:n]), "HTTP/") {
			return fmt.Errorf("response is not HTTP: %q", string(buf[:n]))
		}
		return nil
	}
}
