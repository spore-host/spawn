package egressproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Defaults for [Config]'s zero values.
const (
	DefaultMaxConns          = 256
	DefaultDialTimeout       = 10 * time.Second
	DefaultIdleTimeout       = 2 * time.Minute
	DefaultHeaderTimeout     = 10 * time.Second
	DefaultResponseTimeout   = 30 * time.Second
	defaultMaxHeaderBytes    = 32 << 10
	defaultClientIdleTimeout = 90 * time.Second
	copyBufferSize           = 32 << 10
	// activityGranularity bounds how often a byte stream refreshes the
	// activity time: an atomic store per 32 KiB read is cheap, but there is no
	// need to do it more than once a second.
	activityGranularity = time.Second
	// tunnelLinger is how long a tunnel whose origin has closed waits for the
	// client to close its side.
	tunnelLinger = 5 * time.Second
)

// Config configures a [Proxy]. The zero value is usable.
type Config struct {
	// MaxConns caps concurrent proxied connections, CONNECT tunnels and HTTP
	// forwards together. One over the cap is answered 503. Zero means
	// DefaultMaxConns.
	MaxConns int
	// DialTimeout bounds each upstream dial. Zero means DefaultDialTimeout.
	DialTimeout time.Duration
	// IdleTimeout closes a tunnel, or abandons a forward, after this long
	// with no bytes either way. Zero means DefaultIdleTimeout.
	IdleTimeout time.Duration
	// HeaderTimeout bounds how long a client may take to send its request
	// headers. Zero means DefaultHeaderTimeout.
	HeaderTimeout time.Duration
	// ResponseTimeout bounds how long a forwarded request waits for the
	// origin's response headers. Zero means DefaultResponseTimeout.
	ResponseTimeout time.Duration
	// InitialActivity seeds LastActivity, so a restarted proxy keeps the
	// activity it had (see InitialActivity in activity.go). Zero means the
	// proxy's start time.
	InitialActivity time.Time
	// Logger receives one line per request. nil means log.Default().
	Logger *log.Logger
	// Now is the clock. nil means time.Now. Tests only.
	Now func() time.Time
}

// Stats are the proxy's counters.
type Stats struct {
	Accepted uint64 // requests forwarded or tunnelled
	Refused  uint64 // requests answered with a refusal (403, 502 or 503)
}

// resolver is the DNS lookup the proxy vets addresses from.
type resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Proxy is the egress forward proxy. It is an http.Handler; [Proxy.Serve]
// runs it with the timeouts in its Config.
type Proxy struct {
	cfg Config
	log *log.Logger
	now func() time.Time

	// resolve and dial are the network seams. In production they are the
	// system resolver and a net.Dialer. Tests replace them to reach a
	// loopback upstream; the address check between them is not a seam.
	resolve resolver
	dial    func(ctx context.Context, network, addr string) (net.Conn, error)

	slots     chan struct{}
	last      atomic.Int64 // unix nanoseconds
	accepted  atomic.Uint64
	refused   atomic.Uint64
	transport *http.Transport
}

// New returns a Proxy for cfg.
func New(cfg Config) *Proxy {
	if cfg.MaxConns <= 0 {
		cfg.MaxConns = DefaultMaxConns
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = DefaultDialTimeout
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = DefaultIdleTimeout
	}
	if cfg.HeaderTimeout <= 0 {
		cfg.HeaderTimeout = DefaultHeaderTimeout
	}
	if cfg.ResponseTimeout <= 0 {
		cfg.ResponseTimeout = DefaultResponseTimeout
	}
	p := &Proxy{
		cfg:     cfg,
		log:     cfg.Logger,
		now:     cfg.Now,
		resolve: net.DefaultResolver,
		slots:   make(chan struct{}, cfg.MaxConns),
	}
	if p.log == nil {
		p.log = log.Default()
	}
	if p.now == nil {
		p.now = time.Now
	}
	d := &net.Dialer{Timeout: cfg.DialTimeout, KeepAlive: 30 * time.Second}
	p.dial = d.DialContext
	start := cfg.InitialActivity
	if start.IsZero() {
		start = p.now()
	}
	p.last.Store(start.UnixNano())
	p.transport = &http.Transport{
		// Never chain to another proxy, whatever the environment says.
		Proxy:                  nil,
		DialContext:            p.dialVetted,
		ResponseHeaderTimeout:  cfg.ResponseTimeout,
		IdleConnTimeout:        defaultClientIdleTimeout,
		MaxIdleConnsPerHost:    8,
		DisableCompression:     true, // pass the origin's encoding through untouched
		MaxResponseHeaderBytes: defaultMaxHeaderBytes,
	}
	return p
}

// LastActivity is when the proxy last accepted a request or moved a byte.
func (p *Proxy) LastActivity() time.Time { return time.Unix(0, p.last.Load()) }

// Stats returns the proxy's counters.
func (p *Proxy) Stats() Stats {
	return Stats{Accepted: p.accepted.Load(), Refused: p.refused.Load()}
}

// touch records activity now.
func (p *Proxy) touch() {
	n := p.now().UnixNano()
	for {
		old := p.last.Load()
		if n <= old || p.last.CompareAndSwap(old, n) {
			return
		}
	}
}

// Serve accepts connections on ln until ctx is done, then shuts down,
// waiting up to 10 s for forwards in flight. Hijacked tunnels are not waited
// for: their idle timeout ends them.
func (p *Proxy) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           p,
		ReadHeaderTimeout: p.cfg.HeaderTimeout,
		IdleTimeout:       defaultClientIdleTimeout,
		MaxHeaderBytes:    defaultMaxHeaderBytes,
		ErrorLog:          p.log,
		// No ReadTimeout or WriteTimeout: a forward of a 600 MB installer
		// legitimately takes minutes. Stalls are bounded by IdleTimeout on
		// the upstream connection instead.
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	p.transport.CloseIdleConnections()
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// refusal is a request the proxy will not carry.
type refusal struct {
	status int
	reason string
}

func (r *refusal) Error() string { return r.reason }

func refuse(status int, format string, args ...any) *refusal {
	return &refusal{status: status, reason: fmt.Sprintf(format, args...)}
}

// ServeHTTP handles one proxied request.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host, port, ref := p.target(r)
	if ref == nil {
		ref = p.acquire()
	}
	if ref != nil {
		p.deny(w, r, host, port, ref)
		return
	}
	defer p.release()

	addrs, ref := p.vet(r.Context(), host)
	if ref != nil {
		p.deny(w, r, host, port, ref)
		return
	}
	if r.Method == http.MethodConnect {
		p.tunnel(w, r, host, port, addrs)
		return
	}
	p.forward(w, r, host, port, addrs)
}

// target checks the request's shape, method, host and port, returning the
// normalised host and port, or a refusal.
func (p *Proxy) target(r *http.Request) (host, port string, _ *refusal) {
	var hostport string
	switch r.Method {
	case http.MethodConnect:
		// CONNECT's request target is the authority form, host:port.
		hostport = r.Host
		if hostport == "" || r.URL.Path != "" {
			return "", "", refuse(http.StatusForbidden, "CONNECT needs a host:port target")
		}
	case http.MethodGet, http.MethodHead:
		// A forward proxy request carries the absolute URI.
		if !r.URL.IsAbs() || r.URL.Host == "" {
			return "", "", refuse(http.StatusForbidden, "a proxied %s needs an absolute http:// URI", r.Method)
		}
		if r.URL.Scheme != "http" {
			return "", "", refuse(http.StatusForbidden, "only http:// is forwarded; use CONNECT for https")
		}
		if r.URL.User != nil {
			return "", "", refuse(http.StatusForbidden, "URIs with user info are refused")
		}
		hostport = r.URL.Host
	default:
		return "", "", refuse(http.StatusForbidden, "method %s is not allowed", sanitize(r.Method))
	}

	h, pt, err := net.SplitHostPort(hostport)
	if err != nil {
		if r.Method == http.MethodConnect {
			return "", "", refuse(http.StatusForbidden, "CONNECT needs a host:port target")
		}
		h, pt = hostport, "80"
	}
	want := "80"
	if r.Method == http.MethodConnect {
		want = "443"
	}
	if pt != want {
		return h, pt, refuse(http.StatusForbidden, "port %s is not allowed for %s (only %s)", sanitize(pt), r.Method, want)
	}
	norm, ok := normalizeHost(h)
	if !ok || !Allowed(norm) {
		return h, pt, refuse(http.StatusForbidden, "host %s is not on the egress allowlist", sanitize(h))
	}
	return norm, pt, nil
}

// acquire takes a connection slot, or refuses when none is free.
func (p *Proxy) acquire() *refusal {
	select {
	case p.slots <- struct{}{}:
		return nil
	default:
		return refuse(http.StatusServiceUnavailable, "too many concurrent connections (%d)", p.cfg.MaxConns)
	}
}

func (p *Proxy) release() { <-p.slots }

// vet resolves host once and returns its addresses, or a refusal if it has
// none or ANY of them is not a public unicast address. The caller dials only
// these addresses.
func (p *Proxy) vet(ctx context.Context, host string) ([]netip.Addr, *refusal) {
	lctx, cancel := context.WithTimeout(ctx, p.cfg.DialTimeout)
	defer cancel()
	addrs, err := p.resolve.LookupNetIP(lctx, "ip", host)
	if err != nil || len(addrs) == 0 {
		return nil, refuse(http.StatusBadGateway, "cannot resolve %s", host)
	}
	for _, a := range addrs {
		if !publicAddr(a) {
			return nil, refuse(http.StatusForbidden, "%s resolves to %s, which is not a public address", host, a.Unmap())
		}
	}
	return addrs, nil
}

// dialAddrs dials the vetted addresses in order and returns the first
// connection, wrapped so that every byte refreshes the activity time and
// extends the idle deadline.
func (p *Proxy) dialAddrs(ctx context.Context, addrs []netip.Addr, port string) (net.Conn, error) {
	pn, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return nil, err
	}
	var errs []error
	for _, a := range addrs {
		dctx, cancel := context.WithTimeout(ctx, p.cfg.DialTimeout)
		c, err := p.dial(dctx, "tcp", netip.AddrPortFrom(a.Unmap(), uint16(pn)).String())
		cancel()
		if err == nil {
			return &activeConn{Conn: c, p: p}, nil
		}
		errs = append(errs, err)
	}
	return nil, errors.Join(errs...)
}

// vettedKey carries a forward's vetted addresses to the transport's dialer.
type vettedKey struct{}

// dialVetted is the HTTP transport's dialer: it dials only the addresses the
// handler vetted for this request, never the name in addr.
func (p *Proxy) dialVetted(ctx context.Context, _, addr string) (net.Conn, error) {
	addrs, _ := ctx.Value(vettedKey{}).([]netip.Addr)
	if len(addrs) == 0 {
		return nil, errors.New("egressproxy: no vetted address for " + addr)
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	return p.dialAddrs(ctx, addrs, port)
}

// tunnel carries a CONNECT.
func (p *Proxy) tunnel(w http.ResponseWriter, r *http.Request, host, port string, addrs []netip.Addr) {
	start := p.now()
	up, err := p.dialAddrs(r.Context(), addrs, port)
	if err != nil {
		p.deny(w, r, host, port, refuse(http.StatusBadGateway, "cannot connect to %s:%s", host, port))
		return
	}
	defer func() { _ = up.Close() }()

	hj, ok := w.(http.Hijacker)
	if !ok {
		p.deny(w, r, host, port, refuse(http.StatusInternalServerError, "connection cannot be tunnelled"))
		return
	}
	client, brw, err := hj.Hijack()
	if err != nil {
		p.refused.Add(1)
		p.logLine(r, host, port, "refused", "hijack failed", 0, 0, start)
		return
	}
	defer func() { _ = client.Close() }()
	p.accepted.Add(1)
	p.touch()

	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		p.logLine(r, host, port, "allowed", "client gone", 0, 0, start)
		return
	}
	cc := &activeConn{Conn: client, p: p}
	cc.extend()
	up.(*activeConn).extend()

	var upBytes, downBytes int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// The client may have sent bytes (its TLS ClientHello) right behind
		// the CONNECT, already buffered in brw.
		upBytes, _ = copyBuf(up, io.MultiReader(io.LimitReader(brw, int64(brw.Reader.Buffered())), cc))
		closeWrite(up)
	}()
	downBytes, _ = copyBuf(cc, up)
	// The origin is done. Pass its EOF on, and give the client a moment to
	// finish its side rather than an idle timeout's worth.
	closeWrite(client)
	cc.finish(tunnelLinger)
	wg.Wait()
	p.logLine(r, host, port, "allowed", "", upBytes, downBytes, start)
}

// hopHeaders are the hop-by-hop headers RFC 9110 section 7.6.1 says a proxy
// must not forward, plus the proxy-only ones.
var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// stripHop removes hop-by-hop headers, those Connection names, and every
// Proxy-* header from h.
func stripHop(h http.Header) {
	for _, v := range h.Values("Connection") {
		for _, name := range strings.Split(v, ",") {
			if name = strings.TrimSpace(name); name != "" {
				h.Del(name)
			}
		}
	}
	for _, name := range hopHeaders {
		h.Del(name)
	}
	for name := range h {
		if strings.HasPrefix(http.CanonicalHeaderKey(name), "Proxy-") {
			delete(h, name)
		}
	}
}

// forward carries a plain-HTTP GET or HEAD.
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, host, port string, addrs []netip.Addr) {
	start := p.now()
	ctx := context.WithValue(r.Context(), vettedKey{}, addrs)
	out := r.Clone(ctx)
	out.RequestURI = ""
	out.URL.Host = net.JoinHostPort(host, port)
	if port == "80" {
		out.URL.Host = host
	}
	out.Host = out.URL.Host
	out.Header = r.Header.Clone()
	stripHop(out.Header)
	out.Body = http.NoBody
	out.ContentLength = 0
	out.Close = false

	resp, err := p.transport.RoundTrip(out)
	if err != nil {
		p.deny(w, r, host, port, refuse(http.StatusBadGateway, "upstream %s:%s failed", host, port))
		return
	}
	defer func() { _ = resp.Body.Close() }()
	p.accepted.Add(1)
	p.touch()

	stripHop(resp.Header)
	for k, vs := range resp.Header {
		w.Header()[k] = vs
	}
	w.WriteHeader(resp.StatusCode)
	n, _ := copyBuf(&deadlineWriter{w: w, rc: http.NewResponseController(w), idle: p.cfg.IdleTimeout}, resp.Body)
	p.logLine(r, host, port, "allowed", strconv.Itoa(resp.StatusCode), 0, n, start)
}

// deny answers a refusal and logs it.
func (p *Proxy) deny(w http.ResponseWriter, r *http.Request, host, port string, ref *refusal) {
	p.refused.Add(1)
	p.logLine(r, host, port, "refused", ref.reason, 0, 0, p.now())
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Connection", "close")
	w.WriteHeader(ref.status)
	_, _ = fmt.Fprintf(w, "spawn egress proxy: refused: %s\n", ref.reason)
}

// logLine writes the one line a request gets: method, host, port, verdict,
// reason, bytes each way and duration. Never the path, query or headers.
func (p *Proxy) logLine(r *http.Request, host, port, verdict, why string, up, down int64, start time.Time) {
	p.log.Printf("egress-proxy: client=%s method=%s host=%q port=%q verdict=%s reason=%q bytes_up=%d bytes_down=%d duration=%s",
		clientIP(r.RemoteAddr), sanitize(r.Method), sanitize(host), sanitize(port), verdict, why, up, down,
		p.now().Sub(start).Round(time.Millisecond))
}

// clientIP is the address part of a RemoteAddr.
func clientIP(remote string) string {
	if h, _, err := net.SplitHostPort(remote); err == nil {
		return h
	}
	return sanitize(remote)
}

// sanitize bounds a client-supplied string for a log line or a refusal body.
func sanitize(s string) string {
	const max = 255
	if len(s) > max {
		s = s[:max] + "..."
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, s)
}

// activeConn refreshes the proxy's activity time and the connection's idle
// deadline whenever bytes move.
type activeConn struct {
	net.Conn
	p        *Proxy
	lastSeen atomic.Int64
	done     atomic.Bool
}

func (c *activeConn) extend() {
	if c.done.Load() {
		return
	}
	_ = c.SetDeadline(time.Now().Add(c.p.cfg.IdleTimeout))
}

// finish stops extending the deadline and cuts it to d from now.
func (c *activeConn) finish(d time.Duration) {
	c.done.Store(true)
	_ = c.SetDeadline(time.Now().Add(d))
}

func (c *activeConn) moved(n int) {
	if n <= 0 {
		return
	}
	now := time.Now().UnixNano()
	if now-c.lastSeen.Load() < int64(activityGranularity) {
		return
	}
	c.lastSeen.Store(now)
	c.p.touch()
	c.extend()
}

func (c *activeConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.moved(n)
	return n, err
}

func (c *activeConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.moved(n)
	return n, err
}

// CloseWrite half-closes the underlying connection when it can.
func (c *activeConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Conn.Close()
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

// deadlineWriter pushes the client's write deadline out before each write, so
// a client that stops reading a forwarded response is dropped after the idle
// timeout instead of holding a connection slot for ever.
type deadlineWriter struct {
	w    io.Writer
	rc   *http.ResponseController
	idle time.Duration
}

func (d *deadlineWriter) Write(b []byte) (int, error) {
	_ = d.rc.SetWriteDeadline(time.Now().Add(d.idle))
	return d.w.Write(b)
}

func copyBuf(dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, copyBufferSize)
	return io.CopyBuffer(dst, src, buf)
}
