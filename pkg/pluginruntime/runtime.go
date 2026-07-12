package pluginruntime

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sync"
	"time"

	"github.com/spore-host/spawn/pkg/plugin"
	"github.com/spore-host/spawn/pkg/provider"
)

// pushedKeyRe matches valid pushed key names (same constraints as POSIX env vars, max 64 chars).
var pushedKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

// Runtime manages plugins on an instance (the spored side).
// It is safe for concurrent use.
type Runtime struct {
	store    plugin.StateStore
	executor *RemoteExecutor
	identity *provider.Identity // may be nil in tests

	mu            sync.Mutex
	healthCancels map[string]context.CancelFunc
}

// NewRuntime creates a Runtime backed by the default disk state store.
// identity may be nil (e.g. in tests); instance.* template variables will be empty.
func NewRuntime(identity *provider.Identity) *Runtime {
	return &Runtime{
		store:         plugin.DefaultStateStore(),
		executor:      NewRemoteExecutor(),
		identity:      identity,
		healthCancels: make(map[string]context.CancelFunc),
	}
}

// Install runs install, configure, and start steps for a plugin, then starts
// the health goroutine. It is equivalent to InstallWithPushed with no
// pre-seeded pushed values.
func (rt *Runtime) Install(ctx context.Context, spec *plugin.PluginSpec, cfg map[string]string) error {
	return rt.InstallWithPushed(ctx, spec, cfg, nil)
}

// InstallWithPushed runs install, configure, and start steps for a plugin with
// pushed values seeded up front, then starts the health goroutine.
//
// Seeding pushed values before the configure phase is what lets the unified
// `spawn plugin install` flow work: the controller runs the plugin's local
// provision steps first (which may capture and push values, e.g. a Globus setup
// key), collects those pushes, and hands them here so {{ pushed.<key> }}
// references resolve during configure without the plugin ever parking at
// StatusWaitingForPush.
//
// NOTE: the launch-time / async path (LoadFromDeclarations → Install with nil
// pushed) has no controller to run local provision, so a plugin whose configure
// step needs a pushed value still parks at StatusWaitingForPush and is not
// automatically resumed when ReceivePush later delivers the value — see
// ReceivePush. That path is unused by the current registry plugins and is left
// as a known limitation.
func (rt *Runtime) InstallWithPushed(ctx context.Context, spec *plugin.PluginSpec, cfg map[string]string, pushed map[string]string) error {
	resolvedCfg, err := spec.ResolvedConfig(cfg)
	if err != nil {
		return fmt.Errorf("resolve config: %w", err)
	}

	seededPushed := make(map[string]string, len(pushed))
	for k, v := range pushed {
		seededPushed[k] = v
	}

	state := &plugin.PluginState{
		Name:        spec.Name,
		Version:     spec.Version,
		Status:      plugin.StatusInstalling,
		Config:      resolvedCfg,
		Outputs:     make(map[string]string),
		Pushed:      seededPushed,
		InstalledAt: time.Now(),
	}
	if err := rt.store.Save(state); err != nil {
		return fmt.Errorf("save initial state: %w", err)
	}

	tmplCtx := rt.buildTemplateContext(state)

	// Install phase.
	log.Printf("Plugin %s: install", spec.Name)
	if err := rt.executor.RunSteps(ctx, spec.Remote.Install, tmplCtx); err != nil {
		return rt.failPlugin(state, fmt.Errorf("install: %w", err))
	}

	// Configure phase.
	state.Status = plugin.StatusConfiguring
	_ = rt.store.Save(state)
	log.Printf("Plugin %s: configure", spec.Name)
	if err := rt.executor.RunSteps(ctx, spec.Remote.Configure, tmplCtx); err != nil {
		if errors.Is(err, plugin.ErrMissingKey) {
			state.Status = plugin.StatusWaitingForPush
			_ = rt.store.Save(state)
			log.Printf("Plugin %s: waiting for push (missing pushed key)", spec.Name)
			return nil
		}
		return rt.failPlugin(state, fmt.Errorf("configure: %w", err))
	}

	// Start phase.
	state.Status = plugin.StatusStarting
	_ = rt.store.Save(state)
	log.Printf("Plugin %s: start", spec.Name)
	if err := rt.executor.RunSteps(ctx, spec.Remote.Start, tmplCtx); err != nil {
		return rt.failPlugin(state, fmt.Errorf("start: %w", err))
	}

	state.Status = plugin.StatusRunning
	state.Error = ""
	if err := rt.store.Save(state); err != nil {
		return fmt.Errorf("save state: %w", err)
	}

	if len(spec.Remote.Health.Steps) > 0 {
		rt.startHealthLoop(ctx, spec.Name, spec.Remote.Health)
	}

	log.Printf("Plugin %s: running", spec.Name)
	return nil
}

// Remove stops a plugin and deletes its persisted state.
func (rt *Runtime) Remove(ctx context.Context, name string) error {
	if _, err := rt.store.Load(name); err != nil {
		return err
	}

	rt.stopHealthLoop(name)

	if err := rt.store.Delete(name); err != nil {
		return fmt.Errorf("delete state: %w", err)
	}

	log.Printf("Plugin %s: removed", name)
	return nil
}

// StopAll stops every running plugin. Called during agent Cleanup.
func (rt *Runtime) StopAll(ctx context.Context) {
	states, err := rt.store.List()
	if err != nil {
		log.Printf("Plugin runtime: list plugins for StopAll: %v", err)
		return
	}

	for _, st := range states {
		if st.Status != plugin.StatusRunning && st.Status != plugin.StatusDegraded {
			continue
		}
		rt.stopHealthLoop(st.Name)
		log.Printf("Plugin %s: stopping (agent shutdown)", st.Name)
		st.Status = plugin.StatusStopped
		_ = rt.store.Save(st)
	}
}

// Status returns the current persisted state of a named plugin.
func (rt *Runtime) Status(name string) (*plugin.PluginState, error) {
	return rt.store.Load(name)
}

// ListAll returns persisted state for all known plugins.
func (rt *Runtime) ListAll() ([]*plugin.PluginState, error) {
	return rt.store.List()
}

// ReceivePush stores a key/value pushed from the local controller into the
// plugin's state.  If the plugin was waiting for this push it transitions to
// StatusRunning.
//
// NOTE: this only records the value and flips the status; it does NOT re-run the
// configure/start steps that were skipped when the plugin parked at
// StatusWaitingForPush (the persisted state does not retain the spec needed to
// replay them). The unified `spawn plugin install` flow avoids this by seeding
// pushed values before configure via InstallWithPushed, so this path is only
// reachable from the async launch-time flow.
func (rt *Runtime) ReceivePush(ctx context.Context, pluginName, key, value string) error {
	st, err := rt.store.Load(pluginName)
	if err != nil {
		return fmt.Errorf("plugin %s: %w", pluginName, err)
	}

	if !pushedKeyRe.MatchString(key) {
		return fmt.Errorf("plugin %s: invalid pushed key %q", pluginName, key)
	}

	if st.Pushed == nil {
		st.Pushed = make(map[string]string)
	}
	st.Pushed[key] = value

	if st.Status == plugin.StatusWaitingForPush {
		st.Status = plugin.StatusRunning
	}

	return rt.store.Save(st)
}

// LoadFromDeclarations installs each plugin in the declaration list.
// Already-running plugins are skipped.  Each install runs in its own goroutine.
func (rt *Runtime) LoadFromDeclarations(ctx context.Context, declarations []plugin.Declaration, resolver plugin.RegistryResolver) {
	for _, decl := range declarations {
		go func(d plugin.Declaration) {
			spec, err := resolver.Resolve(ctx, d.Ref)
			if err != nil {
				log.Printf("Plugin %s: resolve spec: %v", d.Ref, err)
				return
			}

			existing, err := rt.store.Load(spec.Name)
			if err == nil && (existing.Status == plugin.StatusRunning || existing.Status == plugin.StatusDegraded) {
				log.Printf("Plugin %s: already running, skipping install", spec.Name)
				if len(spec.Remote.Health.Steps) > 0 {
					rt.startHealthLoop(ctx, spec.Name, spec.Remote.Health)
				}
				return
			}

			if err := rt.Install(ctx, spec, d.Config); err != nil {
				log.Printf("Plugin %s: install failed: %v", spec.Name, err)
			}
		}(decl)
	}
}

// buildTemplateContext constructs a TemplateContext from persisted plugin state.
func (rt *Runtime) buildTemplateContext(st *plugin.PluginState) plugin.TemplateContext {
	tmplCtx := plugin.NewTemplateContext()
	for k, v := range st.Config {
		tmplCtx.Config[k] = v
	}
	for k, v := range st.Outputs {
		tmplCtx.Outputs[k] = v
	}
	for k, v := range st.Pushed {
		tmplCtx.Pushed[k] = v
	}
	if rt.identity != nil {
		tmplCtx.Instance["id"] = rt.identity.InstanceID
		tmplCtx.Instance["name"] = rt.identity.Name
		tmplCtx.Instance["ip"] = rt.identity.PublicIP
	}
	return tmplCtx
}

func (rt *Runtime) failPlugin(state *plugin.PluginState, err error) error {
	state.Status = plugin.StatusFailed
	state.Error = err.Error()
	_ = rt.store.Save(state)
	return err
}

func (rt *Runtime) startHealthLoop(ctx context.Context, name string, health plugin.HealthBlock) {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	// Cancel any existing loop for this plugin.
	if cancel, ok := rt.healthCancels[name]; ok {
		cancel()
	}

	healthCtx, cancel := context.WithCancel(ctx)
	rt.healthCancels[name] = cancel
	go rt.healthLoop(healthCtx, name, health)
}

func (rt *Runtime) stopHealthLoop(name string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	if cancel, ok := rt.healthCancels[name]; ok {
		cancel()
		delete(rt.healthCancels, name)
	}
}
