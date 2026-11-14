package egressproxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeNet is the test seam: a resolver answering from a table, and a dialer
// that records the address the proxy dialled and connects to a loopback
// upstream instead. The proxy's address check sits between the two and is not
// replaced, so loopback is reachable only because the dialer redirects a
// vetted public address to it.
type fakeNet struct {
	mu       sync.Mutex
	answers  map[string][][]netip.Addr // per host, one answer per lookup; the last repeats
	lookups  map[string]int
	dialed   []string
	upstream string // loopback host:port every dial reaches
}

func newFakeNet(upstream string) *fakeNet {
	return &fakeNet{answers: map[string][][]netip.Addr{}, lookups: map[string]int{}, upstream: upstream}
}

func (f *fakeNet) set(host string, answers ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var as []netip.Addr
	for _, a := range answers {
		as = append(as, netip.MustParseAddr(a))
	}
	f.answers[host] = append(f.answers[host], as)
}

func (f *fakeNet) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	seq := f.answers[host]
	if len(seq) == 0 {
		return nil, errors.New("no such host")
	}
	i := f.lookups[host]
	f.lookups[host]++
	if i >= len(seq) {
		i = len(seq) - 1
	}
	return seq[i], nil
}

func (f *fakeNet) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	f.mu.Lock()
	f.dialed = append(f.dialed, addr)
	up := f.upstream
	f.mu.Unlock()
	var d net.Dialer
	return d.DialContext(ctx, network, up)
}

func (f *fakeNet) dials() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.dialed...)
}

func (f *fakeNet) lookupCount(host string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lookups[host]
}

// syncBuf is a goroutine-safe log sink.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// startProxy serves a Proxy wired to fn on a loopback port and returns it and
// its address.
func startProxy(t *testing.T, fn *fakeNet, cfg Config) (*Proxy, string, *syncBuf) {
	t.Helper()
	logs := &syncBuf{}
	cfg.Logger = log.New(logs, "", 0)
	p := New(cfg)
	p.resolve = fn
	p.dial = fn.dial
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = p.Serve(ctx, ln); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return p, ln.Addr().String(), logs
}

// echoServer is a TLS-less upstream that echoes every byte back.
func echoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer func() { _ = c.Close() }(); _, _ = io.Copy(c, c) }()
		}
	}()
	return ln.Addr().String()
}

// rawRequest sends req verbatim to the proxy and returns its response and
// the connection, positioned after the response headers.
func rawRequest(t *testing.T, proxy, req string) (*http.Response, net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.Dial("tcp", proxy)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(c, req); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("reading response to %q: %v", strings.SplitN(req, "\r\n", 2)[0], err)
	}
	return resp, c, br
}

func TestConnectTunnelsBytesToTheVettedAddress(t *testing.T) {
	fn := newFakeNet(echoServer(t))
	fn.set("efa-installer.amazonaws.com", "52.94.233.129")
	clock := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	p, addr, logs := startProxy(t, fn, Config{Now: now, InitialActivity: clock.Add(-time.Hour)})

	mu.Lock()
	clock = clock.Add(time.Minute)
	mu.Unlock()
	// The ClientHello, and "hello" after it, ride in the same write as the
	// CONNECT, as a TLS client's first flight often does: the proxy must not
	// drop bytes it has already buffered, and forwards the hello unchanged.
	ch := clientHelloFor(t, "efa-installer.amazonaws.com")
	resp, c, br := rawRequest(t, addr,
		"CONNECT efa-installer.amazonaws.com:443 HTTP/1.1\r\nHost: efa-installer.amazonaws.com:443\r\n\r\n"+string(ch)+"hello")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status %d, want 200", resp.StatusCode)
	}
	echoed := make([]byte, len(ch))
	if _, err := io.ReadFull(br, echoed); err != nil || !bytes.Equal(echoed, ch) {
		t.Fatalf("the ClientHello was not forwarded unchanged: %v", err)
	}
	got := make([]byte, 5)
	if _, err := io.ReadFull(br, got); err != nil || string(got) != "hello" {
		t.Fatalf("echo of the buffered bytes = %q, %v", got, err)
	}
	if _, err := io.WriteString(c, "world"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(br, got); err != nil || string(got) != "world" {
		t.Fatalf("echo = %q, %v", got, err)
	}
	if d := fn.dials(); len(d) != 1 || d[0] != "52.94.233.129:443" {
		t.Errorf("dialled %v, want exactly the vetted 52.94.233.129:443", d)
	}
	if !p.LastActivity().Equal(clock) {
		t.Errorf("LastActivity = %v, want the accept time %v", p.LastActivity(), clock)
	}
	_ = c.Close()
	up := fmt.Sprintf("bytes_up=%d", len(ch)+10)
	waitFor(t, func() bool { return strings.Contains(logs.String(), up) })
	if s := p.Stats(); s.Accepted != 1 || s.Refused != 0 {
		t.Errorf("stats %+v, want 1 accepted, 0 refused", s)
	}
	line := logs.String()
	for _, want := range []string{`method=CONNECT`, `host="efa-installer.amazonaws.com"`, `port="443"`, `verdict=allowed`, up, fmt.Sprintf("bytes_down=%d", len(ch)+10)} {
		if !strings.Contains(line, want) {
			t.Errorf("log %q lacks %q", line, want)
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in 5s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// httpUpstream records the request it receives and answers with hop-by-hop
// headers of its own.
func httpUpstream(t *testing.T) (string, func() *http.Request) {
	t.Helper()
	var mu sync.Mutex
	var got *http.Request
	srv := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			got = r.Clone(context.Background())
			mu.Unlock()
			w.Header().Set("Connection", "X-Up-Hop")
			w.Header().Set("X-Up-Hop", "1")
			w.Header().Set("Keep-Alive", "timeout=5")
			w.Header().Set("Proxy-Authenticate", "Basic")
			w.Header().Set("X-Up-Keep", "yes")
			_, _ = io.WriteString(w, "InRelease body")
		}),
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String(), func() *http.Request { mu.Lock(); defer mu.Unlock(); return got }
}

func TestHTTPForwardStripsHopByHopHeaders(t *testing.T) {
	up, received := httpUpstream(t)
	fn := newFakeNet(up)
	fn.set("us-east-1.ec2.archive.ubuntu.com", "91.189.91.81")
	p, addr, logs := startProxy(t, fn, Config{})

	resp, _, br := rawRequest(t, addr, "GET http://us-east-1.ec2.archive.ubuntu.com/ubuntu/dists/noble/InRelease?token=s3cr3t HTTP/1.1\r\n"+
		"Host: somewhere-else.example\r\n"+
		"Connection: keep-alive, X-Hop\r\n"+
		"X-Hop: 1\r\n"+
		"Keep-Alive: timeout=5\r\n"+
		"Proxy-Authorization: Basic Zm9vOmJhcg==\r\n"+
		"Proxy-Connection: keep-alive\r\n"+
		"Proxy-Anything: x\r\n"+
		"Te: trailers\r\n"+
		"Upgrade: h2c\r\n"+
		"User-Agent: Debian APT-HTTP/1.3\r\n"+
		"X-Keep: yes\r\n\r\n")
	body, _ := io.ReadAll(io.LimitReader(br, resp.ContentLength))
	if resp.StatusCode != 200 || string(body) != "InRelease body" {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}

	r := received()
	if r == nil {
		t.Fatal("upstream received nothing")
	}
	if r.Host != "us-east-1.ec2.archive.ubuntu.com" {
		t.Errorf("upstream Host %q: the proxy must send the URI's host, not the client's Host header", r.Host)
	}
	if r.URL.RequestURI() != "/ubuntu/dists/noble/InRelease?token=s3cr3t" {
		t.Errorf("upstream request URI %q", r.URL.RequestURI())
	}
	for _, h := range []string{"X-Hop", "Keep-Alive", "Proxy-Authorization", "Proxy-Connection", "Proxy-Anything", "Te", "Upgrade"} {
		if v := r.Header.Get(h); v != "" {
			t.Errorf("upstream received hop-by-hop %s: %q", h, v)
		}
	}
	for _, h := range []string{"X-Keep", "User-Agent"} {
		if r.Header.Get(h) == "" {
			t.Errorf("upstream did not receive end-to-end %s", h)
		}
	}
	for _, h := range []string{"X-Up-Hop", "Keep-Alive", "Proxy-Authenticate"} {
		if v := resp.Header.Get(h); v != "" {
			t.Errorf("client received hop-by-hop %s: %q", h, v)
		}
	}
	if resp.Header.Get("X-Up-Keep") != "yes" {
		t.Error("client did not receive end-to-end X-Up-Keep")
	}
	if d := fn.dials(); len(d) != 1 || d[0] != "91.189.91.81:80" {
		t.Errorf("dialled %v, want exactly the vetted 91.189.91.81:80", d)
	}
	if s := p.Stats(); s.Accepted != 1 {
		t.Errorf("stats %+v, want 1 accepted", s)
	}
	waitFor(t, func() bool { return strings.Contains(logs.String(), "verdict=allowed") })
	l := logs.String()
	for _, secret := range []string{"s3cr3t", "InRelease", "/ubuntu", "Zm9vOmJhcg", "APT-HTTP"} {
		if strings.Contains(l, secret) {
			t.Errorf("log %q contains %q: no paths, queries or headers may be logged", l, secret)
		}
	}
}

func TestEveryRefusal(t *testing.T) {
	cases := []struct {
		name, req string
		status    int
		why       string
	}{
		{"host not listed", "CONNECT evil.example.com:443 HTTP/1.1\r\nHost: evil.example.com:443\r\n\r\n", 403, "not on the egress allowlist"},
		{"suffix trick", "CONNECT efa-installer.amazonaws.com.evil.com:443 HTTP/1.1\r\nHost: x\r\n\r\n", 403, "not on the egress allowlist"},
		{"CONNECT port 22", "CONNECT efa-installer.amazonaws.com:22 HTTP/1.1\r\nHost: x\r\n\r\n", 403, "port 22 is not allowed"},
		{"CONNECT port 80", "CONNECT archive.ubuntu.com:80 HTTP/1.1\r\nHost: x\r\n\r\n", 403, "port 80 is not allowed"},
		{"CONNECT no port", "CONNECT efa-installer.amazonaws.com HTTP/1.1\r\nHost: x\r\n\r\n", 403, "host:port"},
		{"CONNECT IP literal", "CONNECT 52.94.233.129:443 HTTP/1.1\r\nHost: x\r\n\r\n", 403, "not on the egress allowlist"},
		{"CONNECT IMDS literal", "CONNECT 169.254.169.254:443 HTTP/1.1\r\nHost: x\r\n\r\n", 403, "not on the egress allowlist"},
		{"GET port 8080", "GET http://archive.ubuntu.com:8080/ HTTP/1.1\r\nHost: archive.ubuntu.com\r\n\r\n", 403, "port 8080 is not allowed"},
		{"GET port 443", "GET http://archive.ubuntu.com:443/ HTTP/1.1\r\nHost: archive.ubuntu.com\r\n\r\n", 403, "port 443 is not allowed"},
		{"GET not absolute", "GET /ubuntu/ HTTP/1.1\r\nHost: archive.ubuntu.com\r\n\r\n", 403, "absolute http:// URI"},
		{"GET https URI", "GET https://archive.ubuntu.com/ HTTP/1.1\r\nHost: archive.ubuntu.com\r\n\r\n", 403, "only http://"},
		{"GET user info", "GET http://u:p@archive.ubuntu.com/ HTTP/1.1\r\nHost: archive.ubuntu.com\r\n\r\n", 403, "user info"},
		{"GET IP literal", "GET http://127.0.0.1/ HTTP/1.1\r\nHost: archive.ubuntu.com\r\n\r\n", 403, "not on the egress allowlist"},
		{"GET IMDS", "GET http://169.254.169.254/latest/meta-data/ HTTP/1.1\r\nHost: 169.254.169.254\r\n\r\n", 403, "not on the egress allowlist"},
		{"GET host not listed", "GET http://example.com/ HTTP/1.1\r\nHost: example.com\r\n\r\n", 403, "not on the egress allowlist"},
		{"POST", "POST http://archive.ubuntu.com/ HTTP/1.1\r\nHost: archive.ubuntu.com\r\nContent-Length: 0\r\n\r\n", 403, "method POST"},
		{"PUT", "PUT http://archive.ubuntu.com/ HTTP/1.1\r\nHost: archive.ubuntu.com\r\nContent-Length: 0\r\n\r\n", 403, "method PUT"},
		{"DELETE", "DELETE http://archive.ubuntu.com/ HTTP/1.1\r\nHost: archive.ubuntu.com\r\n\r\n", 403, "method DELETE"},
		{"OPTIONS", "OPTIONS http://archive.ubuntu.com/ HTTP/1.1\r\nHost: archive.ubuntu.com\r\n\r\n", 403, "method OPTIONS"},
		{"TRACE", "TRACE http://archive.ubuntu.com/ HTTP/1.1\r\nHost: archive.ubuntu.com\r\n\r\n", 403, "method TRACE"},
		{"unresolvable", "CONNECT awscli.amazonaws.com:443 HTTP/1.1\r\nHost: x\r\n\r\n", 502, "cannot resolve"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fn := newFakeNet(echoServer(t))
			fn.set("efa-installer.amazonaws.com", "52.94.233.129")
			fn.set("archive.ubuntu.com", "91.189.91.81")
			p, addr, _ := startProxy(t, fn, Config{InitialActivity: time.Unix(1000, 0)})
			resp, _, br := rawRequest(t, addr, tc.req)
			body, _ := io.ReadAll(br)
			if resp.StatusCode != tc.status {
				t.Errorf("status %d, want %d", resp.StatusCode, tc.status)
			}
			if !strings.Contains(string(body), "refused") || !strings.Contains(string(body), tc.why) {
				t.Errorf("body %q, want a refusal saying %q", body, tc.why)
			}
			if d := fn.dials(); len(d) != 0 {
				t.Errorf("a refused request dialled %v", d)
			}
			if s := p.Stats(); s.Refused != 1 || s.Accepted != 0 {
				t.Errorf("stats %+v, want 1 refused, 0 accepted", s)
			}
			if !p.LastActivity().Equal(time.Unix(1000, 0)) {
				t.Error("a refused request counted as activity")
			}
		})
	}
}

// The SSRF guard: a listed host whose DNS answer includes ANY non-public
// address is refused before anything is dialled, for both kinds of request.
func TestNonPublicResolutionIsRefused(t *testing.T) {
	for _, answer := range [][]string{
		{"169.254.169.254"},
		{"52.94.233.129", "169.254.169.254"}, // one bad address taints the answer
		{"127.0.0.1"},
		{"10.0.0.5"},
		{"100.64.0.1"},
		{"::ffff:169.254.169.254"},
		{"fd00:ec2::254"},
		{"fe80::1"},
		{"0.0.0.0"},
	} {
		for _, kind := range []string{"CONNECT", "GET"} {
			t.Run(kind+" "+strings.Join(answer, ","), func(t *testing.T) {
				fn := newFakeNet(echoServer(t))
				fn.set("efa-installer.amazonaws.com", answer...)
				fn.set("archive.ubuntu.com", answer...)
				p, addr, _ := startProxy(t, fn, Config{IdleTimeout: 200 * time.Millisecond})
				req := "CONNECT efa-installer.amazonaws.com:443 HTTP/1.1\r\nHost: x\r\n\r\n"
				if kind == "GET" {
					req = "GET http://archive.ubuntu.com/ubuntu/ HTTP/1.1\r\nHost: archive.ubuntu.com\r\n\r\n"
				}
				resp, _, br := rawRequest(t, addr, req)
				body, _ := io.ReadAll(br)
				if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "not a public address") {
					t.Errorf("status %d body %q, want 403 not a public address", resp.StatusCode, body)
				}
				if d := fn.dials(); len(d) != 0 {
					t.Errorf("dialled %v for a non-public answer", d)
				}
				if p.Stats().Refused != 1 {
					t.Errorf("stats %+v", p.Stats())
				}
			})
		}
	}
}

// DNS rebinding: the name is resolved once per request and the vetted address
// is what is dialled, so an answer that changes to IMDS after the check never
// reaches it.
func TestDialUsesTheVettedAddressNotANewLookup(t *testing.T) {
	t.Run("CONNECT", func(t *testing.T) {
		fn := newFakeNet(echoServer(t))
		fn.set("awscli.amazonaws.com", "52.94.233.129")
		fn.set("awscli.amazonaws.com", "169.254.169.254") // every later lookup
		_, addr, _ := startProxy(t, fn, Config{})
		resp, _, _ := rawRequest(t, addr, "CONNECT awscli.amazonaws.com:443 HTTP/1.1\r\nHost: x\r\n\r\n")
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		if n := fn.lookupCount("awscli.amazonaws.com"); n != 1 {
			t.Errorf("%d lookups, want 1", n)
		}
		if d := fn.dials(); len(d) != 1 || d[0] != "52.94.233.129:443" {
			t.Errorf("dialled %v, want the vetted 52.94.233.129:443", d)
		}
	})
	t.Run("GET", func(t *testing.T) {
		up, _ := httpUpstream(t)
		fn := newFakeNet(up)
		fn.set("security.ubuntu.com", "185.125.190.36")
		fn.set("security.ubuntu.com", "169.254.169.254")
		_, addr, _ := startProxy(t, fn, Config{})
		resp, _, _ := rawRequest(t, addr, "GET http://security.ubuntu.com/ubuntu/ HTTP/1.1\r\nHost: security.ubuntu.com\r\n\r\n")
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		if n := fn.lookupCount("security.ubuntu.com"); n != 1 {
			t.Errorf("%d lookups, want 1", n)
		}
		if d := fn.dials(); len(d) != 1 || d[0] != "185.125.190.36:80" {
			t.Errorf("dialled %v, want the vetted 185.125.190.36:80", d)
		}
	})
}

// The test seam replaces only the resolver and the dialer; the address check
// between them is not a seam. A loopback answer is refused even though the
// fake dialer could reach loopback, and the production resolver is the
// system's.
func TestDialerSeamDoesNotBypassTheAddressCheck(t *testing.T) {
	fn := newFakeNet(echoServer(t))
	fn.set("efa-installer.amazonaws.com", "127.0.0.1")
	_, addr, _ := startProxy(t, fn, Config{})
	resp, _, _ := rawRequest(t, addr, "CONNECT efa-installer.amazonaws.com:443 HTTP/1.1\r\nHost: x\r\n\r\n")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a loopback answer got status %d, want 403", resp.StatusCode)
	}
	if len(fn.dials()) != 0 {
		t.Error("a loopback answer was dialled")
	}
	if New(Config{}).resolve != net.DefaultResolver {
		t.Error("the production resolver is not the system resolver")
	}
}

// net/http answers "OPTIONS *" itself, without calling the handler. That is
// harmless here, and this pins that it reaches nothing upstream.
func TestServerWideOptionsReachesNothing(t *testing.T) {
	fn := newFakeNet(echoServer(t))
	p, addr, _ := startProxy(t, fn, Config{InitialActivity: time.Unix(1000, 0)})
	resp, _, _ := rawRequest(t, addr, "OPTIONS * HTTP/1.1\r\nHost: archive.ubuntu.com\r\n\r\n")
	_ = resp.Body.Close()
	if len(fn.dials()) != 0 || fn.lookupCount("archive.ubuntu.com") != 0 {
		t.Errorf("OPTIONS * reached the network: dials %v", fn.dials())
	}
	if p.Stats().Accepted != 0 || !p.LastActivity().Equal(time.Unix(1000, 0)) {
		t.Error("OPTIONS * counted as proxied activity")
	}
}

func TestConcurrentConnectionCap(t *testing.T) {
	fn := newFakeNet(echoServer(t))
	fn.set("efa-installer.amazonaws.com", "52.94.233.129")
	p, addr, _ := startProxy(t, fn, Config{MaxConns: 1})
	req := "CONNECT efa-installer.amazonaws.com:443 HTTP/1.1\r\nHost: x\r\n\r\n"
	first, c, _ := rawRequest(t, addr, req)
	if first.StatusCode != 200 {
		t.Fatalf("first CONNECT status %d", first.StatusCode)
	}
	second, _, _ := rawRequest(t, addr, req)
	if second.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("second CONNECT over the cap: status %d, want 503", second.StatusCode)
	}
	_ = c.Close()
	waitFor(t, func() bool {
		r, _, _ := rawRequest(t, addr, req)
		return r.StatusCode == 200
	})
	if p.Stats().Refused < 1 {
		t.Errorf("stats %+v", p.Stats())
	}
}

// A tunnel with no bytes either way for IdleTimeout is closed.
func TestIdleTunnelIsClosed(t *testing.T) {
	fn := newFakeNet(echoServer(t))
	fn.set("efa-installer.amazonaws.com", "52.94.233.129")
	_, addr, _ := startProxy(t, fn, Config{IdleTimeout: 100 * time.Millisecond})
	ch := clientHelloFor(t, "efa-installer.amazonaws.com")
	resp, _, br := rawRequest(t, addr, "CONNECT efa-installer.amazonaws.com:443 HTTP/1.1\r\nHost: x\r\n\r\n"+string(ch))
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if _, err := io.ReadFull(br, make([]byte, len(ch))); err != nil {
		t.Fatalf("the ClientHello was not echoed: %v", err)
	}
	start := time.Now()
	if _, err := br.ReadByte(); err == nil {
		t.Fatal("read a byte from an idle tunnel")
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Errorf("idle tunnel closed after %v", el)
	}
}

// Bytes moving through a tunnel refresh LastActivity, not only the accept.
func TestBytesRefreshActivity(t *testing.T) {
	fn := newFakeNet(echoServer(t))
	fn.set("efa-installer.amazonaws.com", "52.94.233.129")
	var mu sync.Mutex
	clock := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	p, addr, _ := startProxy(t, fn, Config{Now: now})
	ch := clientHelloFor(t, "efa-installer.amazonaws.com")
	_, c, br := rawRequest(t, addr, "CONNECT efa-installer.amazonaws.com:443 HTTP/1.1\r\nHost: x\r\n\r\n"+string(ch))
	if _, err := io.ReadFull(br, make([]byte, len(ch))); err != nil {
		t.Fatalf("the ClientHello was not echoed: %v", err)
	}
	accepted := p.LastActivity()

	mu.Lock()
	clock = clock.Add(time.Hour)
	mu.Unlock()
	time.Sleep(activityGranularity + 50*time.Millisecond) // past the per-connection throttle
	if _, err := io.WriteString(c, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := br.ReadByte(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return p.LastActivity().After(accepted) })
	if got := p.LastActivity(); !got.Equal(clock) {
		t.Errorf("LastActivity %v, want %v", got, clock)
	}
}

func ExampleAllowed() {
	fmt.Println(Allowed("us-east-1.ec2.archive.ubuntu.com"), Allowed("evilefa-installer.amazonaws.com"))
	// Output: true false
}
