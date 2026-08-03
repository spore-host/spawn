package service

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// forwarderSrc is a stand-in for `ssh -N -L local:remote`: it forwards a local
// port to a remote address and stays alive until killed.
//
// A real forwarding process rather than a stub, because the behaviors that matter
// are exactly the process-shaped ones — the forward isn't usable the instant the
// process starts, killing the process must stop the forward, and a forward that
// can't bind must exit rather than pretend.
const forwarderSrc = `package main

import (
	"flag"
	"io"
	"log"
	"net"
	"os"
	"time"
)

func main() {
	local := flag.String("local", "", "local addr")
	remote := flag.String("remote", "", "remote addr")
	delay := flag.Duration("delay", 0, "wait before binding")
	failBind := flag.Bool("fail-bind", false, "exit nonzero instead of binding")
	flag.Parse()

	if *failBind {
		log.Fatal("channel setup failed: administratively prohibited")
	}
	time.Sleep(*delay)
	ln, err := net.Listen("tcp", *local)
	if err != nil {
		log.Fatalf("bind: %v", err)
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			os.Exit(0)
		}
		go func() {
			defer c.Close()
			up, err := net.Dial("tcp", *remote)
			if err != nil {
				return
			}
			defer up.Close()
			go func() { _, _ = io.Copy(up, c) }()
			_, _ = io.Copy(c, up)
		}()
	}
}
`

// fakeForwarder returns a TunnelOptions.Start that runs the forwarder fixture.
func fakeForwarder(t *testing.T, bin string, extra ...string) func(context.Context, int, string) (*exec.Cmd, error) {
	t.Helper()
	return func(ctx context.Context, localPort int, remoteAddr string) (*exec.Cmd, error) {
		args := append([]string{
			"-local", net.JoinHostPort("127.0.0.1", strconv.Itoa(localPort)),
			"-remote", remoteAddr,
		}, extra...)
		// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command -- test fixture; bin is a binary this test just built into t.TempDir(), args are loopback addresses. No shell.
		return exec.CommandContext(ctx, bin, args...), nil
	}
}

func TestOpenTunnelForwardsTraffic(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a binary; skipped in -short mode")
	}
	// A real HTTP server on the "remote" side, so success means an actual request
	// crossed the forward — not just that a port was open.
	srv := httptest("remote-ok")
	defer srv.Close()

	bin := buildFakeService(t, forwarderSrc)
	tun, err := OpenTunnel(context.Background(), TunnelOptions{
		RemoteAddr:   srv.addr,
		ReadyTimeout: 20 * time.Second,
		Start:        fakeForwarder(t, bin),
	})
	if err != nil {
		t.Fatalf("OpenTunnel: %v", err)
	}
	defer func() { _ = tun.Close() }()

	// The local port must be resolved, not the 0 we asked for implicitly.
	_, port, err := net.SplitHostPort(tun.LocalAddr)
	if err != nil {
		t.Fatalf("LocalAddr %q: %v", tun.LocalAddr, err)
	}
	if port == "0" {
		t.Fatalf("LocalAddr %q — the local port must be resolved", tun.LocalAddr)
	}
	if got := httpGet(t, "http://"+tun.LocalAddr+"/", ""); got != "remote-ok" {
		t.Fatalf("through the tunnel = %q, want remote-ok", got)
	}
}

func TestOpenTunnelWaitsForTheForwardToOpen(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a binary; skipped in -short mode")
	}
	// The forward is NOT usable when the process starts — ssh has to connect and
	// authenticate first. Returning at Start would hand back a tunnel whose first
	// request fails with connection-refused.
	srv := httptest("late-ok")
	defer srv.Close()

	bin := buildFakeService(t, forwarderSrc)
	tun, err := OpenTunnel(context.Background(), TunnelOptions{
		RemoteAddr:   srv.addr,
		ReadyTimeout: 20 * time.Second,
		Start:        fakeForwarder(t, bin, "-delay", "600ms"),
	})
	if err != nil {
		t.Fatalf("OpenTunnel: %v", err)
	}
	defer func() { _ = tun.Close() }()

	// No sleep here on purpose: OpenTunnel must not return until the forward is
	// actually accepting.
	if got := httpGet(t, "http://"+tun.LocalAddr+"/", ""); got != "late-ok" {
		t.Fatalf("first request through a slow-to-open tunnel = %q, want late-ok", got)
	}
}

func TestOpenTunnelFailsWhenTheForwardNeverOpens(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a binary; skipped in -short mode")
	}
	// Both pre-existing copies of this logic polled for readiness and then called
	// their HTTP endpoint regardless of the outcome, so a forward that never
	// opened surfaced as connection-refused from the request instead of the real
	// reason. This must fail, and it must carry ssh's own complaint.
	srv := httptest("unreachable")
	defer srv.Close()

	bin := buildFakeService(t, forwarderSrc)
	_, err := OpenTunnel(context.Background(), TunnelOptions{
		RemoteAddr:   srv.addr,
		ReadyTimeout: 10 * time.Second,
		Start:        fakeForwarder(t, bin, "-fail-bind"),
	})
	if err == nil {
		t.Fatal("a forward that fails to open must be an error, not a half-open tunnel")
	}
	var te *TunnelError
	if !errors.As(err, &te) {
		t.Fatalf("err is not a *TunnelError: %T %v", err, err)
	}
	if !strings.Contains(err.Error(), "administratively prohibited") {
		t.Errorf("error dropped the forwarder's own diagnosis, which is the whole reason:\n%s", err)
	}
}

func TestOpenTunnelTimesOutOnAHungForward(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a binary; skipped in -short mode")
	}
	// A forward that never binds but never exits either — a hung SSH connect. The
	// only detector is the deadline.
	srv := httptest("never")
	defer srv.Close()

	bin := buildFakeService(t, forwarderSrc)
	// Capture the process so the leak check is exact. pgrep can't distinguish this
	// forwarder from another test's — every fixture builds to the same name — and
	// the cost analog of a leaked forward is a billable SSH session.
	inner := fakeForwarder(t, bin, "-delay", "60s")
	var forwarder *exec.Cmd
	start := time.Now()
	_, err := OpenTunnel(context.Background(), TunnelOptions{
		RemoteAddr:   srv.addr,
		ReadyTimeout: 700 * time.Millisecond,
		Start: func(ctx context.Context, localPort int, remoteAddr string) (*exec.Cmd, error) {
			c, err := inner(ctx, localPort, remoteAddr)
			forwarder = c
			return c, err
		},
	})
	if err == nil {
		t.Fatal("a forward that never opens must time out")
	}
	var te *TunnelError
	if !errors.As(err, &te) {
		t.Fatalf("err is not a *TunnelError: %T %v", err, err)
	}
	if te.Waited == 0 {
		t.Error("a timeout should record how long it waited, to distinguish it from an exit")
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Errorf("took %s — the ready timeout should bound this", elapsed)
	}
	// A failed OpenTunnel must have reaped the process before returning: the
	// caller got no Tunnel, so nothing else can ever clean it up.
	if forwarder == nil || forwarder.ProcessState == nil {
		t.Error("the forwarder was not reaped — a failed OpenTunnel leaked a process")
	}
}

func TestTunnelCloseStopsTheForward(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a binary; skipped in -short mode")
	}
	srv := httptest("closing")
	defer srv.Close()

	bin := buildFakeService(t, forwarderSrc)
	tun, err := OpenTunnel(context.Background(), TunnelOptions{
		RemoteAddr:   srv.addr,
		ReadyTimeout: 20 * time.Second,
		Start:        fakeForwarder(t, bin),
	})
	if err != nil {
		t.Fatalf("OpenTunnel: %v", err)
	}
	local := tun.LocalAddr

	if err := tun.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	// Idempotent: a teardown that errors on "already gone" is one people stop
	// trusting, and the holding loop calls it from more than one path.
	if err := tun.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	// Wait must return rather than hang, for callers that Close then Wait.
	waited := make(chan struct{})
	go func() { _ = tun.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after Close")
	}

	// The forward must actually be gone — a Close that only detaches leaves a
	// process holding a port and, remotely, a billable session.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", local, 100*time.Millisecond)
		if err != nil {
			return
		}
		_ = c.Close()
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("%s is still accepting connections after Close", local)
}

func TestTunnelDoneFiresWhenTheForwardDies(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a binary; skipped in -short mode")
	}
	// A tunnel dying is user-visible (the URL stops working), so a loop holding a
	// service until TTL has to be able to notice it rather than find out by
	// failed request.
	srv := httptest("dies")
	defer srv.Close()

	bin := buildFakeService(t, forwarderSrc)
	tun, err := OpenTunnel(context.Background(), TunnelOptions{
		RemoteAddr:   srv.addr,
		ReadyTimeout: 20 * time.Second,
		Start:        fakeForwarder(t, bin),
	})
	if err != nil {
		t.Fatalf("OpenTunnel: %v", err)
	}
	defer func() { _ = tun.Close() }()

	select {
	case <-tun.Done():
		t.Fatal("Done fired while the tunnel was healthy")
	case <-time.After(100 * time.Millisecond):
	}

	_ = tun.cmd.Process.Kill()
	select {
	case <-tun.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done did not fire after the forward died")
	}
}

func TestOpenTunnelRejectsBadInput(t *testing.T) {
	tests := []struct {
		name string
		opts TunnelOptions
	}{
		{"no remote addr", TunnelOptions{Target: SSHTarget{User: "u", Host: "h"}}},
		{"remote addr without a port", TunnelOptions{
			RemoteAddr: "127.0.0.1",
			Target:     SSHTarget{User: "u", Host: "h"},
		}},
		{"no ssh target and no Start override", TunnelOptions{RemoteAddr: "127.0.0.1:8080"}},
		{"target missing a user", TunnelOptions{
			RemoteAddr: "127.0.0.1:8080",
			Target:     SSHTarget{Host: "h"},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := OpenTunnel(context.Background(), tc.opts); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestSSHTunnelArgs(t *testing.T) {
	args := SSHTunnelArgs(SSHTarget{User: "ec2-user", Host: "203.0.113.7", KeyPath: "/k/id_rsa"}, 51234, "127.0.0.1:54321")
	joined := strings.Join(args, " ")

	// The forward itself: local port and remote addr are both dynamic, which is
	// the whole change from the hardcoded `-L 7777:127.0.0.1:7777` copies.
	if !strings.Contains(joined, "-L 127.0.0.1:51234:127.0.0.1:54321") {
		t.Errorf("forward spec wrong:\n%s", joined)
	}
	// Binding the local end to 127.0.0.1 rather than all interfaces: the service
	// may be unauthenticated, and a 0.0.0.0 forward would publish it to the LAN.
	if strings.Contains(joined, "-L 51234:") {
		t.Errorf("local end must be bound to 127.0.0.1, not every interface:\n%s", joined)
	}
	required := []string{
		"-N",                       // forward only, no remote shell
		"ExitOnForwardFailure=yes", // else ssh connects with no forward at all
		"ServerAliveInterval=15",   // a held forward dies silently otherwise
		"ServerAliveCountMax=3",    //
		"ControlMaster=no",         // don't join the user's multiplexed session (#56)
		"ControlPath=none",         //
		"-i /k/id_rsa",             //
		"ec2-user@203.0.113.7",     //
	}
	for _, want := range required {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
	// Default SSH port must not produce a -p flag (noise, and it overrides a
	// user's ssh_config Port for that host).
	if strings.Contains(joined, "-p ") {
		t.Errorf("port 22 should not emit -p:\n%s", joined)
	}
	withPort := strings.Join(SSHTunnelArgs(SSHTarget{User: "u", Host: "h", Port: 2222}, 1, "127.0.0.1:2"), " ")
	if !strings.Contains(withPort, "-p 2222") {
		t.Errorf("a non-default port should emit -p:\n%s", withPort)
	}
	// No key: rely on the ambient setup rather than passing -i "".
	noKey := strings.Join(SSHTunnelArgs(SSHTarget{User: "u", Host: "h"}, 1, "127.0.0.1:2"), " ")
	if strings.Contains(noKey, "-i") {
		t.Errorf("no key should mean no -i flag:\n%s", noKey)
	}
}

func TestBoundedBufferCapsAndReportsFullWrites(t *testing.T) {
	b := &boundedBuffer{limit: 10}
	// A short write would be reported to a subprocess's stderr as an I/O error on
	// output it can do nothing about.
	n, err := b.Write([]byte(strings.Repeat("x", 100)))
	if err != nil || n != 100 {
		t.Errorf("Write = (%d, %v), want (100, nil)", n, err)
	}
	if got := b.String(); len(got) != 10 {
		t.Errorf("kept %d bytes, want the 10-byte cap", len(got))
	}
	if n, err := b.Write([]byte("more")); err != nil || n != 4 {
		t.Errorf("Write past the cap = (%d, %v), want (4, nil)", n, err)
	}
}

func TestFreeLocalPort(t *testing.T) {
	p, err := freeLocalPort()
	if err != nil {
		t.Fatalf("freeLocalPort: %v", err)
	}
	if p <= 0 || p > 65535 {
		t.Fatalf("port %d out of range", p)
	}
	// It must be free: the point is to hand a bindable port to the forward.
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
	if err != nil {
		t.Fatalf("port %d was not bindable: %v", p, err)
	}
	_ = ln.Close()
}

// TestServiceOverTunnelEndToEnd is acceptance criteria 1–2 from the issue at $0:
// launch the service, read its readiness line, forward to the port it actually
// bound, and drive it through the forward.
func TestServiceOverTunnelEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs binaries; skipped in -short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("process signaling differs on Windows")
	}
	svcBin := buildFakeService(t, fakeServiceSrc)
	fwdBin := buildFakeService(t, forwarderSrc)

	svc, err := RunLocal(context.Background(), LocalOptions{Path: svcBin, BootTimeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("RunLocal: %v", err)
	}
	defer func() { _ = svc.Stop() }()

	// The remote addr is the one the service ANNOUNCED — nothing guessed it, and
	// nothing could have: it was assigned by the OS at bind time.
	tun, err := OpenTunnel(context.Background(), TunnelOptions{
		RemoteAddr:   svc.Addr(),
		ReadyTimeout: 20 * time.Second,
		Start:        fakeForwarder(t, fwdBin),
	})
	if err != nil {
		t.Fatalf("OpenTunnel: %v", err)
	}
	defer func() { _ = tun.Close() }()

	base := "http://" + tun.LocalAddr
	if got := httpGet(t, base+"/value", ""); got != "1" {
		t.Fatalf("initial value through the tunnel = %q, want 1", got)
	}
	httpPost(t, base+"/set", `{"value":99}`, "", http.StatusNoContent)
	if got := httpGet(t, base+"/value", ""); got != "99" {
		t.Fatalf("after drive value = %q, want 99 — driving through the tunnel had no effect", got)
	}
}

// --- helpers ---

// httptest is a minimal fixed-response HTTP server on loopback. (Named to read
// like the stdlib's, but local: net/http/httptest is fine too, this just keeps
// the body a single constant.)
type testServer struct {
	addr string
	srv  *http.Server
	ln   net.Listener
}

func httptest(body string) *testServer {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Write, not Fprint: the body is a fixture string, and there is nothing
		// here to format.
		_, _ = w.Write([]byte(body))
	})}
	go func() { _ = srv.Serve(ln) }()
	return &testServer{addr: ln.Addr().String(), srv: srv, ln: ln}
}

func (s *testServer) Close() {
	_ = s.srv.Close()
	_ = s.ln.Close()
}
