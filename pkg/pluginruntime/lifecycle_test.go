package pluginruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/spore-host/spawn/pkg/plugin"
)

// TestPlugin_Lifecycle exercises the full sequential plugin lifecycle through
// the push API: seed → push → status → list → remove → status-404.
// This integration test verifies that state transitions, persistence, and HTTP
// handlers work correctly end-to-end using an in-process httptest server.
func TestPlugin_Lifecycle(t *testing.T) {
	s, srv := newTestServer(t)
	ctx := context.Background()
	_ = ctx

	const name = "my-plugin"
	auth := "Bearer test-token"

	// 1. Seed plugin in WaitingForPush (simulates post-install state).
	seedPlugin(t, s.rt, name, plugin.StatusWaitingForPush)

	// 2. Push a key — should transition state to Running.
	body, _ := json.Marshal(map[string]string{"key": "api_token", "value": "secret123"})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/plugins/"+name+"/push", bytes.NewReader(body))
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("push: got %d, want 200", resp.StatusCode)
	}

	st, err := s.rt.store.Load(name)
	if err != nil {
		t.Fatalf("load after push: %v", err)
	}
	if st.Status != plugin.StatusRunning {
		t.Errorf("after push: status = %q, want %q", st.Status, plugin.StatusRunning)
	}
	if st.Pushed["api_token"] != "secret123" {
		t.Errorf("after push: pushed[api_token] = %q, want %q", st.Pushed["api_token"], "secret123")
	}

	// 3. GET status — should return 200 with Running state.
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/v1/plugins/"+name+"/status", nil)
	req.Header.Set("Authorization", auth)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	var statusState plugin.PluginState
	if err := json.NewDecoder(resp.Body).Decode(&statusState); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if statusState.Status != plugin.StatusRunning {
		t.Errorf("status response: status = %q, want %q", statusState.Status, plugin.StatusRunning)
	}

	// 4. GET list — plugin should appear.
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/v1/plugins", nil)
	req.Header.Set("Authorization", auth)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: got %d, want 200", resp.StatusCode)
	}
	var listed []*plugin.PluginState
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	found := false
	for _, p := range listed {
		if p.Name == name {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("list: plugin %q not found in %d results", name, len(listed))
	}

	// 5. DELETE — should remove plugin state.
	req, _ = http.NewRequest(http.MethodDelete, srv.URL+"/v1/plugins/"+name, nil)
	req.Header.Set("Authorization", auth)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("remove: got %d, want 200", resp.StatusCode)
	}

	// 6. GET status after remove — should return 404.
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/v1/plugins/"+name+"/status", nil)
	req.Header.Set("Authorization", auth)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("status after remove: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status after remove: got %d, want 404", resp.StatusCode)
	}
}

// TestPlugin_PushToRunning verifies that pushing a key to an already-Running
// plugin stores the value without changing the status.
func TestPlugin_PushToRunning(t *testing.T) {
	s, srv := newTestServer(t)

	seedPlugin(t, s.rt, "bar", plugin.StatusRunning)

	body, _ := json.Marshal(map[string]string{"key": "extra_key", "value": "val42"})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/plugins/bar/push", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("push to running: got %d, want 200", resp.StatusCode)
	}

	st, err := s.rt.store.Load("bar")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if st.Status != plugin.StatusRunning {
		t.Errorf("status changed unexpectedly: got %q, want %q", st.Status, plugin.StatusRunning)
	}
	if st.Pushed["extra_key"] != "val42" {
		t.Errorf("pushed[extra_key] = %q, want %q", st.Pushed["extra_key"], "val42")
	}
}
