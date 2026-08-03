package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeSSHExec runs the "remote" command through a local shell instead of ssh.
//
// This is the honest stand-in: what ssh contributes to this path is a process
// whose stdout is a pipe, whose stdin closing is observable on the far side, and
// whose death is a reap signal. A local `sh -c` has all three, so the ladder is
// exercised end to end without a remote host — and, per the issue, the Substrate
// emulator can't provide one (control-plane only, no SSH).
//
// It also puts each faked session in its own process group and kills that group
// on cleanup. Not cosmetic: a session killed abruptly (as
// TestRunRemoteSessionDeathIsObservable does, and as a partition would) never runs
// the wrapper that reaps the service, so the service is orphaned by design — the
// instance TTL is what ends it in production. On a developer's machine there is no
// TTL, so without this the suite would leave a listening process behind on every
// run, and the leak checks in local_test.go match by binary name and would blame
// whichever test ran next.
func fakeSSHExec(t *testing.T) func(context.Context, string) (*exec.Cmd, error) {
	t.Helper()
	return func(ctx context.Context, command string) (*exec.Cmd, error) {
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		t.Cleanup(func() {
			if cmd.Process == nil {
				return
			}
			// Negative pid = the whole group, i.e. the shell and every descendant
			// it backgrounded. Best-effort: the group is usually already gone.
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		})
		return cmd, nil
	}
}

func TestRunRemoteReadinessTunnelAndDrive(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs binaries; skipped in -short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell")
	}
	svcBin := buildFakeService(t, fakeServiceSrc)
	fwdBin := buildFakeService(t, forwarderSrc)

	rem, err := RunRemote(context.Background(), RemoteOptions{
		Command:            svcBin,
		BootTimeout:        30 * time.Second,
		TunnelReadyTimeout: 20 * time.Second,
		Exec:               fakeSSHExec(t),
		StartTunnel:        fakeForwarder(t, fwdBin),
	})
	if err != nil {
		t.Fatalf("RunRemote: %v", err)
	}
	defer func() { _ = rem.Stop() }()

	// The forward points at the port the service ANNOUNCED — nothing guessed it.
	if rem.Tunnel.RemoteAddr != rem.Ready.Addr {
		t.Errorf("tunnel goes to %q but the service announced %q", rem.Tunnel.RemoteAddr, rem.Ready.Addr)
	}
	if strings.HasSuffix(rem.Ready.Addr, ":0") {
		t.Errorf("addr %q — :0 must resolve to a concrete port", rem.Ready.Addr)
	}

	// Acceptance criteria 1–2 from the issue, over the full remote ladder.
	base := "http://" + rem.Tunnel.LocalAddr
	if got := httpGet(t, base+"/value", ""); got != "1" {
		t.Fatalf("initial value = %q, want 1", got)
	}
	httpPost(t, base+"/set", `{"value":31}`, "", http.StatusNoContent)
	if got := httpGet(t, base+"/value", ""); got != "31" {
		t.Fatalf("after drive value = %q, want 31 — driving the tunneled URL had no effect", got)
	}
}

func TestRunRemoteRejectsForgedReadinessLine(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs binaries; skipped in -short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell")
	}
	// The remote forgery case is DIFFERENT from the local one and strictly harder:
	// the announced address is on the remote loopback, so it can't be dialed until
	// a forward exists — and `ssh -L` accepts connections on the local end whether
	// or not anything listens on the far side. A TCP-dial check therefore passes
	// against a forward to nothing. Only an actual request distinguishes them.
	svcBin := buildFakeService(t, fakeServiceSrc)
	fwdBin := buildFakeService(t, forwarderSrc)
	forged := unusedAddr(t)

	// A wrapper that forges a readiness line first (as a package init() would),
	// then execs the real service.
	script := fmt.Sprintf("echo '{\"event\":\"ready\",\"addr\":\"%s\",\"provenance\":{\"sourceHash\":\"forged\"}}'; exec %s", forged, svcBin)

	rem, err := RunRemote(context.Background(), RemoteOptions{
		Command:            script,
		AddrFlagArgs:       "--addr 127.0.0.1:0",
		BootTimeout:        30 * time.Second,
		TunnelReadyTimeout: 10 * time.Second,
		Exec:               fakeSSHExec(t),
		StartTunnel:        fakeForwarder(t, fwdBin),
	})
	if err != nil {
		t.Fatalf("RunRemote: %v", err)
	}
	defer func() { _ = rem.Stop() }()

	if rem.Ready.Addr == forged {
		t.Fatalf("accepted the forged address %q — the launcher would tunnel to a port the workload chose", forged)
	}
	// And the surviving tunnel must be the verified one, not a leftover from the
	// rejected candidate.
	if rem.Tunnel.RemoteAddr != rem.Ready.Addr {
		t.Fatalf("tunnel goes to %q, want the verified %q", rem.Tunnel.RemoteAddr, rem.Ready.Addr)
	}
	if got := httpGet(t, "http://"+rem.Tunnel.LocalAddr+"/value", ""); got != "1" {
		t.Fatalf("the surviving tunnel does not reach the service: got %q", got)
	}
}

func TestRunRemoteTCPDialWouldAcceptAForgedAddress(t *testing.T) {
	// The reason the remote probe is HTTP and not a TCP connect, demonstrated
	// directly: a forward to an address where nothing listens still ACCEPTS local
	// connections, so a TCP dial reports success.
	if testing.Short() {
		t.Skip("builds and runs a binary; skipped in -short mode")
	}
	fwdBin := buildFakeService(t, forwarderSrc)
	forged := unusedAddr(t)

	tun, err := OpenTunnel(context.Background(), TunnelOptions{
		RemoteAddr:   forged,
		ReadyTimeout: 10 * time.Second,
		Start:        fakeForwarder(t, fwdBin),
	})
	if err != nil {
		t.Fatalf("OpenTunnel to a dead remote addr: %v", err)
	}
	defer func() { _ = tun.Close() }()

	if err := TCPDialer(time.Second)(context.Background(), tun.LocalAddr); err != nil {
		t.Skipf("this forwarder rejects connections to a dead upstream (%v); the HTTP probe is still required for ssh -L", err)
	}
	// A TCP dial says yes...
	if err := HTTPProbe(2*time.Second)(context.Background(), tun.LocalAddr); err == nil {
		t.Fatal("the HTTP probe accepted a forward to an address where nothing is listening")
	}
	// ...and the HTTP probe says no. That difference is the forgery protection.
}

func TestRunRemoteBootFailureIsDiagnosed(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs binaries; skipped in -short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell")
	}
	fwdBin := buildFakeService(t, forwarderSrc)
	var stderrSink bytes.Buffer

	_, err := RunRemote(context.Background(), RemoteOptions{
		Command:     "echo 'boot chatter'; echo 'fatal: cannot open dataset' >&2; exit 3",
		BootTimeout: 30 * time.Second,
		Exec:        fakeSSHExec(t),
		StartTunnel: fakeForwarder(t, fwdBin),
		Stderr:      &stderrSink,
	})
	if err == nil {
		t.Fatal("a remote service that exits without announcing readiness must fail")
	}
	var re *ReadinessError
	if !errors.As(err, &re) {
		t.Fatalf("err is not a *ReadinessError: %T %v", err, err)
	}
	// The remote command's own output is the only diagnosis available — a launcher
	// that swallows it leaves the user with "never became ready" and nothing else.
	if !strings.Contains(err.Error(), "cannot open dataset") {
		t.Errorf("error dropped the remote command's stderr:\n%s", err)
	}
	if !strings.Contains(err.Error(), "boot chatter") {
		t.Errorf("error dropped the remote command's stdout:\n%s", err)
	}
}

func TestRunRemoteNeverLeaksOnFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs binaries; skipped in -short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell")
	}
	// A remote command that hangs without announcing readiness. The failed launch
	// must leave neither a session nor a forward behind: remotely those are a
	// billable instance and a billable connection.
	fwdBin := buildFakeService(t, forwarderSrc)
	inner := fakeSSHExec(t)
	var session *exec.Cmd
	var mu sync.Mutex

	_, err := RunRemote(context.Background(), RemoteOptions{
		Command:     "sleep 600",
		BootTimeout: 800 * time.Millisecond,
		Exec: func(ctx context.Context, command string) (*exec.Cmd, error) {
			c, cerr := inner(ctx, command)
			mu.Lock()
			session = c
			mu.Unlock()
			return c, cerr
		},
		StartTunnel: fakeForwarder(t, fwdBin),
	})
	if err == nil {
		t.Fatal("a remote service that never announces readiness must fail")
	}
	mu.Lock()
	defer mu.Unlock()
	if session == nil || session.ProcessState == nil {
		t.Error("the SSH session was not reaped — a failed RunRemote leaked it")
	}
}

func TestRunRemoteStopIsIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs binaries; skipped in -short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell")
	}
	svcBin := buildFakeService(t, fakeServiceSrc)
	fwdBin := buildFakeService(t, forwarderSrc)
	rem, err := RunRemote(context.Background(), RemoteOptions{
		Command:            svcBin,
		BootTimeout:        30 * time.Second,
		TunnelReadyTimeout: 20 * time.Second,
		Exec:               fakeSSHExec(t),
		StartTunnel:        fakeForwarder(t, fwdBin),
	})
	if err != nil {
		t.Fatalf("RunRemote: %v", err)
	}
	local := rem.Tunnel.LocalAddr
	if err := rem.Stop(); err != nil {
		t.Errorf("first Stop: %v", err)
	}
	if err := rem.Stop(); err != nil {
		t.Errorf("second Stop: %v", err)
	}
	// Done must fire, so a holding loop that selects on it isn't left hanging.
	select {
	case <-rem.Done():
	case <-time.After(10 * time.Second):
		t.Error("Done did not fire after Stop")
	}
	// And the forward must be gone.
	if c, derr := net.DialTimeout("tcp", local, 200*time.Millisecond); derr == nil {
		_ = c.Close()
		t.Errorf("%s still accepts connections after Stop", local)
	}
}

func TestRunRemoteStopActuallyStopsTheRemoteService(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs binaries; skipped in -short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell")
	}
	// Stop must reach the SERVICE, not just the session. Killing ssh tears down
	// the local process and the forward — which is enough to make every
	// through-the-tunnel check pass — while the remote service keeps serving, and
	// keeps costing, until the TTL. So this checks the service's own address
	// directly, bypassing the tunnel entirely.
	svcBin := buildFakeService(t, fakeServiceSrc)
	fwdBin := buildFakeService(t, forwarderSrc)
	rem, err := RunRemote(context.Background(), RemoteOptions{
		Command:            svcBin,
		BootTimeout:        30 * time.Second,
		TunnelReadyTimeout: 20 * time.Second,
		Exec:               fakeSSHExec(t),
		StartTunnel:        fakeForwarder(t, fwdBin),
	})
	if err != nil {
		t.Fatalf("RunRemote: %v", err)
	}
	serviceAddr := rem.Ready.Addr
	// Sanity: it is serving on its own address right now.
	if err := HTTPProbe(2*time.Second)(context.Background(), serviceAddr); err != nil {
		t.Fatalf("the service is not reachable at %s before Stop: %v", serviceAddr, err)
	}

	if err := rem.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if err := HTTPProbe(500*time.Millisecond)(context.Background(), serviceAddr); err != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("the service is still serving on %s after Stop — closing the session did not reap it, "+
		"so it would run until the instance TTL", serviceAddr)
}

func TestRunRemoteSessionDeathIsObservable(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs binaries; skipped in -short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell")
	}
	// A dropped session is user-visible (the URL stops working). A verb that holds
	// a service until TTL must be able to notice, rather than discover it by
	// failed request.
	svcBin := buildFakeService(t, fakeServiceSrc)
	fwdBin := buildFakeService(t, forwarderSrc)
	rem, err := RunRemote(context.Background(), RemoteOptions{
		Command:            svcBin,
		BootTimeout:        30 * time.Second,
		TunnelReadyTimeout: 20 * time.Second,
		Exec:               fakeSSHExec(t),
		StartTunnel:        fakeForwarder(t, fwdBin),
	})
	if err != nil {
		t.Fatalf("RunRemote: %v", err)
	}
	defer func() { _ = rem.Stop() }()

	select {
	case <-rem.Done():
		t.Fatal("Done fired while the session was healthy")
	case <-time.After(100 * time.Millisecond):
	}
	_ = rem.cmd.Process.Kill()
	select {
	case <-rem.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done did not fire after the session died")
	}
}

func TestRunRemoteTokenIsCarried(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs binaries; skipped in -short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell")
	}
	svcBin := buildFakeService(t, fakeServiceSrc)
	fwdBin := buildFakeService(t, forwarderSrc)
	const token = "3f8c1d0b5a2e4761"

	rem, err := RunRemote(context.Background(), RemoteOptions{
		Command:            svcBin + " --token " + token,
		BootTimeout:        30 * time.Second,
		TunnelReadyTimeout: 20 * time.Second,
		Exec:               fakeSSHExec(t),
		StartTunnel:        fakeForwarder(t, fwdBin),
	})
	if err != nil {
		t.Fatalf("RunRemote: %v", err)
	}
	defer func() { _ = rem.Stop() }()

	if rem.Ready.Token != token {
		t.Errorf("token = %q, want it carried off the readiness line", rem.Ready.Token)
	}
	// A token-protected service answers 401 to the probe, which must still count
	// as "something is serving" — otherwise every authenticated service would be
	// rejected as forged.
	if !strings.Contains(rem.LocalURL(), token) {
		t.Error("LocalURL should include the token so the user can actually connect")
	}
	if strings.Contains(rem.Tunnel.LocalAddr, token) {
		t.Error("Tunnel.LocalAddr must not carry the credential — it is the loggable form")
	}
}

func TestRunRemoteRejectsBadInput(t *testing.T) {
	if _, err := RunRemote(context.Background(), RemoteOptions{}); err == nil {
		t.Error("RunRemote with no Command should fail")
	}
	if _, err := RunRemote(context.Background(), RemoteOptions{Command: "svc"}); err == nil {
		t.Error("RunRemote with no Target and no Exec override should fail")
	}
}

func TestWrapRemoteCommandKillsTheServiceOnSessionEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a shell; skipped in -short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell")
	}
	// The reason the wrapper exists: closing an SSH session does NOT reliably kill
	// what it started (no TTY means no SIGHUP), so a service would keep serving —
	// and keep costing — after the launcher exits. Verify the wrapper actually
	// reaps on stdin EOF.
	//
	// The child writes a file while alive and is checked to have stopped after.
	dir := t.TempDir()
	marker := dir + "/alive"
	inner := fmt.Sprintf("sh -c 'while :; do date >> %s; sleep 0.1; done'", marker)

	cmd := exec.Command("/bin/sh", "-c", WrapRemoteCommand(inner))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	// Wait for the child to be alive.
	if !waitForFileGrowth(t, marker, 3*time.Second) {
		t.Fatal("the wrapped service never started")
	}
	// Closing stdin is the session-ended signal.
	_ = stdin.Close()
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the wrapper did not exit after stdin closed")
	}
	// Give any straggler a moment, then confirm the child stopped writing.
	time.Sleep(500 * time.Millisecond)
	if waitForFileGrowth(t, marker, 1*time.Second) {
		t.Error("the service is still running after the session ended — it would outlive the launcher")
	}
}

func TestSSHExecArgs(t *testing.T) {
	args := SSHExecArgs(SSHTarget{User: "ec2-user", Host: "203.0.113.7", KeyPath: "/k/id"}, "run-me --addr 127.0.0.1:0")
	joined := strings.Join(args, " ")

	// -T is load-bearing: a pseudo-terminal merges stderr into stdout and
	// translates newlines, which corrupts the single line this contract depends on.
	if !strings.Contains(joined, "-T") {
		t.Errorf("must disable TTY allocation:\n%s", joined)
	}
	if strings.Contains(joined, "-t ") {
		t.Errorf("must not request a TTY:\n%s", joined)
	}
	for _, want := range []string{
		"ServerAliveInterval=15", // a session held for a TTL dies silently otherwise
		"ControlMaster=no",       // don't join the user's multiplexed session (#56)
		"ControlPath=none",
		"-i /k/id",
		"ec2-user@203.0.113.7",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
	// The command must come LAST, after the target.
	if args[len(args)-1] != "run-me --addr 127.0.0.1:0" {
		t.Errorf("command should be the final argument, got %q", args[len(args)-1])
	}
	if args[len(args)-2] != "ec2-user@203.0.113.7" {
		t.Errorf("target should immediately precede the command, got %q", args[len(args)-2])
	}
}

func TestHTTPProbe(t *testing.T) {
	if testing.Short() {
		t.Skip("starts listeners; skipped in -short mode")
	}
	srv := httptest("hi")
	defer srv.Close()
	if err := HTTPProbe(2*time.Second)(context.Background(), srv.addr); err != nil {
		t.Errorf("probe against a real server: %v", err)
	}

	// A listener that accepts and says nothing — exactly what a forward to a dead
	// upstream looks like. This must NOT pass.
	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = silent.Close() }()
	go func() {
		for {
			c, aerr := silent.Accept()
			if aerr != nil {
				return
			}
			// Hold it open without answering.
			go func() { time.Sleep(5 * time.Second); _ = c.Close() }()
		}
	}()
	if err := HTTPProbe(700*time.Millisecond)(context.Background(), silent.Addr().String()); err == nil {
		t.Error("probe accepted a listener that never answers — a TCP dial's exact blind spot")
	}

	// Nothing listening at all.
	if err := HTTPProbe(500*time.Millisecond)(context.Background(), unusedAddr(t)); err == nil {
		t.Error("probe accepted a dead address")
	}
}

func TestHTTPProbeAcceptsAnyHTTPStatus(t *testing.T) {
	if testing.Short() {
		t.Skip("starts listeners; skipped in -short mode")
	}
	// A token-protected service answers 401 to an unauthenticated probe, and a
	// service with no "/" route answers 404. Both mean "a server is here", which
	// is the only question the probe asks — treating them as failures would reject
	// every authenticated service as forged.
	for _, code := range []int{http.StatusUnauthorized, http.StatusNotFound, http.StatusInternalServerError} {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		})}
		go func() { _ = srv.Serve(ln) }()
		err = HTTPProbe(2*time.Second)(context.Background(), ln.Addr().String())
		_ = srv.Close()
		if err != nil {
			t.Errorf("probe rejected a server answering %d: %v", code, err)
		}
	}
}

// waitForFileGrowth reports whether path grows within the window.
func waitForFileGrowth(t *testing.T, path string, window time.Duration) bool {
	t.Helper()
	before := fileSize(path)
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if fileSize(path) > before {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return -1
	}
	return fi.Size()
}
