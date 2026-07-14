package pluginruntime

import (
	"context"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/plugin"
	"github.com/spore-host/spawn/pkg/provider"
)

func newTestRuntime(t *testing.T) *Runtime {
	t.Helper()
	dir := t.TempDir()
	return &Runtime{
		store:         plugin.NewDiskStateStore(dir),
		executor:      NewRemoteExecutor(""),
		identity:      nil,
		healthCancels: make(map[string]context.CancelFunc),
	}
}

func saveState(t *testing.T, rt *Runtime, name string, status plugin.PluginStatus) {
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
		t.Fatalf("save state %s: %v", name, err)
	}
}

func TestRuntime_StopAll(t *testing.T) {
	rt := newTestRuntime(t)
	ctx := context.Background()

	saveState(t, rt, "alpha", plugin.StatusRunning)
	saveState(t, rt, "beta", plugin.StatusDegraded)
	saveState(t, rt, "gamma", plugin.StatusStopped) // should be untouched

	rt.StopAll(ctx)

	for _, name := range []string{"alpha", "beta"} {
		st, err := rt.store.Load(name)
		if err != nil {
			t.Fatalf("load %s: %v", name, err)
		}
		if st.Status != plugin.StatusStopped {
			t.Errorf("%s: got status %q, want %q", name, st.Status, plugin.StatusStopped)
		}
	}

	// gamma was already stopped, should still load fine.
	st, err := rt.store.Load("gamma")
	if err != nil {
		t.Fatalf("load gamma: %v", err)
	}
	if st.Status != plugin.StatusStopped {
		t.Errorf("gamma: unexpected status change to %q", st.Status)
	}
}

func TestRuntime_ReceivePush(t *testing.T) {
	rt := newTestRuntime(t)
	ctx := context.Background()

	saveState(t, rt, "myplugin", plugin.StatusWaitingForPush)

	if err := rt.ReceivePush(ctx, "myplugin", "setup_key", "secret-value"); err != nil {
		t.Fatalf("ReceivePush: %v", err)
	}

	st, err := rt.store.Load("myplugin")
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if st.Pushed["setup_key"] != "secret-value" {
		t.Errorf("pushed value not stored: got %q", st.Pushed["setup_key"])
	}
	if st.Status != plugin.StatusRunning {
		t.Errorf("status: got %q, want %q", st.Status, plugin.StatusRunning)
	}
}

func TestRuntime_ReceivePush_NonWaiting(t *testing.T) {
	rt := newTestRuntime(t)
	ctx := context.Background()

	saveState(t, rt, "myplugin", plugin.StatusRunning)

	if err := rt.ReceivePush(ctx, "myplugin", "extra_key", "val"); err != nil {
		t.Fatalf("ReceivePush: %v", err)
	}

	st, err := rt.store.Load("myplugin")
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	// Status should remain Running (not transition).
	if st.Status != plugin.StatusRunning {
		t.Errorf("status changed unexpectedly: got %q", st.Status)
	}
	if st.Pushed["extra_key"] != "val" {
		t.Errorf("pushed value not stored")
	}
}

func TestRuntime_Remove(t *testing.T) {
	rt := newTestRuntime(t)
	ctx := context.Background()

	saveState(t, rt, "myplugin", plugin.StatusRunning)

	// Add a fake health cancel to verify it's cleaned up.
	cancelCalled := false
	rt.mu.Lock()
	rt.healthCancels["myplugin"] = func() { cancelCalled = true }
	rt.mu.Unlock()

	if err := rt.Remove(ctx, "myplugin"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if !cancelCalled {
		t.Error("health loop cancel not called")
	}

	if _, err := rt.store.Load("myplugin"); err == nil {
		t.Error("state still present after Remove")
	}

	rt.mu.Lock()
	_, stillPresent := rt.healthCancels["myplugin"]
	rt.mu.Unlock()
	if stillPresent {
		t.Error("health cancel still in map after Remove")
	}
}

func TestRuntime_Remove_NotFound(t *testing.T) {
	rt := newTestRuntime(t)
	ctx := context.Background()

	err := rt.Remove(ctx, "doesnotexist")
	if err == nil {
		t.Error("expected error for missing plugin, got nil")
	}
}

func TestRuntime_TemplateContext_InstanceName(t *testing.T) {
	dir := t.TempDir()
	rt := &Runtime{
		store:    plugin.NewDiskStateStore(dir),
		executor: NewRemoteExecutor(""),
		identity: &provider.Identity{
			InstanceID: "i-0abc123",
			Name:       "my-worker",
			PublicIP:   "1.2.3.4",
		},
		healthCancels: make(map[string]context.CancelFunc),
	}

	st := &plugin.PluginState{
		Name:    "testplugin",
		Version: "1.0.0",
		Config:  make(map[string]string),
		Outputs: make(map[string]string),
		Pushed:  make(map[string]string),
	}

	tmplCtx := rt.buildTemplateContext(st)

	if tmplCtx.Instance["name"] != "my-worker" {
		t.Errorf("instance.name: got %q, want %q", tmplCtx.Instance["name"], "my-worker")
	}
	if tmplCtx.Instance["id"] != "i-0abc123" {
		t.Errorf("instance.id: got %q, want %q", tmplCtx.Instance["id"], "i-0abc123")
	}
	if tmplCtx.Instance["ip"] != "1.2.3.4" {
		t.Errorf("instance.ip: got %q, want %q", tmplCtx.Instance["ip"], "1.2.3.4")
	}
}
