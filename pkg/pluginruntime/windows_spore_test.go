package pluginruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/plugin"
)

// posixSporeOnly skips a test that executes plugin steps when the host is
// Windows: the runtime refuses plugins there (#1001), so there is nothing to
// execute. The refusal itself is tested below on every OS.
func posixSporeOnly(t *testing.T) {
	t.Helper()
	if plugin.CheckSporeOS(hostGOOS) != nil {
		t.Skip("plugins are Linux-only; a Windows spore refuses them (#1001)")
	}
}

// asWindowsSpore makes the runtime believe it is on a Windows spore.
func asWindowsSpore(t *testing.T) {
	t.Helper()
	old := hostGOOS
	hostGOOS = "windows"
	t.Cleanup(func() { hostGOOS = old })
}

// stepMarkerSpec returns a spec whose every phase would create marker if run.
func stepMarkerSpec(marker string) *plugin.PluginSpec {
	touch := []plugin.Step{{Type: "run", Run: "echo ran > " + marker}}
	return &plugin.PluginSpec{
		Name: "win-refused", Version: "v1.0.0", Description: "d",
		Remote: plugin.RemoteBlock{Install: touch, Configure: touch, Start: touch},
	}
}

// On a Windows spore an install fails up front with the documented error,
// runs no step and records no state (#1001). Before, it recorded "installing"
// and then failed at the first step with "exec: sh: not found".
func TestInstall_RefusedOnWindowsSpore(t *testing.T) {
	asWindowsSpore(t)
	rt := newTestRuntime(t)
	marker := filepath.Join(t.TempDir(), "ran")

	err := rt.InstallWithProvenance(context.Background(), stepMarkerSpec(marker), nil, nil, nil)
	if !errors.Is(err, plugin.ErrWindowsSporeUnsupported) {
		t.Fatalf("InstallWithProvenance on a Windows spore = %v, want ErrWindowsSporeUnsupported", err)
	}
	if _, serr := os.Stat(marker); serr == nil {
		t.Error("a plugin step ran on a Windows spore")
	}
	if _, lerr := rt.store.Load("win-refused"); lerr == nil {
		t.Error("state was recorded for a refused install")
	}
}

// The push API refuses synchronously, with the reason in the body, so even an
// older spawn that does not check spawn:os itself prints why (it surfaces the
// body of any non-202 response).
func TestHandleInstall_RefusedOnWindowsSpore(t *testing.T) {
	asWindowsSpore(t)
	s, srv := newTestServer(t)
	marker := filepath.Join(t.TempDir(), "ran")

	body, _ := json.Marshal(map[string]interface{}{"spec": mustMarshalSpec(t, stepMarkerSpec(marker))})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/plugins/install", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("install request: %v", err)
	}
	msg, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("install on a Windows spore: HTTP %d, want 501", resp.StatusCode)
	}
	if !strings.Contains(string(msg), plugin.ErrWindowsSporeUnsupported.Error()) {
		t.Errorf("response body %q does not explain the refusal", msg)
	}
	if _, lerr := s.rt.store.Load("win-refused"); lerr == nil {
		t.Error("state was recorded for a refused install")
	}
}

type countingResolver struct {
	n      atomic.Int32
	called chan struct{}
}

func (c *countingResolver) Resolve(context.Context, string) (*plugin.PluginSpec, error) {
	if c.n.Add(1) == 1 {
		close(c.called)
	}
	return nil, errors.New("not used")
}

func (c *countingResolver) ResolveWithProvenance(ctx context.Context, ref string) (*plugin.PluginSpec, *plugin.Provenance, error) {
	_, err := c.Resolve(ctx, ref)
	return nil, nil, err
}

// Launch-time declarations are not even resolved (no registry fetch) on a
// Windows spore.
func TestLoadFromDeclarations_SkippedOnWindowsSpore(t *testing.T) {
	asWindowsSpore(t)
	rt := newTestRuntime(t)
	res := &countingResolver{called: make(chan struct{})}
	rt.LoadFromDeclarations(context.Background(), []plugin.Declaration{{Ref: "docker"}}, res)
	// Installs run in goroutines, so give one a moment to show up.
	select {
	case <-res.called:
		t.Error("a declaration was resolved on a Windows spore")
	case <-time.After(300 * time.Millisecond):
	}
}
