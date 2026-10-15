package pluginruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/plugin"
)

// A plugin's remote.stop is the undo of its install, and `spawn plugin
// inspect` lists it under "On removal". It was validated and displayed, and
// run by nothing (#1018).

// installOverHTTP installs spec through POST /v1/plugins/install, as `spawn
// plugin install` does, and waits for it to run.
func installOverHTTP(t *testing.T, s *PushAPIServer, url string, spec *plugin.PluginSpec, pushed map[string]string) {
	t.Helper()
	body, _ := json.Marshal(map[string]interface{}{"spec": mustMarshalSpec(t, spec), "config": map[string]string{}, "pushed": pushed})
	req, _ := http.NewRequest(http.MethodPost, url+"/v1/plugins/install", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("install: HTTP %d", resp.StatusCode)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if st, err := s.rt.store.Load(spec.Name); err == nil && (st.Status == plugin.StatusRunning || st.Status == plugin.StatusFailed) {
			if st.Status != plugin.StatusRunning {
				t.Fatalf("install failed: %s", st.Error)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("install did not finish")
}

// removeOverHTTP is DELETE /v1/plugins/{name}, which `spawn plugin remove` calls.
func removeOverHTTP(t *testing.T, url, name string) (int, map[string]string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, url+"/v1/plugins/"+name, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}

// The issue's scenario: a plugin that starts a service is removed, and its
// stop steps must run, with the plugin's template values.
func TestPluginRemoveRunsTheStopSteps(t *testing.T) {
	s, srv := newTestServer(t)
	dir := t.TempDir()
	started, stopped := filepath.Join(dir, "started"), filepath.Join(dir, "stopped")
	spec := &plugin.PluginSpec{Name: "svc", Version: "v1.0.0", Description: "d", Remote: plugin.RemoteBlock{
		Start: []plugin.Step{{Type: "run", Run: "touch " + started}},
		Stop:  []plugin.Step{{Type: "run", Run: "printf 'stopped %s' {{ pushed.k }} > " + stopped}},
	}}
	installOverHTTP(t, s, srv.URL, spec, map[string]string{"k": "V"})
	if _, err := os.Stat(stopped); err == nil {
		t.Fatal("setup: the stop step ran at install")
	}

	code, body := removeOverHTTP(t, srv.URL, "svc")
	if code != http.StatusOK || body["warning"] != "" {
		t.Fatalf("remove: HTTP %d %v, want 200 with no warning", code, body)
	}
	got, err := os.ReadFile(stopped)
	if err != nil {
		t.Fatalf("spawn plugin remove did not run remote.stop: %v", err)
	}
	if string(got) != "stopped V" {
		t.Errorf("stop step wrote %q, want %q (rendered with the plugin's values)", got, "stopped V")
	}
	if _, err := s.rt.store.Load("svc"); err == nil {
		t.Error("plugin state still present after remove")
	}
}

// A stop step that fails does not keep the plugin installed; the response
// says the stop failed, so the CLI can warn.
func TestPluginRemoveWithAFailingStopStepRemovesAndWarns(t *testing.T) {
	s, srv := newTestServer(t)
	seedPlugin(t, s.rt, "svc", plugin.StatusRunning)
	st, _ := s.rt.store.Load("svc")
	st.StopSteps = []plugin.Step{{Type: "run", Run: "exit 3"}}
	if err := s.rt.store.Save(st); err != nil {
		t.Fatal(err)
	}

	code, body := removeOverHTTP(t, srv.URL, "svc")
	if code != http.StatusOK {
		t.Fatalf("remove with a failing stop step: HTTP %d, want 200 (removed)", code)
	}
	if body["status"] != "removed" || body["warning"] == "" {
		t.Errorf("remove response = %v, want status removed and a warning", body)
	}
	if _, err := s.rt.store.Load("svc"); err == nil {
		t.Error("a failing stop step kept the plugin installed")
	}
}

func TestRemoveReportsAFailedStopAsErrStopStepsFailed(t *testing.T) {
	rt := newTestRuntime(t)
	saveState(t, rt, "svc", plugin.StatusRunning)
	st, _ := rt.store.Load("svc")
	st.StopSteps = []plugin.Step{{Type: "run", Run: "exit 3"}}
	_ = rt.store.Save(st)
	if err := rt.Remove(context.Background(), "svc"); !errors.Is(err, ErrStopStepsFailed) {
		t.Errorf("Remove = %v, want ErrStopStepsFailed", err)
	}
}

// StopAll (spored's Cleanup before a stop, hibernate or terminate) must NOT
// run remote.stop: nothing re-runs remote.start when the instance comes back,
// so `tailscale down` there would keep the node off the tailnet after a
// `spawn start`, which the Tailscale plugin documents it relies on (the node
// "reconnects on boot"). `spawn plugin inspect` says so too.
func TestStopAllDoesNotRunTheStopSteps(t *testing.T) {
	rt := newTestRuntime(t)
	saveState(t, rt, "svc", plugin.StatusRunning)
	marker := filepath.Join(t.TempDir(), "stopped")
	st, _ := rt.store.Load("svc")
	st.StopSteps = []plugin.Step{{Type: "run", Run: "touch " + marker}}
	_ = rt.store.Save(st)

	rt.StopAll(context.Background())

	if _, err := os.Stat(marker); err == nil {
		t.Error("StopAll ran remote.stop; an instance stop would leave the plugin's service down after the next start")
	}
	if st, _ := rt.store.Load("svc"); st.Status != plugin.StatusStopped {
		t.Errorf("status after StopAll = %q, want stopped", st.Status)
	}
}
