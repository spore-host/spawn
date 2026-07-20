package pluginruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/plugin"
	"gopkg.in/yaml.v3"
)

// TestInstallWithPushed_SeedsPushedBeforeConfigure verifies the core of the
// unified install flow: a value "pushed" by the controller is available to the
// remote configure phase via {{ pushed.<key> }}, so the plugin reaches Running
// without ever parking at WaitingForPush. The configure step writes the pushed
// value to a file; the test asserts the file content.
func TestInstallWithPushed_SeedsPushedBeforeConfigure(t *testing.T) {
	rt := newTestRuntime(t)
	out := filepath.Join(t.TempDir(), "configured")

	spec := &plugin.PluginSpec{
		Name:        "seeded",
		Version:     "v1.0.0",
		Description: "d",
		Remote: plugin.RemoteBlock{
			Install: []plugin.Step{{Type: "run", Run: "true"}},
			// configure consumes the pushed value.
			Configure: []plugin.Step{{Type: "run", Run: "printf '%s' {{ pushed.setup_key }} > " + out}},
			Start:     []plugin.Step{{Type: "run", Run: "true"}},
		},
	}

	err := rt.InstallWithPushed(context.Background(), spec, nil, map[string]string{"setup_key": "SEEDED123"})
	if err != nil {
		t.Fatalf("InstallWithPushed: %v", err)
	}

	st, err := rt.store.Load("seeded")
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if st.Status != plugin.StatusRunning {
		t.Errorf("status = %q, want running (error: %s)", st.Status, st.Error)
	}
	if st.Pushed["setup_key"] != "SEEDED123" {
		t.Errorf("pushed[setup_key] = %q, want SEEDED123", st.Pushed["setup_key"])
	}

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read configure output: %v", err)
	}
	if string(got) != "SEEDED123" {
		t.Errorf("configure wrote %q, want SEEDED123 (pushed value did not reach configure)", string(got))
	}
}

// TestInstallWithProvenance_PersistsToState verifies that a resolved Provenance
// handed to the runtime is recorded in the plugin's on-instance state, so an
// audit can read what was installed and how it was verified.
func TestInstallWithProvenance_PersistsToState(t *testing.T) {
	rt := newTestRuntime(t)
	spec := &plugin.PluginSpec{
		Name:        "provrec",
		Version:     "v1.0.0",
		Description: "d",
		Remote:      plugin.RemoteBlock{Install: []plugin.Step{{Type: "run", Run: "true"}}},
	}
	prov := &plugin.Provenance{
		Host:              "official",
		Name:              "provrec",
		CommitSHA:         "2dda4ab8dc6f734699112473f011e34fbf862f2b",
		ContentSHA256:     "0bffc628dd64b12920bef7561ff105c0f2ea23b2fe370db3be07cccda99561d0",
		ManifestVerified:  true,
		SignatureVerified: true,
		ReleaseTag:        "provrec-v1.0.0",
	}

	if err := rt.InstallWithProvenance(context.Background(), spec, nil, nil, prov); err != nil {
		t.Fatalf("InstallWithProvenance: %v", err)
	}
	st, err := rt.store.Load("provrec")
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if st.Provenance == nil {
		t.Fatal("state.Provenance = nil, want the recorded provenance")
	}
	if !st.Provenance.SignatureVerified || st.Provenance.CommitSHA != prov.CommitSHA {
		t.Errorf("state.Provenance = %+v, want signature-verified with commit %s", st.Provenance, prov.CommitSHA)
	}
}

// TestInstall_MissingPushedParksWaiting confirms the documented limitation: with
// no pushed value seeded, a configure step referencing {{ pushed.x }} parks the
// plugin at WaitingForPush rather than failing.
func TestInstall_MissingPushedParksWaiting(t *testing.T) {
	rt := newTestRuntime(t)
	spec := &plugin.PluginSpec{
		Name:        "waits",
		Version:     "v1.0.0",
		Description: "d",
		Remote: plugin.RemoteBlock{
			Configure: []plugin.Step{{Type: "run", Run: "echo {{ pushed.setup_key }}"}},
		},
	}
	if err := rt.Install(context.Background(), spec, nil); err != nil {
		t.Fatalf("Install: %v", err)
	}
	st, err := rt.store.Load("waits")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if st.Status != plugin.StatusWaitingForPush {
		t.Errorf("status = %q, want waiting_for_push", st.Status)
	}
}

// TestHandleInstall_EndToEnd drives the install endpoint the way the CLI does:
// POST a resolved spec + config + pushed, expect 202, then poll status to
// Running.
func TestHandleInstall_EndToEnd(t *testing.T) {
	s, srv := newTestServer(t)
	out := filepath.Join(t.TempDir(), "endtoend")

	spec := &plugin.PluginSpec{
		Name:        "e2e",
		Version:     "v1.0.0",
		Description: "d",
		Remote: plugin.RemoteBlock{
			Install:   []plugin.Step{{Type: "run", Run: "true"}},
			Configure: []plugin.Step{{Type: "run", Run: "printf '%s' {{ pushed.k }} > " + out}},
			Start:     []plugin.Step{{Type: "run", Run: "true"}},
		},
	}
	specYAML := mustMarshalSpec(t, spec)

	body, _ := json.Marshal(map[string]interface{}{
		"spec":   specYAML,
		"config": map[string]string{},
		"pushed": map[string]string{"k": "V"},
	})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/plugins/install", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("install request: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("install: got %d, want 202", resp.StatusCode)
	}

	// Poll for the async install to finish.
	deadline := time.Now().Add(10 * time.Second)
	var st *plugin.PluginState
	for time.Now().Before(deadline) {
		st, err = s.rt.store.Load("e2e")
		if err == nil && (st.Status == plugin.StatusRunning || st.Status == plugin.StatusFailed) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if st == nil || st.Status != plugin.StatusRunning {
		t.Fatalf("plugin did not reach running: %+v", st)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read configure output: %v", err)
	}
	if string(got) != "V" {
		t.Errorf("configure wrote %q, want V", string(got))
	}
}

// TestHandleInstall_RejectsInvalidSpec verifies the endpoint validates the spec
// (a non-canonical template reference must be rejected up front, not silently
// installed).
func TestHandleInstall_RejectsInvalidSpec(t *testing.T) {
	_, srv := newTestServer(t)

	badSpec := "name: bad\nversion: v1.0.0\ndescription: d\nremote:\n  start:\n    - type: run\n      run: serve {{ .Config.x }}\n"
	body, _ := json.Marshal(map[string]interface{}{"spec": badSpec})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/plugins/install", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("install request: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("install with invalid spec: got %d, want 400", resp.StatusCode)
	}
}

func mustMarshalSpec(t *testing.T, spec *plugin.PluginSpec) string {
	t.Helper()
	b, err := yaml.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	return string(b)
}
