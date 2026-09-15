package agent

import (
	"context"
	"crypto/tls"
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

	// App is up — bring up the TLS reverse proxy once.
	if !a.webProxyStarted {
		if err := a.startWebProxy(a.config.ReadyPort); err != nil {
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
	a.writeReadyTags(ctx, map[string]string{
		"spawn:ready-url":    buildWebReadyURL(host),
		"spawn:ready-status": "ready",
	})
	if a.dcvReadyURLWritten {
		log.Printf("web: spawn:ready-url written (port %d, host %s)", a.config.ReadyPort, host)
		a.webReadyDone = true
	}
}

// buildWebReadyURL is the ready-url for a web app: the app is reached over the
// :443 TLS reverse proxy at the instance FQDN root. No auth token (unlike DCV) —
// the app owns its own auth. Pure, so it's unit-tested.
func buildWebReadyURL(host string) string {
	return fmt.Sprintf("https://%s/", host)
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
func (a *Agent) startWebProxy(port int) error {
	cert, err := tls.LoadX509KeyPair(webProxyCertPath, webProxyKeyPath)
	if err != nil {
		return fmt.Errorf("load webproxy cert (%s): %w", webProxyCertPath, err)
	}
	target, err := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("parse proxy target: %w", err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
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
