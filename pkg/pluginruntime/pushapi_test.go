package pluginruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		executor:      NewRemoteExecutor(),
		identity:      nil,
		healthCancels: make(map[string]context.CancelFunc),
	}
	s := &PushAPIServer{rt: rt, token: "test-token", baseCtx: context.Background()}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/plugins/install", s.handleInstall)
	mux.HandleFunc("POST /v1/plugins/{name}/push", s.handlePush)
	mux.HandleFunc("GET /v1/plugins/{name}/status", s.handleStatus)
	mux.HandleFunc("GET /v1/plugins", s.handleList)
	mux.HandleFunc("DELETE /v1/plugins/{name}", s.handleRemove)
	srv := httptest.NewServer(s.authMiddleware(mux))
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
