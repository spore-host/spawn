package pluginruntime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/plugin"
)

// healthTicks counts the lines a health step has appended to its file.
func healthTicks(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	return n
}

// waitForTicks waits up to 3s for the health file to reach want lines.
func waitForTicks(t *testing.T, path string, want int) bool {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if healthTicks(t, path) >= want {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func status(t *testing.T, rt *Runtime, name string) plugin.PluginStatus {
	t.Helper()
	st, err := rt.store.Load(name)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return st.Status
}

// A stop that StopAll prepared for can fail, leaving the instance running.
// ResumeStopped must put back each plugin's status and its health loop (#979).
func TestResumeStoppedRestoresStatusAndHealthLoop(t *testing.T) {
	rt := newTestRuntime(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	beat := filepath.Join(t.TempDir(), "beats")
	health := plugin.HealthBlock{
		Interval: "20ms",
		Steps:    []plugin.Step{{Type: "run", Run: "echo ok >> " + beat}},
	}

	saveState(t, rt, "alpha", plugin.StatusRunning) // with a health loop
	saveState(t, rt, "beta", plugin.StatusDegraded) // no health loop
	saveState(t, rt, "gamma", plugin.StatusStopped) // stopped before StopAll: stays stopped
	saveState(t, rt, "delta", plugin.StatusFailed)  // not running: untouched
	saveState(t, rt, "eps", plugin.StatusRunning)   // removed before the resume
	rt.startHealthLoop(ctx, "alpha", health)
	if !waitForTicks(t, beat, 1) {
		t.Fatal("health loop never ran before StopAll")
	}

	rt.StopAll(ctx)
	for _, n := range []string{"alpha", "beta", "eps"} {
		if s := status(t, rt, n); s != plugin.StatusStopped {
			t.Fatalf("%s after StopAll = %q, want stopped", n, s)
		}
	}
	time.Sleep(60 * time.Millisecond) // let a tick in flight finish
	stoppedAt := healthTicks(t, beat)
	time.Sleep(100 * time.Millisecond)
	if n := healthTicks(t, beat); n != stoppedAt {
		t.Fatalf("health loop still running after StopAll (%d -> %d ticks)", stoppedAt, n)
	}

	if err := rt.Remove(ctx, "eps"); err != nil {
		t.Fatal(err)
	}
	rt.ResumeStopped(ctx)

	for name, want := range map[string]plugin.PluginStatus{
		"alpha": plugin.StatusRunning,
		"beta":  plugin.StatusDegraded,
		"gamma": plugin.StatusStopped,
		"delta": plugin.StatusFailed,
	} {
		if s := status(t, rt, name); s != want {
			t.Errorf("%s after ResumeStopped = %q, want %q", name, s, want)
		}
	}
	if _, err := rt.store.Load("eps"); err == nil {
		t.Error("a plugin removed between StopAll and ResumeStopped was recreated")
	}
	if !waitForTicks(t, beat, stoppedAt+2) {
		t.Errorf("alpha's health loop did not restart: %d ticks, want at least %d", healthTicks(t, beat), stoppedAt+2)
	}
}

// What ResumeStopped restores is what the LAST StopAll stopped, once: a
// second call, or one after a plugin was reinstalled, changes nothing.
func TestResumeStoppedIsOnceAndYieldsToNewerState(t *testing.T) {
	rt := newTestRuntime(t)
	ctx := context.Background()
	saveState(t, rt, "alpha", plugin.StatusRunning)
	saveState(t, rt, "beta", plugin.StatusRunning)

	rt.StopAll(ctx)
	saveState(t, rt, "beta", plugin.StatusInstalling) // reinstall began in between
	rt.ResumeStopped(ctx)
	if s := status(t, rt, "alpha"); s != plugin.StatusRunning {
		t.Errorf("alpha = %q, want running", s)
	}
	if s := status(t, rt, "beta"); s != plugin.StatusInstalling {
		t.Errorf("beta = %q, want the newer installing state left alone", s)
	}

	// Stopped again for real, e.g. by spored's own shutdown: a stale resume
	// must not bring it back.
	saveState(t, rt, "alpha", plugin.StatusStopped)
	rt.ResumeStopped(ctx)
	if s := status(t, rt, "alpha"); s != plugin.StatusStopped {
		t.Errorf("second ResumeStopped changed alpha to %q, want stopped", s)
	}
}
