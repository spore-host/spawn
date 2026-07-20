package pluginruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/plugin"
)

// newFetchTestClient returns an *http.Client that trusts the httptest server's
// cert (valid for example.com) and dials it regardless of the requested host, so
// tests can fetch from "https://example.com/..." — a host that passes
// validateFetchURL's loopback/private-IP guard — without a real DNS name or
// disabling TLS verification.
func newFetchTestClient(srv *httptest.Server) *http.Client {
	c := srv.Client()
	tr := c.Transport.(*http.Transport).Clone()
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
	}
	c.Transport = tr
	return c
}

func TestFetch_SHA256(t *testing.T) {
	payload := []byte("the transitive download bytes\n")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload) // nosemgrep: no-direct-write-to-responsewriter -- test fixture serving a static payload
	}))
	t.Cleanup(srv.Close)
	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])

	orig := fetchClient
	fetchClient = newFetchTestClient(srv)
	t.Cleanup(func() { fetchClient = orig })

	// example.com passes validateFetchURL (public host, https); the test client
	// dials the httptest server and trusts its cert.
	const url = "https://example.com/tool.bin"

	e := NewRemoteExecutor("")
	ctx := context.Background()

	t.Run("match passes and writes file", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "out.bin")
		err := e.fetch(ctx, plugin.Step{Type: "fetch", URL: url, Dest: dest, SHA256: digest})
		if err != nil {
			t.Fatalf("fetch: unexpected error: %v", err)
		}
		got, err := os.ReadFile(dest)
		if err != nil {
			t.Fatalf("read dest: %v", err)
		}
		if string(got) != string(payload) {
			t.Errorf("content = %q, want %q", got, payload)
		}
	})

	t.Run("mismatch fails and removes file", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "out.bin")
		wrong := strings.Repeat("0", 64)
		err := e.fetch(ctx, plugin.Step{Type: "fetch", URL: url, Dest: dest, SHA256: wrong})
		if err == nil {
			t.Fatal("expected sha256 mismatch error, got nil")
		}
		if !strings.Contains(err.Error(), "sha256 mismatch") {
			t.Errorf("error %q does not mention sha256 mismatch", err)
		}
		if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
			t.Errorf("expected dest removed on mismatch, stat err = %v", statErr)
		}
	})

	t.Run("no checksum still downloads", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "out.bin")
		if err := e.fetch(ctx, plugin.Step{Type: "fetch", URL: url, Dest: dest}); err != nil {
			t.Fatalf("fetch without sha256: unexpected error: %v", err)
		}
		if _, err := os.Stat(dest); err != nil {
			t.Errorf("expected file written, stat err = %v", err)
		}
	})
}
