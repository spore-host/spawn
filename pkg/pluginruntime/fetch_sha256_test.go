package pluginruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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

	// #838: an unpinned artifact is refused before a byte is requested, and
	// nothing is left at dest for a later step to run.
	t.Run("no checksum is refused before download", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "out.bin")
		err := e.fetch(ctx, plugin.Step{Type: "fetch", URL: url, Dest: dest})
		if !errors.Is(err, plugin.ErrUnpinnedArtifact) {
			t.Fatalf("fetch without sha256: err = %v, want ErrUnpinnedArtifact", err)
		}
		if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
			t.Errorf("expected no file at dest, stat err = %v", statErr)
		}
	})

	t.Run("exact size passes", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "out.bin")
		if err := e.fetch(ctx, plugin.Step{Type: "fetch", URL: url, Dest: dest, SHA256: digest, Size: int64(len(payload))}); err != nil {
			t.Fatalf("fetch with exact size: %v", err)
		}
	})

	for name, size := range map[string]int64{"shorter": int64(len(payload)) - 1, "longer": int64(len(payload)) + 1} {
		t.Run("size mismatch ("+name+" than served) fails and removes file", func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "out.bin")
			err := e.fetch(ctx, plugin.Step{Type: "fetch", URL: url, Dest: dest, SHA256: digest, Size: size})
			if err == nil || !strings.Contains(err.Error(), "size mismatch") {
				t.Fatalf("err = %v, want a size mismatch", err)
			}
			if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
				t.Errorf("expected dest removed on size mismatch, stat err = %v", statErr)
			}
		})
	}
}

// A tampered artifact (the URL now serves different bytes than the spec
// pinned) is refused, and the install stops there: the step that would have
// run it never runs.
func TestInstall_TamperedArtifactRefused(t *testing.T) {
	pinned := []byte("the released artifact\n")
	served := []byte("a substituted artifact\n")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(served) // nosemgrep: no-direct-write-to-responsewriter -- test fixture serving a static payload
	}))
	t.Cleanup(srv.Close)
	orig := fetchClient
	fetchClient = newFetchTestClient(srv)
	t.Cleanup(func() { fetchClient = orig })

	sum := sha256.Sum256(pinned)
	dir := t.TempDir()
	dest := filepath.Join(dir, "tool.bin")
	marker := filepath.Join(dir, "ran")
	spec := &plugin.PluginSpec{
		Name: "tampered", Version: "v1.0.0", Description: "d",
		Remote: plugin.RemoteBlock{Install: []plugin.Step{
			{Type: "fetch", URL: "https://example.com/tool.bin", Dest: dest, SHA256: hex.EncodeToString(sum[:])},
			{Type: "run", Run: "touch " + marker},
		}},
	}
	rt := NewRuntimeWithStore(plugin.NewDiskStateStore(t.TempDir()), nil, "")
	err := rt.Install(context.Background(), spec, nil)
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("Install err = %v, want a sha256 mismatch", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Errorf("tampered artifact left at dest (stat err = %v)", statErr)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Errorf("the step after the refused fetch ran")
	}
}

// A spec with an unpinned fetch is refused before ANY step runs, even one that
// comes before the fetch, and the reason is recorded in its state.
func TestInstall_UnpinnedArtifactRefusedBeforeAnyStep(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	spec := &plugin.PluginSpec{
		Name: "unpinned", Version: "v1.0.0", Description: "d",
		Remote: plugin.RemoteBlock{Install: []plugin.Step{
			{Type: "run", Run: "touch " + marker},
			{Type: "fetch", URL: "https://example.com/tool.bin", Dest: filepath.Join(dir, "tool.bin")},
		}},
	}
	store := plugin.NewDiskStateStore(t.TempDir())
	rt := NewRuntimeWithStore(store, nil, "")
	err := rt.Install(context.Background(), spec, nil)
	if !errors.Is(err, plugin.ErrUnpinnedArtifact) {
		t.Fatalf("Install err = %v, want ErrUnpinnedArtifact", err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Errorf("a step ran before the unpinned fetch was refused")
	}
	st, lerr := store.Load("unpinned")
	if lerr != nil {
		t.Fatalf("load state: %v", lerr)
	}
	if st.Status != plugin.StatusFailed || !strings.Contains(st.Error, "sha256") {
		t.Errorf("state = %s %q, want failed naming the missing sha256", st.Status, st.Error)
	}
}

// A fetch with arch: runs only on that architecture, so one spec can pin a
// different artifact per architecture.
func TestFetch_ArchSelectsArtifact(t *testing.T) {
	payload := []byte("arch artifact\n")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload) // nosemgrep: no-direct-write-to-responsewriter -- test fixture serving a static payload
	}))
	t.Cleanup(srv.Close)
	orig := fetchClient
	fetchClient = newFetchTestClient(srv)
	t.Cleanup(func() { fetchClient = orig })
	origArch := hostGOARCH
	t.Cleanup(func() { hostGOARCH = origArch })

	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])
	wrong := strings.Repeat("0", 64) // the other arch's pin: never checked here
	for _, host := range []string{"amd64", "arm64"} {
		t.Run(host, func(t *testing.T) {
			hostGOARCH = host
			dir := t.TempDir()
			x86 := plugin.Step{Type: "fetch", Arch: "x86_64", URL: "https://example.com/x86", Dest: filepath.Join(dir, "x86"), SHA256: digest}
			arm := plugin.Step{Type: "fetch", Arch: "arm64", URL: "https://example.com/arm", Dest: filepath.Join(dir, "arm"), SHA256: digest}
			if host == "amd64" {
				arm.SHA256 = wrong
			} else {
				x86.SHA256 = wrong
			}
			if err := NewRemoteExecutor("").RunSteps(context.Background(), []plugin.Step{x86, arm}, plugin.NewTemplateContext()); err != nil {
				t.Fatalf("RunSteps: %v", err)
			}
			want, skip := "x86", "arm"
			if host == "arm64" {
				want, skip = "arm", "x86"
			}
			if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
				t.Errorf("%s artifact not fetched on %s: %v", want, host, err)
			}
			if _, err := os.Stat(filepath.Join(dir, skip)); !os.IsNotExist(err) {
				t.Errorf("%s artifact fetched on %s", skip, host)
			}
		})
	}
}
