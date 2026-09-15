package agent

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/spore-host/spawn/pkg/dns"
)

// Web-UI app readiness (#590). For AppMode=="web" spored probes the app's own
// HTTP port instead of a DCV session, fronts it with a TLS reverse proxy on :443
// (terminating TLS with the same wildcard cert the DCV path uses), and writes the
// spawn:ready-* tags the CLI already understands. There is no DCV on a web
// instance; idle detection uses the generic CPU/network/process path.

// Web ready-status wire values (mirrored in cmd/dcv.go for the CLI). "ready" is
// shared with the DCV path.
const (
	webStatusWaiting       = "web-waiting"
	webStatusNotResponding = "web-not-responding" // terminal: app never answered
	webStatusProxyFailed   = "web-proxy-failed"   // terminal: TLS proxy couldn't start (e.g. cert missing)
)

// webReadyWaitTimeout bounds how long spored waits for the app's port to answer
// before recording a terminal web-not-responding, mirroring dcvSessionWaitTimeout.
const webReadyWaitTimeout = 4 * time.Minute

// webProxyCertPath / webProxyKeyPath are where the launch user-data drops the
// wildcard TLS cert for the reverse proxy (same spawn-certs source as DCV, but
// DCV owns its own copy under /var/lib/dcv). Kept off the DCV path so the two
// modes don't share cert state.
const (
	webProxyCertPath = "/etc/spore/webproxy/cert.pem"
	webProxyKeyPath  = "/etc/spore/webproxy/key.pem"
)

// maybeSetupWebReady drives the web-app readiness handshake once per monitor
// tick: probe the app's HTTP port; when it answers, start the :443 TLS reverse
// proxy (once) and write spawn:ready-url. Retries within the CLI's poll window,
// records a terminal status on give-up. No-op unless AppMode=="web" on EC2.
func (a *Agent) maybeSetupWebReady(ctx context.Context) {
	if a.config.AppMode != "web" || a.config.ReadyPort <= 0 || a.identity.Provider != "ec2" || a.webReadyDone {
		return
	}

	if !probeHTTP(a.config.ReadyPort, a.config.ReadyHealthPath) {
		if time.Since(a.startTime) > webReadyWaitTimeout {
			log.Printf("web: app on 127.0.0.1:%d never answered (giving up)", a.config.ReadyPort)
			a.writeReadyTags(ctx, map[string]string{"spawn:ready-status": webStatusNotResponding})
			a.webReadyDone = true
		}
		return // keep polling
	}

	// Access token gating the :443 proxy (default), unless auth is disabled. Like
	// the DCV authToken, it's generated once so any web app is gated regardless of
	// its own auth. Generate before starting the proxy so the gate has it.
	if a.config.WebAuth != "none" && a.webToken == "" {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			log.Printf("web: failed to generate access token: %v", err)
			return // retry next tick
		}
		a.webToken = hex.EncodeToString(b)
	}

	// App is up — bring up the TLS reverse proxy once.
	if !a.webProxyStarted {
		if err := a.startWebProxy(a.config.ReadyPort, a.webToken); err != nil {
			log.Printf("web: TLS reverse proxy failed to start: %v (giving up)", err)
			a.writeReadyTags(ctx, map[string]string{"spawn:ready-status": webStatusProxyFailed})
			a.webReadyDone = true
			return
		}
		a.webProxyStarted = true
	}

	host := a.identity.PublicIP
	if a.config.DNSName != "" && a.dnsDomain != "" {
		host = dns.GetFullDNSName(a.config.DNSName, a.identity.AccountID, a.dnsDomain)
	}
	tags := map[string]string{
		"spawn:ready-url":    buildWebReadyURL(host, a.webToken),
		"spawn:ready-status": "ready",
	}
	if a.webToken != "" {
		tags["spawn:ready-token"] = a.webToken
	}
	a.writeReadyTags(ctx, tags)
	if a.dcvReadyURLWritten {
		log.Printf("web: spawn:ready-url written (port %d, host %s, auth %s)", a.config.ReadyPort, host, a.config.WebAuth)
		a.webReadyDone = true
	}
}

// buildWebReadyURL is the ready-url for a web app: the app is reached over the
// :443 TLS reverse proxy at the instance FQDN root. When token != "", it's a
// one-time query param that the proxy exchanges for a session cookie; empty
// (WebAuth=none) yields a bare URL. Pure, so it's unit-tested.
func buildWebReadyURL(host, token string) string {
	if token == "" {
		return fmt.Sprintf("https://%s/", host)
	}
	return fmt.Sprintf("https://%s/?spore_token=%s", host, token)
}

// spore_token is the cookie/query-param name the proxy gate uses.
const webTokenParam = "spore_token"

// webAuthHandler wraps the reverse proxy with a token gate: a request bearing
// ?spore_token=<token> (matching) gets a Secure/HttpOnly cookie and a redirect
// to the same path sans the param; a request with a valid cookie passes through
// (this covers same-origin WebSocket upgrades, which carry cookies); anything
// else gets 403. When token == "" the gate is a pass-through (WebAuth=none).
func webAuthHandler(proxy http.Handler, token string) http.Handler {
	if token == "" {
		return proxy
	}
	tokenBytes := []byte(token)
	valid := func(got string) bool {
		return subtle.ConstantTimeCompare([]byte(got), tokenBytes) == 1
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query().Get(webTokenParam); q != "" && valid(q) {
			http.SetCookie(w, &http.Cookie{
				Name:     webTokenParam,
				Value:    token,
				Path:     "/",
				Secure:   true,
				HttpOnly: true,
				SameSite: http.SameSiteLaxMode,
			})
			// Redirect to the same path without the token query param.
			clean := *r.URL
			qq := clean.Query()
			qq.Del(webTokenParam)
			clean.RawQuery = qq.Encode()
			http.Redirect(w, r, clean.RequestURI(), http.StatusFound)
			return
		}
		if c, err := r.Cookie(webTokenParam); err == nil && valid(c.Value) {
			proxy.ServeHTTP(w, r)
			return
		}
		http.Error(w, "forbidden: missing or invalid spore access token", http.StatusForbidden)
	})
}

// probeHTTP reports whether the app is serving on 127.0.0.1:port. Any response
// below 500 counts as up (a 200/302/401 all mean the server is answering — the
// app owns its own auth/redirects). Path defaults to "/".
func probeHTTP(port int, path string) bool {
	if path == "" {
		path = "/"
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d%s", port, path))
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode < 500
}

// startWebProxy starts a TLS reverse proxy on :443 that terminates TLS with the
// wildcard cert (dropped by the launch user-data) and forwards to the app on
// 127.0.0.1:port. httputil.ReverseProxy transparently handles WebSocket upgrades
// (Jupyter/code-server need them). Runs in a background goroutine.
func (a *Agent) startWebProxy(port int, token string) error {
	cert, err := tls.LoadX509KeyPair(webProxyCertPath, webProxyKeyPath)
	if err != nil {
		return fmt.Errorf("load webproxy cert (%s): %w", webProxyCertPath, err)
	}
	target, err := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("parse proxy target: %w", err)
	}
	proxy := webAuthHandler(httputil.NewSingleHostReverseProxy(target), token)
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	// Binding all interfaces on :443 is intentional and required: the web app must
	// be reachable over TLS from the user's browser. Public exposure is gated by
	// the spawn-web security group (:443 only) + the app's own auth, not by the
	// bind address. TLS min version is pinned above.
	// nosemgrep: go.lang.security.audit.net.bind_all.avoid-bind-to-all-interfaces
	ln, err := tls.Listen("tcp", ":443", tlsCfg)
	if err != nil {
		return fmt.Errorf("listen :443: %w", err)
	}
	srv := &http.Server{Handler: proxy}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("web proxy: serve stopped: %v", err)
		}
	}()
	log.Printf("web: TLS reverse proxy listening on :443 → 127.0.0.1:%d", port)
	return nil
}
