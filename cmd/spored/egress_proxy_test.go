package main

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/agent"
	"github.com/spore-host/spawn/pkg/egressproxy"
	"github.com/spore-host/spawn/pkg/provider"
)

type fakeIdleSetter struct {
	mu   sync.Mutex
	cfg  *provider.Config
	idle *agent.ExternalIdle
}

func (f *fakeIdleSetter) SetExternalIdle(e agent.ExternalIdle) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.idle = &e
}

func (f *fakeIdleSetter) GetConfig() *provider.Config { return f.cfg }

func (f *fakeIdleSetter) get() *agent.ExternalIdle {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.idle
}

// startMode runs the mode on a loopback listener and returns where it listens.
func startMode(t *testing.T, m *egressProxyMode, ag *fakeIdleSetter) (addr string, stop func()) {
	t.Helper()
	bound := make(chan string, 1)
	m.listenFunc = func(network, _ string) (net.Listener, error) {
		ln, err := net.Listen(network, "127.0.0.1:0")
		if err == nil {
			bound <- ln.Addr().String()
		}
		return ln, err
	}
	m.retryWait = 10 * time.Millisecond
	m.persist = 10 * time.Millisecond
	stop = m.start(context.Background(), ag, &provider.Identity{PrivateIP: "10.0.1.23"})
	select {
	case addr = <-bound:
	case <-time.After(5 * time.Second):
		t.Fatal("the proxy never bound")
	}
	return addr, stop
}

func TestEgressProxyCommandContract(t *testing.T) {
	cmd := findCmd(newRootCmd().Commands(), "egress-proxy")
	if cmd == nil {
		t.Fatal("missing egress-proxy subcommand")
	}
	for flag, want := range map[string]string{
		"listen": "", "port": "3128", "idle": "72h0m0s", "state-file": egressProxyStatePath(), "max-conns": "256",
	} {
		f := cmd.Flags().Lookup(flag)
		if f == nil {
			t.Errorf("egress-proxy lacks --%s", flag)
			continue
		}
		if f.DefValue != want {
			t.Errorf("--%s default %q, want %q", flag, f.DefValue, want)
		}
	}
}

func TestEgressProxyValidate(t *testing.T) {
	ok := egressProxyMode{port: 3128, idle: time.Hour, statePath: "/x"}
	if err := ok.validate(); err != nil {
		t.Fatal(err)
	}
	for name, m := range map[string]egressProxyMode{
		"zero idle":  {port: 3128, statePath: "/x"},
		"bad port":   {port: 70000, idle: time.Hour, statePath: "/x"},
		"bad listen": {listen: "10.0.0.1", idle: time.Hour, statePath: "/x"},
		"no state":   {port: 3128, idle: time.Hour},
	} {
		if err := m.validate(); err == nil {
			t.Errorf("%s: validated", name)
		}
	}
}

// By default the proxy binds the private IP, never every interface.
func TestEgressProxyListensOnThePrivateIP(t *testing.T) {
	m := &egressProxyMode{port: 3128}
	got, err := m.listenAddr(&provider.Identity{PrivateIP: "10.0.1.23"})
	if err != nil || got != "10.0.1.23:3128" {
		t.Errorf("listenAddr = %q, %v; want 10.0.1.23:3128", got, err)
	}
	if _, err := m.listenAddr(&provider.Identity{}); err == nil {
		t.Error("no private IP and no --listen bound something")
	}
	m.listen = "0.0.0.0:3128"
	if got, _ := m.listenAddr(&provider.Identity{PrivateIP: "10.0.1.23"}); got != "0.0.0.0:3128" {
		t.Errorf("--listen ignored: %q", got)
	}
}

// The mode hands the proxy's activity to the agent as its idle signal, with
// --idle as the timeout, and serves refusals on its listener.
func TestEgressProxyModeWiresTheIdleSignal(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	m := &egressProxyMode{port: 3128, idle: 72 * time.Hour, statePath: filepath.Join(t.TempDir(), "a.json"),
		now: func() time.Time { return now }}
	ag := &fakeIdleSetter{cfg: &provider.Config{}}
	addr, stop := startMode(t, m, ag)
	defer stop()

	e := ag.get()
	if e == nil || e.Timeout != 72*time.Hour || e.LastActivity == nil {
		t.Fatalf("external idle %+v, want a 72h timeout and an activity source", e)
	}
	if !e.LastActivity().Equal(now) {
		t.Errorf("first start: LastActivity %v, want now %v (no launch time, nothing persisted)", e.LastActivity(), now)
	}

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.WriteString(c, "CONNECT evil.example.com:443 HTTP/1.1\r\nHost: evil.example.com:443\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status %d, want 403", resp.StatusCode)
	}
}

// Activity before a restart is not lost, and a restart does not reset the
// idle clock either: the restarted proxy starts from the persisted time.
func TestEgressProxyActivitySurvivesRestart(t *testing.T) {
	state := filepath.Join(t.TempDir(), "activity.json")
	launch := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	used := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	if err := egressproxy.SaveActivity(state, used, used); err != nil {
		t.Fatal(err)
	}
	restartAt := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	m := &egressProxyMode{port: 3128, idle: 14 * 24 * time.Hour, statePath: state,
		now: func() time.Time { return restartAt }}
	ag := &fakeIdleSetter{cfg: &provider.Config{LaunchTime: launch}}
	_, stop := startMode(t, m, ag)
	if got := ag.get().LastActivity(); !got.Equal(used) {
		t.Errorf("after restart LastActivity %v, want the persisted %v", got, used)
	}
	stop()
	if got, err := egressproxy.LoadActivity(state); err != nil || !got.Equal(used) {
		t.Errorf("persisted at shutdown: %v, %v; want %v", got, err, used)
	}

	// Never used and nothing persisted: idle counts from the launch, so a
	// proxy rebooted every week for patches still expires.
	fresh := filepath.Join(t.TempDir(), "none.json")
	m2 := &egressProxyMode{port: 3128, idle: 14 * 24 * time.Hour, statePath: fresh,
		now: func() time.Time { return restartAt }}
	ag2 := &fakeIdleSetter{cfg: &provider.Config{LaunchTime: launch}}
	_, stop2 := startMode(t, m2, ag2)
	defer stop2()
	if got := ag2.get().LastActivity(); !got.Equal(launch) {
		t.Errorf("unused proxy after restart: LastActivity %v, want the launch time %v", got, launch)
	}
}

// The idle signal is set even when the listener cannot bind, so a proxy that
// can never serve still expires instead of running until its TTL.
func TestEgressProxyIdleIsSetWhenBindFails(t *testing.T) {
	m := &egressProxyMode{port: 3128, idle: time.Hour, statePath: filepath.Join(t.TempDir(), "a.json"),
		retryWait: 10 * time.Millisecond}
	var mu sync.Mutex
	tries := 0
	m.listenFunc = func(string, string) (net.Listener, error) {
		mu.Lock()
		defer mu.Unlock()
		tries++
		return nil, &net.OpError{Op: "listen", Err: errBind}
	}
	ag := &fakeIdleSetter{cfg: &provider.Config{}}
	stop := m.start(context.Background(), ag, &provider.Identity{PrivateIP: "10.0.1.23"})
	defer stop()
	if ag.get() == nil {
		t.Fatal("no idle signal while the listener cannot bind")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := tries
		mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("bind tried %d times, want a retry", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type bindErr struct{}

func (bindErr) Error() string { return "address already in use" }

var errBind = bindErr{}

// An idle period longer than the instance's life (launch to deadline) is
// refused: the proxy does not serve, and the instance is retired at the next
// tick instead of running under a period its deadline always beats (#1515).
func TestEgressProxyRefusesAnIdleLongerThanItsLife(t *testing.T) {
	launch := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	cfg := &provider.Config{LaunchTime: launch, TTLDeadline: launch.Add(egressproxy.MaxLife)}

	if err := idleRefusal(cfg, egressproxy.MaxLife); err != nil {
		t.Errorf("idle equal to the life refused: %v", err)
	}
	if err := idleRefusal(cfg, egressproxy.MaxLife+time.Second); err == nil {
		t.Error("idle one second past the life accepted")
	}
	if err := idleRefusal(&provider.Config{}, 1000*time.Hour); err != nil {
		t.Errorf("no deadline to measure against, yet refused: %v", err)
	}

	m := &egressProxyMode{port: 3128, idle: egressproxy.MaxLife + time.Hour,
		statePath: filepath.Join(t.TempDir(), "a.json"), retryWait: 10 * time.Millisecond}
	bound := false
	m.listenFunc = func(network, _ string) (net.Listener, error) {
		bound = true
		return net.Listen(network, "127.0.0.1:0")
	}
	ag := &fakeIdleSetter{cfg: cfg}
	stop := m.start(context.Background(), ag, &provider.Identity{PrivateIP: "10.0.1.23"})
	defer stop()
	time.Sleep(50 * time.Millisecond)
	if bound {
		t.Error("a refused proxy opened a listener")
	}
	e := ag.get()
	if e == nil {
		t.Fatal("a refused proxy set no idle signal, so nothing retires it")
	}
	if idle := time.Since(e.LastActivity()); e.Timeout <= 0 || idle < e.Timeout {
		t.Errorf("refused proxy's idle signal (timeout %v, idle %v) does not retire it at the next tick", e.Timeout, idle)
	}
}
