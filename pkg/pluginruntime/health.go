package pluginruntime

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/spore-host/spawn/pkg/plugin"
)

// healthFailThreshold is the number of consecutive failures before a plugin
// is marked as StatusDegraded.
const healthFailThreshold = 3

// healthLoop runs the health check loop for a single plugin until ctx is done.
func (rt *Runtime) healthLoop(ctx context.Context, name string, health plugin.HealthBlock) {
	interval := time.Minute
	if health.Interval != "" {
		if d, err := time.ParseDuration(health.Interval); err == nil {
			interval = d
		} else {
			log.Printf("Plugin %s: invalid health interval %q, using 1m", name, health.Interval)
		}
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	failures := 0

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := rt.runHealthCheck(ctx, name, health.Steps); err != nil {
				failures++
				log.Printf("Plugin %s: health check failed (%d/%d): %v",
					name, failures, healthFailThreshold, err)
				if failures >= healthFailThreshold {
					rt.setPluginStatus(ctx, name, plugin.StatusDegraded, err.Error())
				}
			} else {
				if failures > 0 {
					log.Printf("Plugin %s: health check recovered after %d failure(s)", name, failures)
				}
				failures = 0
				rt.recordHealthOK(ctx, name)
			}
		}
	}
}

// runHealthCheck executes the health steps for the named plugin.
func (rt *Runtime) runHealthCheck(ctx context.Context, name string, steps []plugin.Step) error {
	st, err := rt.store.Load(name)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	tmplCtx := rt.buildTemplateContext(st)
	return rt.executor.RunSteps(ctx, steps, tmplCtx)
}

// recordHealthOK stamps the last_health time and clears degraded status.
func (rt *Runtime) recordHealthOK(ctx context.Context, name string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if loopStopped(ctx) {
		return
	}
	st, err := rt.store.Load(name)
	if err != nil {
		return
	}
	now := time.Now()
	st.LastHealth = &now
	if st.Status == plugin.StatusDegraded {
		st.Status = plugin.StatusRunning
		st.Error = ""
	}
	_ = rt.store.Save(st)
}

// setPluginStatus updates the status and optional error message in persisted state.
func (rt *Runtime) setPluginStatus(ctx context.Context, name string, status plugin.PluginStatus, errMsg string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if loopStopped(ctx) {
		return
	}
	st, err := rt.store.Load(name)
	if err != nil {
		return
	}
	st.Status = status
	st.Error = errMsg
	_ = rt.store.Save(st)
}

// loopStopped reports whether the health loop that ctx belongs to has been
// stopped. Call it holding rt.mu: stopHealthLoop cancels under that lock, so a
// tick that gets the lock after it sees the cancel and writes nothing.
//
// Without it, a tick blocked on rt.mu while StopAll stopped the loop got the
// lock next, loaded "running" before StopAll saved "stopped" (that save is not
// under rt.mu), and wrote "running" back over it. The plugin then read as
// running after the agent shutdown, and ResumeStopped, which only resumes what
// is recorded as stopped, never restored its health loop (#979).
func loopStopped(ctx context.Context) bool { return ctx.Err() != nil }
