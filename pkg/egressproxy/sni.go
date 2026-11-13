package egressproxy

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// The ClientHello check on a CONNECT tunnel (#1595).
//
// The allowlisted HTTPS hosts sit behind shared CDN edges (CloudFront serves
// awscli.amazonaws.com and efa-installer.amazonaws.com), and an edge serves
// whichever distribution the TLS ClientHello's server_name asks for. Checking
// only the CONNECT authority let a client open "CONNECT awscli.amazonaws.com:443"
// and then send a ClientHello for <anything>.cloudfront.net, reaching any
// distribution through the proxy: exfiltration and command-and-control from a
// subnet chosen to have no internet route. So the tunnel reads the client's
// ClientHello before passing a byte on, and refuses it unless its server_name
// is the CONNECT host. A ClientHello with no server_name, or one carrying
// encrypted_client_hello (whose inner name the proxy cannot read, and whose
// outer name is only a cover), is refused too. TLS is still never terminated:
// the ClientHello is parsed, then forwarded unchanged with everything after it.

// maxClientHelloBytes bounds what the proxy reads before it has a
// ClientHello: a hello with post-quantum key shares is a few KiB, and TLS
// caps a handshake message at 16 MiB, which is no size to buffer.
const maxClientHelloBytes = 64 << 10

// extensionECH is the encrypted_client_hello extension's code point
// (RFC 9849).
const extensionECH = 0xfe0d

// clientHello is what the proxy reads from a ClientHello.
type clientHello struct {
	serverName string
	ech        bool
}

var errHelloRead = errors.New("egressproxy: ClientHello read")

// readClientHello reads one TLS ClientHello from r and returns it and every
// byte it read, which the caller must forward ahead of the rest of r. It parses
// with crypto/tls, as a server whose GetConfigForClient stops the handshake
// once it has the hello, so nothing is answered and nothing is decrypted.
func readClientHello(r io.Reader) (clientHello, []byte, error) {
	var rec bytes.Buffer
	lim := &io.LimitedReader{R: r, N: maxClientHelloBytes}
	conn := &helloConn{r: io.TeeReader(lim, &rec)}
	var hello clientHello
	seen := false
	srv := tls.Server(conn, &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetConfigForClient: func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
			seen = true
			hello.serverName = chi.ServerName
			for _, e := range chi.Extensions {
				if e == extensionECH {
					hello.ech = true
				}
			}
			return nil, errHelloRead
		},
	})
	err := srv.Handshake()
	if !seen {
		if lim.N <= 0 {
			return hello, rec.Bytes(), fmt.Errorf("no ClientHello in the first %d bytes", maxClientHelloBytes)
		}
		return hello, rec.Bytes(), fmt.Errorf("no TLS ClientHello: %w", err)
	}
	return hello, rec.Bytes(), nil
}

// helloRefusal is why a tunnel to host is refused given what readClientHello
// returned, or "".
func helloRefusal(host string, hello clientHello, err error) string {
	switch {
	case err != nil:
		return "the tunnel did not start with a TLS ClientHello"
	case hello.ech:
		return "the ClientHello uses encrypted_client_hello, so the server it names cannot be checked"
	case hello.serverName == "":
		return "the ClientHello names no server (SNI); it must name " + host
	}
	if sni, ok := normalizeHost(hello.serverName); !ok || sni != host {
		return fmt.Sprintf("the ClientHello names %s, not the CONNECT host %s", sanitize(hello.serverName), host)
	}
	return ""
}

// helloConn is the net.Conn readClientHello's TLS server reads from: reads
// come from r, and writes (the alert the server sends when stopped) go
// nowhere, so the client sees nothing from the proxy's parse.
type helloConn struct {
	r io.Reader
}

func (c *helloConn) Read(b []byte) (int, error)         { return c.r.Read(b) }
func (c *helloConn) Write(b []byte) (int, error)        { return len(b), nil }
func (c *helloConn) Close() error                       { return nil }
func (c *helloConn) LocalAddr() net.Addr                { return helloAddr{} }
func (c *helloConn) RemoteAddr() net.Addr               { return helloAddr{} }
func (c *helloConn) SetDeadline(time.Time) error        { return nil }
func (c *helloConn) SetReadDeadline(time.Time) error    { return nil }
func (c *helloConn) SetWriteDeadline(t time.Time) error { return nil }

type helloAddr struct{}

func (helloAddr) Network() string { return "egressproxy-hello" }
func (helloAddr) String() string  { return "client" }
