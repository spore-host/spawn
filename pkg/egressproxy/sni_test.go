package egressproxy

import (
	"crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// clientHelloFor is the first TLS record a Go client sends for serverName: a
// real ClientHello, captured over a pipe. An IP literal sends no server_name
// (RFC 6066 forbids one), which is how a hello without SNI is made.
func clientHelloFor(t *testing.T, serverName string) []byte {
	t.Helper()
	c1, c2 := net.Pipe()
	defer func() { _ = c1.Close(); _ = c2.Close() }()
	go func() {
		_ = tls.Client(c1, &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}).Handshake()
	}()
	_ = c2.SetReadDeadline(time.Now().Add(5 * time.Second))
	hdr := make([]byte, 5)
	if _, err := io.ReadFull(c2, hdr); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, binary.BigEndian.Uint16(hdr[3:5]))
	if _, err := io.ReadFull(c2, body); err != nil {
		t.Fatal(err)
	}
	return append(append(make([]byte, 0, len(hdr)+len(body)), hdr...), body...)
}

// withExtension appends an empty extension of type typ to a one-record
// ClientHello, fixing the three lengths that cover it.
func withExtension(t *testing.T, rec []byte, typ uint16) []byte {
	t.Helper()
	out := append([]byte(nil), rec...)
	h := out[5:]
	p := 4 + 2 + 32 // handshake header, legacy_version, random
	p += 1 + int(h[p])
	p += 2 + int(binary.BigEndian.Uint16(h[p:]))
	p += 1 + int(h[p])
	extLen := binary.BigEndian.Uint16(h[p:])
	binary.BigEndian.PutUint16(h[p:], extLen+4)
	out = append(out, byte(typ>>8), byte(typ), 0, 0)
	binary.BigEndian.PutUint16(out[3:5], binary.BigEndian.Uint16(out[3:5])+4)
	hl := uint32(out[6])<<16 | uint32(out[7])<<8 | uint32(out[8])
	hl += 4
	out[6], out[7], out[8] = byte(hl>>16), byte(hl>>8), byte(hl)
	return out
}

func TestReadClientHello(t *testing.T) {
	ch := clientHelloFor(t, "awscli.amazonaws.com")
	hello, got, err := readClientHello(strings.NewReader(string(ch) + "rest"))
	if err != nil || hello.serverName != "awscli.amazonaws.com" || hello.ech {
		t.Fatalf("readClientHello = %+v, %v", hello, err)
	}
	if !strings.HasPrefix(string(ch)+"rest", string(got)) || len(got) < len(ch) {
		t.Errorf("the bytes read (%d) are not a prefix covering the ClientHello (%d)", len(got), len(ch))
	}
	if _, _, err := readClientHello(strings.NewReader("GET / HTTP/1.1\r\n\r\n")); err == nil {
		t.Error("plain HTTP was read as a ClientHello")
	}
}

func TestHelloRefusal(t *testing.T) {
	const host = "awscli.amazonaws.com"
	for _, tc := range []struct {
		hello clientHello
		err   error
		ok    bool
	}{
		{clientHello{serverName: host}, nil, true},
		{clientHello{serverName: "AWSCLI.amazonaws.com."}, nil, true},
		{clientHello{serverName: "d111111abcdef8.cloudfront.net"}, nil, false},
		{clientHello{}, nil, false},
		{clientHello{serverName: host, ech: true}, nil, false},
		{clientHello{serverName: host}, io.EOF, false},
	} {
		if why := helloRefusal(host, tc.hello, tc.err); (why == "") != tc.ok {
			t.Errorf("helloRefusal(%+v, %v) = %q, want allowed=%t", tc.hello, tc.err, why, tc.ok)
		}
	}
}

// #1595: a CONNECT to an allowlisted CDN host whose ClientHello asks the edge
// for another name (domain fronting onto any CloudFront distribution), names
// none, carries encrypted_client_hello, or is not TLS at all, is cut before a
// byte reaches the origin.
func TestConnectRefusesAClientHelloForAnotherHost(t *testing.T) {
	ech := withExtension(t, clientHelloFor(t, "awscli.amazonaws.com"), extensionECH)
	for name, first := range map[string][]byte{
		"fronted":    clientHelloFor(t, "d111111abcdef8.cloudfront.net"),
		"no SNI":     clientHelloFor(t, "192.0.2.1"),
		"ECH":        ech,
		"not TLS":    []byte("GET / HTTP/1.1\r\nHost: evil\r\n\r\n"),
		"wrong host": clientHelloFor(t, "efa-installer.amazonaws.com"),
	} {
		t.Run(name, func(t *testing.T) {
			fn := newFakeNet(echoServer(t))
			fn.set("awscli.amazonaws.com", "52.94.233.130")
			p, addr, logs := startProxy(t, fn, Config{HeaderTimeout: 2 * time.Second})
			resp, _, br := rawRequest(t, addr,
				"CONNECT awscli.amazonaws.com:443 HTTP/1.1\r\nHost: awscli.amazonaws.com:443\r\n\r\n"+string(first))
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("CONNECT status %d", resp.StatusCode)
			}
			if n, _ := io.Copy(io.Discard, br); n != 0 {
				t.Errorf("%d bytes came back through a tunnel that should have been cut", n)
			}
			waitFor(t, func() bool { return strings.Contains(logs.String(), "verdict=refused") })
			if s := p.Stats(); s.Accepted != 0 || s.Refused != 1 {
				t.Errorf("stats %+v, want 0 accepted, 1 refused", s)
			}
		})
	}
}

// A client that opens a tunnel and never sends a ClientHello holds its slot
// for the header timeout, not the idle timeout.
func TestConnectWithNoClientHelloTimesOut(t *testing.T) {
	fn := newFakeNet(echoServer(t))
	fn.set("awscli.amazonaws.com", "52.94.233.130")
	_, addr, logs := startProxy(t, fn, Config{HeaderTimeout: 200 * time.Millisecond, IdleTimeout: time.Hour})
	resp, _, br := rawRequest(t, addr, "CONNECT awscli.amazonaws.com:443 HTTP/1.1\r\nHost: x\r\n\r\n")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	start := time.Now()
	if _, err := br.ReadByte(); err == nil {
		t.Fatal("read a byte from a tunnel that sent no ClientHello")
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Errorf("closed after %v", el)
	}
	waitFor(t, func() bool { return strings.Contains(logs.String(), "verdict=refused") })
}
