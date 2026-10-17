package pluginruntime

import (
	"context"
	"testing"

	"github.com/spore-host/spawn/pkg/plugin"
)

// A health tick whose loop has been stopped must write nothing. StopAll saves
// "stopped" outside rt.mu, so a tick that wrote after stopHealthLoop could put
// "running" back over it, and ResumeStopped would then skip the plugin and
// never restore its health loop (#979). The interleaving is too narrow to hit
// on demand (CI hit it once in TestResumeStoppedRestoresStatusAndHealthLoop),
// so this checks the invariant that closes it directly.
func TestHealthTickAfterItsLoopStoppedWritesNothing(t *testing.T) {
	rt := newTestRuntime(t)
	stopped, cancel := context.WithCancel(context.Background())
	cancel()

	saveState(t, rt, "ok", plugin.StatusDegraded)
	rt.recordHealthOK(stopped, "ok")
	if st, _ := rt.store.Load("ok"); st.Status != plugin.StatusDegraded || st.LastHealth != nil {
		t.Errorf("recordHealthOK after its loop stopped wrote status=%q last_health=%v; want nothing written", st.Status, st.LastHealth)
	}

	saveState(t, rt, "bad", plugin.StatusStopped)
	rt.setPluginStatus(stopped, "bad", plugin.StatusDegraded, "boom")
	if st, _ := rt.store.Load("bad"); st.Status != plugin.StatusStopped || st.Error != "" {
		t.Errorf("setPluginStatus after its loop stopped wrote status=%q error=%q; want nothing written", st.Status, st.Error)
	}

	// Control: a live loop's tick still records.
	live := context.Background()
	rt.recordHealthOK(live, "ok")
	if st, _ := rt.store.Load("ok"); st.Status != plugin.StatusRunning || st.LastHealth == nil {
		t.Errorf("live recordHealthOK: status=%q last_health=%v; want running and stamped", st.Status, st.LastHealth)
	}
	rt.setPluginStatus(live, "bad", plugin.StatusDegraded, "boom")
	if st, _ := rt.store.Load("bad"); st.Status != plugin.StatusDegraded {
		t.Errorf("live setPluginStatus: status=%q; want degraded", st.Status)
	}
}
