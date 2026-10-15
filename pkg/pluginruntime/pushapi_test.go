package pluginruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/plugin"
)

// newTestServer creates a PushAPIServer backed by an in-memory store and
// returns an httptest.Server for use in tests.
func newTestServer(t *testing.T) (*PushAPIServer, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	store := plugin.NewDiskStateStore(dir)
	rt := &Runtime{
		store:         store,
		executor:      NewRemoteExecutor(""),
		identity:      nil,
		healthCancels: make(map[string]context.CancelFunc),
	}
	s := &PushAPIServer{rt: rt, token: "test-token", baseCtx: context.Background()}
	srv := httptest.NewServer(s.authMiddleware(s.routes()))
	t.Cleanup(srv.Close)
	return s, srv
}

func TestPushAPI_Auth(t *testing.T) {
	_, srv := newTestServer(t)
	url := srv.URL + "/v1/plugins"

	tests := []struct {
		name   string
		auth   string
		wantSC int
	}{
		{"no auth", "", http.StatusUnauthorized},
		{"wrong token", "Bearer wrong", http.StatusForbidden},
		{"correct token", "Bearer test-token", http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, url, nil)
			if tt.auth != "" {
				req.Header.Set("Authorization", tt.auth)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != tt.wantSC {
				t.Errorf("got %d, want %d", resp.StatusCode, tt.wantSC)
			}
		})
	}
}

// seedPlugin writes a PluginState directly to the store for test setup.
func seedPlugin(t *testing.T, rt *Runtime, name string, status plugin.PluginStatus) {
	t.Helper()
	st := &plugin.PluginState{
		Name:        name,
		Version:     "1.0.0",
		Status:      status,
		Config:      make(map[string]string),
		Outputs:     make(map[string]string),
		Pushed:      make(map[string]string),
		InstalledAt: time.Now(),
	}
	if err := rt.store.Save(st); err != nil {
		t.Fatalf("seed plugin: %v", err)
	}
}

func TestPushAPI_Push(t *testing.T) {
	s, srv := newTestServer(t)
	seedPlugin(t, s.rt, "foo", plugin.StatusWaitingForPush)

	body, _ := json.Marshal(map[string]string{"key": "token", "value": "abc123"})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/plugins/foo/push", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}

	st, err := s.rt.store.Load("foo")
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if st.Pushed["token"] != "abc123" {
		t.Errorf("pushed value not stored: got %q", st.Pushed["token"])
	}
	if st.Status != plugin.StatusRunning {
		t.Errorf("status not transitioned: got %q", st.Status)
	}
}

func TestPushAPI_List(t *testing.T) {
	s, srv := newTestServer(t)
	seedPlugin(t, s.rt, "foo", plugin.StatusRunning)
	seedPlugin(t, s.rt, "bar", plugin.StatusStopped)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/plugins", nil)
	req.Header.Set("Authorization", "Bearer test-token")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}

	var states []*plugin.PluginState
	if err := json.NewDecoder(resp.Body).Decode(&states); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(states) != 2 {
		t.Errorf("got %d plugins, want 2", len(states))
	}
}

func TestPushAPI_Status(t *testing.T) {
	s, srv := newTestServer(t)
	seedPlugin(t, s.rt, "foo", plugin.StatusRunning)

	tests := []struct {
		name   string
		plugin string
		wantSC int
	}{
		{"existing plugin", "foo", http.StatusOK},
		{"missing plugin", "notexist", http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/plugins/"+tt.plugin+"/status", nil)
			req.Header.Set("Authorization", "Bearer test-token")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != tt.wantSC {
				t.Errorf("got %d, want %d", resp.StatusCode, tt.wantSC)
			}
		})
	}
}

func TestPushAPI_Remove(t *testing.T) {
	s, srv := newTestServer(t)
	seedPlugin(t, s.rt, "foo", plugin.StatusRunning)

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v1/plugins/foo", nil)
	req.Header.Set("Authorization", "Bearer test-token")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}

	// State should be gone.
	if _, err := s.rt.store.Load("foo"); err == nil {
		t.Error("plugin state still present after remove")
	}
}

// do sends an authenticated request to the test server and returns the status.
func do(t *testing.T, srv *httptest.Server, method, path string, body []byte) int {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// corruptPlugin writes an undecodable state file for name: the plugin is
// there, but spored cannot read it. That is a server error, not "not found".
func corruptPlugin(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, name), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name, "state.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// #1038: every per-plugin endpoint answers 404 for a plugin that is not
// installed — the CLI turns that into `plugin "x" not found on <instance>` —
// and 500 only for a real failure. remove used to answer 500 for both, and
// status 404 for both.
func TestPushAPI_NotInstalledIs404_OtherFailuresAre500(t *testing.T) {
	dir := t.TempDir()
	rt := &Runtime{
		store:         plugin.NewDiskStateStore(dir),
		executor:      NewRemoteExecutor(""),
		healthCancels: make(map[string]context.CancelFunc),
	}
	s := &PushAPIServer{rt: rt, token: "test-token", baseCtx: context.Background()}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/plugins/{name}/push", s.handlePush)
	mux.HandleFunc("GET /v1/plugins/{name}/status", s.handleStatus)
	mux.HandleFunc("DELETE /v1/plugins/{name}", s.handleRemove)
	srv := httptest.NewServer(s.authMiddleware(mux))
	t.Cleanup(srv.Close)

	corruptPlugin(t, dir, "broken")
	push, _ := json.Marshal(map[string]string{"key": "token", "value": "v"})

	for _, tt := range []struct {
		name   string
		method string
		path   string
		body   []byte
		want   int
	}{
		{"remove, not installed", http.MethodDelete, "/v1/plugins/absent", nil, http.StatusNotFound},
		{"status, not installed", http.MethodGet, "/v1/plugins/absent/status", nil, http.StatusNotFound},
		{"push, not installed", http.MethodPost, "/v1/plugins/absent/push", push, http.StatusNotFound},
		{"remove, unreadable state", http.MethodDelete, "/v1/plugins/broken", nil, http.StatusInternalServerError},
		{"status, unreadable state", http.MethodGet, "/v1/plugins/broken/status", nil, http.StatusInternalServerError},
		{"push, unreadable state", http.MethodPost, "/v1/plugins/broken/push", push, http.StatusInternalServerError},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := do(t, srv, tt.method, tt.path, tt.body); got != tt.want {
				t.Errorf("%s %s = %d, want %d", tt.method, tt.path, got, tt.want)
			}
		})
	}
}

// The runtime itself reports "not installed" as a typed error, so callers
// other than the push API can tell it from a failure too.
func TestRuntime_RemoveNotInstalled_IsErrNotInstalled(t *testing.T) {
	dir := t.TempDir()
	rt := &Runtime{
		store:         plugin.NewDiskStateStore(dir),
		executor:      NewRemoteExecutor(""),
		healthCancels: make(map[string]context.CancelFunc),
	}
	if err := rt.Remove(context.Background(), "absent"); !errors.Is(err, plugin.ErrNotInstalled) {
		t.Errorf("Remove(absent) = %v, want an error wrapping plugin.ErrNotInstalled", err)
	}
	if _, err := rt.Status("absent"); !errors.Is(err, plugin.ErrNotInstalled) {
		t.Errorf("Status(absent) = %v, want an error wrapping plugin.ErrNotInstalled", err)
	}
	corruptPlugin(t, dir, "broken")
	if err := rt.Remove(context.Background(), "broken"); err == nil || errors.Is(err, plugin.ErrNotInstalled) {
		t.Errorf("Remove(broken) = %v, want a failure that is not ErrNotInstalled", err)
	}
}
