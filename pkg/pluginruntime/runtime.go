package pluginruntime

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"runtime"
	"sync"
	"time"

	"github.com/spore-host/spawn/pkg/plugin"
	"github.com/spore-host/spawn/pkg/provider"
)

// pushedKeyRe matches valid pushed key names (same constraints as POSIX env vars, max 64 chars).
var pushedKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

// hostGOOS is the OS spored is running on. A variable so tests can exercise
// the Windows refusal on any OS (#1001).
var hostGOOS = runtime.GOOS

// Runtime manages plugins on an instance (the spored side).
// It is safe for concurrent use.
type Runtime struct {
	store     plugin.StateStore
	executor  *RemoteExecutor
	identity  *provider.Identity // may be nil in tests
	localUser string             // instance login user (spawn:local-username); exposed as {{ instance.login_user }}

	mu            sync.Mutex
	healthCancels map[string]context.CancelFunc
}

// NewRuntime creates a Runtime backed by the default disk state store.
// identity may be nil (e.g. in tests); instance.* template variables will be
// empty. localUser is the instance's local login user (spawn:local-username);
// steps that opt into AsUser run as this user, and "" means always-root.
func NewRuntime(identity *provider.Identity, localUser string) *Runtime {
	return &Runtime{
		store:         plugin.DefaultStateStore(),
		executor:      NewRemoteExecutor(localUser),
		identity:      identity,
		localUser:     localUser,
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
	return rt.InstallWithProvenance(ctx, spec, cfg, pushed, nil)
}

// InstallWithProvenance is InstallWithPushed plus a resolved Provenance record
// (origin + verification tier) that is persisted into the plugin's on-instance
// state, so an audit can read what was installed and how it was verified. prov
// may be nil (local-file ref, or an older controller that didn't supply it).
func (rt *Runtime) InstallWithProvenance(ctx context.Context, spec *plugin.PluginSpec, cfg, pushed map[string]string, prov *plugin.Provenance) error {
	// Plugins are Linux-only (#1001): refuse before recording any state or
	// running a step, rather than failing at the first "sh -c" on Windows.
	if err := plugin.CheckSporeOS(hostGOOS); err != nil {
		return err
	}
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
		Provenance:  prov,
		StopSteps:   spec.Remote.Stop,
		InstalledAt: time.Now(),
	}
	if err := rt.store.Save(state); err != nil {
		return fmt.Errorf("save initial state: %w", err)
	}

	tmplCtx := rt.buildTemplateContext(state)

	// Pre-flight: conditions.remote, before any install step runs (#1040). The
	// spec documents these as "pre-flight checks before install", but nothing
	// evaluated them, so a plugin's platform or command check was silently
	// ignored. A failure marks the plugin failed with the author's message,
	// which `spawn plugin install` and `spawn plugin status` report.
	if len(spec.Conditions.Remote) > 0 {
		log.Printf("Plugin %s: checking %d remote condition(s)", spec.Name, len(spec.Conditions.Remote))
		if err := rt.checkRemoteConditions(ctx, spec.Conditions.Remote, tmplCtx, false); err != nil {
			return rt.failPlugin(state, fmt.Errorf("remote condition: %w", err))
		}
	}

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

// Preflight evaluates a spec's conditions.remote without installing anything or
// recording state. `spawn plugin install` calls it (POST /v1/plugins/preflight)
// before running the plugin's local provision steps, so a spore that fails a
// remote condition is reported before the controller has, say, minted a
// credential or created an endpoint for an install that cannot succeed.
//
// Pushed values do not exist yet at that point, so a condition whose command
// references {{ pushed.* }} is deferred to the check InstallWithProvenance
// makes, rather than failed here.
func (rt *Runtime) Preflight(ctx context.Context, spec *plugin.PluginSpec, cfg map[string]string) error {
	resolvedCfg, err := spec.ResolvedConfig(cfg)
	if err != nil {
		return fmt.Errorf("resolve config: %w", err)
	}
	tmplCtx := rt.buildTemplateContext(&plugin.PluginState{Config: resolvedCfg})
	return rt.checkRemoteConditions(ctx, spec.Conditions.Remote, tmplCtx, true)
}

// conditionTimeout bounds the evaluation of a spec's conditions.remote, so a
// hung pre-flight command cannot park a plugin at "installing" forever. It is
// kept under the push API's WriteTimeout because the preflight endpoint
// evaluates conditions synchronously.
const conditionTimeout = 10 * time.Second

// checkRemoteConditions evaluates conds on this spore. With deferPushed, a
// condition that cannot be rendered yet for want of a pushed value is skipped.
func (rt *Runtime) checkRemoteConditions(ctx context.Context, conds []plugin.Condition, tmplCtx plugin.TemplateContext, deferPushed bool) error {
	ctx, cancel := context.WithTimeout(ctx, conditionTimeout)
	defer cancel()
	host := rt.conditionHost()
	for _, c := range conds {
		err := plugin.CheckCondition(ctx, c, tmplCtx, host)
		if err != nil && deferPushed && errors.Is(err, plugin.ErrMissingKey) {
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// conditionHost is this spore as a plugin.ConditionHost: platform conditions
// are compared against hostGOOS (the same value the Windows refusal reads),
// and command conditions run exactly as a root run step does.
func (rt *Runtime) conditionHost() plugin.ConditionHost {
	return plugin.ConditionHost{
		GOOS: hostGOOS,
		RunCommand: func(ctx context.Context, script string) error {
			err := rt.executor.runCommand(ctx, plugin.Step{Type: "run", Run: script})
			if inner := errors.Unwrap(err); inner != nil {
				// runCommand prefixes `run "<script>"`; CheckCondition already
				// names the command, so keep only the exit status.
				return inner
			}
			return err
		},
	}
}

// ErrStopStepsFailed marks a Remove whose remote.stop steps failed. The plugin
// is removed all the same; the error says what may still be running.
var ErrStopStepsFailed = errors.New("stop steps failed")

// Remove ends the plugin's health loop, runs its remote.stop steps, and
// deletes its persisted state. This is `spawn plugin remove` (DELETE
// /v1/plugins/{name}), the one time remote.stop runs (#1018).
//
// A failing stop step does not keep the plugin installed: the user asked for
// it gone, and a plugin that cannot be removed until its stop step works
// would leave them nothing to run. The removal completes and the returned
// error wraps ErrStopStepsFailed, so the caller can say what may still be
// running.
func (rt *Runtime) Remove(ctx context.Context, name string) error {
	st, err := rt.store.Load(name)
	if err != nil {
		return err
	}

	// The health check first, so it does not see the service going down and
	// mark the plugin degraded while it is being removed.
	rt.stopHealthLoop(name)

	var stopErr error
	if len(st.StopSteps) > 0 {
		log.Printf("Plugin %s: stop", name)
		if err := rt.executor.RunSteps(ctx, st.StopSteps, rt.buildTemplateContext(st)); err != nil {
			log.Printf("Plugin %s: stop steps failed: %v (removing it anyway)", name, err)
			stopErr = fmt.Errorf("plugin %s: %w: %v", name, ErrStopStepsFailed, err)
		}
	}

	if err := rt.store.Delete(name); err != nil {
		// Not removed after all, so not ErrStopStepsFailed (which means it was);
		// a stop failure is already in the log.
		return fmt.Errorf("delete state: %w", err)
	}

	log.Printf("Plugin %s: removed", name)
	return stopErr
}

// StopAll marks every running plugin stopped and ends its health loop. Called
// during agent Cleanup, before a stop, hibernate or terminate.
//
// It does NOT run the plugins' remote.stop steps, and that is deliberate
// (#1018). Nothing re-runs remote.start when the instance comes back, so a
// stop step run here would outlast the stop: `tailscale down` keeps the node
// off the tailnet after a `spawn start` (the Tailscale plugin relies on the
// node reconnecting on boot), and `globusconnectpersonal -stop` leaves the
// endpoint down. A stop or terminate shuts the OS down, which stops the
// services anyway; a hibernate freezes them and resumes them as they were.
// remote.stop is the undo of an install, run by Remove.
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
	if len(declarations) == 0 {
		return
	}
	if err := plugin.CheckSporeOS(hostGOOS); err != nil {
		log.Printf("Plugins: not installing %d declared plugin(s): %v", len(declarations), err)
		return
	}
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
	// The instance's login user (spawn:local-username) — the same user `as_user`
	// steps run as. Exposed so a plugin can name it in a file it writes (e.g. a
	// systemd unit's User= or a chown target) rather than hardcoding "ec2-user".
	// Falls back to ec2-user when unknown (older instances / not tagged).
	loginUser := rt.localUser
	if loginUser == "" {
		loginUser = "ec2-user"
	}
	tmplCtx.Instance["login_user"] = loginUser
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
