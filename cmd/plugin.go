package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spore-host/spawn/pkg/sshtarget"

	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/plugin"
	"github.com/spore-host/spawn/pkg/sshkey"
	"gopkg.in/yaml.v3"
)

// plugin-level flags shared across subcommands
var (
	pluginInstance    string
	pluginJSONOutput  bool // deprecated: use --output json
	pluginKeyPath     string
	pluginConfigPairs []string
	pluginRemoveYes   bool
	pluginSSHUser     string
	pluginDryRun      bool
	pluginInsecure    bool // --insecure: skip signature/manifest verification for official refs
)

// pluginResolver builds the registry resolver honoring the shared --insecure flag.
func pluginResolver() plugin.RegistryResolver {
	return plugin.DefaultResolver(plugin.WithInsecure(pluginInsecure))
}

var pluginCmd = &cobra.Command{
	Use:   "plugin",
	Short: "Manage plugins on spore instances",
	Long: `Install, inspect, and remove service plugins on running instances.

Plugins are composable service units (Jupyter, Globus, Tailscale, etc.)
defined by YAML specs with install/start/stop/health lifecycles.

Examples:
  spawn plugin list --instance i-0abc123
  spawn plugin install globus-personal-endpoint --instance i-0abc123 \
    --config endpoint_name=my-endpoint
  spawn plugin status globus-personal-endpoint --instance i-0abc123
  spawn plugin remove globus-personal-endpoint --instance i-0abc123`,
}

// ── list ──────────────────────────────────────────────────────────────────────

var pluginListCmd = &cobra.Command{
	Use:   "list",
	Short: "List plugins installed on an instance",
	RunE: func(cmd *cobra.Command, args []string) error {
		if pluginInstance == "" {
			return fmt.Errorf("--instance is required")
		}
		push, err := pluginTransportFor(cmd.Context(), pluginInstance)
		if err != nil {
			return err
		}
		states, err := remotePluginList(cmd.Context(), push)
		if err != nil {
			return err
		}

		if pluginJSONOutput || getOutputFormat() == "json" {
			return json.NewEncoder(os.Stdout).Encode(states)
		}

		if len(states) == 0 {
			fmt.Println("No plugins installed.")
			return nil
		}

		w := newTableWriter(os.Stdout)
		_, _ = fmt.Fprintln(w, "NAME\tVERSION\tSTATUS\tUPDATED")
		for _, st := range states {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
				st.Name, st.Version, st.Status,
				st.UpdatedAt.Format(time.RFC3339))
		}
		return w.Flush()
	},
}

// ── install ───────────────────────────────────────────────────────────────────

var pluginInstallCmd = &cobra.Command{
	Use:   "install <plugin-ref>",
	Short: "Install a plugin on an instance",
	Long: `Install a plugin on a running spore instance.

Runs the plugin's full lifecycle: local provision steps on this controller
(e.g. creating a mutagen sync or a Globus endpoint), then the remote
install/configure/start steps on the instance via spored. Values captured and
pushed by local steps are delivered before the remote configure phase runs.

spored's push API listens on the instance's loopback only. spawn reaches it over
SSH when the instance has a public IP and SSH works, and otherwise runs the
request on the instance over SSM (keyless or SSM-only instances, no public IP).
The same applies to 'spawn plugin list', 'status' and 'remove'.

Linux spores only: plugin steps are POSIX shell, so 'spawn plugin install'
refuses a Windows spore (spawn:os=windows) before running anything.

Plugin ref formats:
  name                  official registry (spore-host/spore-plugins)
  name@v1.2.0           pinned to git tag
  github:user/repo/name custom GitHub repository
  ./path/to/plugin.yaml local file`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		// --dry-run previews the plan without contacting an instance, so it does
		// not require --instance (nothing is installed).
		if pluginDryRun {
			return runPluginInspect(cmd.Context(), args[0], true)
		}
		if pluginInstance == "" {
			return fmt.Errorf("--instance is required")
		}

		cfgMap, err := parseKeyValuePairs(pluginConfigPairs)
		if err != nil {
			return err
		}

		return runPluginInstall(cmd.Context(), args[0], pluginInstance, cfgMap)
	},
}

// ── inspect ─────────────────────────────────────────────────────────────────

var pluginInspectCmd = &cobra.Command{
	Use:   "inspect <plugin-ref>",
	Short: "Preview what a plugin does, without installing it",
	Long: `Resolve a plugin reference and render its plan — resolved source and
version, local (controller) vs remote (instance) steps, requested controller
environment, root vs login-user execution, downloads, health checks, cleanup,
and its declared permissions block — WITHOUT executing anything or contacting an
instance.

Installing a plugin runs its author's code on your machine and, on the instance,
as root. Inspect it first, especially for third-party (github:) plugins.

Plugin ref formats are the same as 'spawn plugin install':
  name                  official registry (spore-host/spore-plugins)
  name@v1.2.0           pinned to git tag
  github:user/repo/name custom GitHub repository
  ./path/to/plugin.yaml local file`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runPluginInspect(cmd.Context(), args[0], false)
	},
}

// pluginInstanceLookup resolves --instance for the Windows guard in
// runPluginInstall. A variable so a test can hand it a Windows instance
// without AWS.
var pluginInstanceLookup = resolveInstanceViaSpawn

// refusePluginsOnWindowsSpore fails a plugin install aimed at a Windows spore
// (spawn:os=windows), before anything runs locally or remotely (#1001). An
// instance spawn cannot resolve (nil: a raw host, or no AWS access) is let
// through; spored on a Windows spore refuses the install itself.
func refusePluginsOnWindowsSpore(inst *aws.InstanceInfo, instance string) error {
	if inst == nil {
		return nil
	}
	if err := plugin.CheckSporeOS(inst.Tags["spawn:os"]); err != nil {
		return fmt.Errorf("cannot install a plugin on %s (spawn:os=windows): %w", instance, err)
	}
	return nil
}

func runPluginInstall(ctx context.Context, ref, instance string, cfg map[string]string) error {
	// Before resolving the plugin, so nothing is fetched and no local provision
	// step runs for an install that cannot succeed.
	if err := refusePluginsOnWindowsSpore(pluginInstanceLookup(ctx, instance), instance); err != nil {
		return err
	}

	resolver := pluginResolver()
	spec, prov, err := resolver.ResolveWithProvenance(ctx, ref)
	if err != nil {
		return fmt.Errorf("resolve plugin %q: %w", ref, err)
	}

	fmt.Printf("Installing %s %s on %s...\n", spec.Name, spec.Version, instance)
	if prov != nil && prov.CommitSHA != "" {
		fmt.Printf("  pinned to commit %s (sha256 %s)\n", prov.CommitSHA, shortHash(prov.ContentSHA256))
	}
	if prov != nil && prov.SignatureVerified {
		fmt.Printf("  signature verified (official release)\n")
	} else if prov != nil && prov.ManifestVerified {
		fmt.Printf("  checksum-manifest verified\n")
	}

	// spored validates the spec when it receives it and refuses one that fails.
	// Validate here too, before any local step runs, so a spec spored would
	// refuse does not first leave a controller-side footprint (#1130).
	if err := spec.Validate(""); err != nil {
		return fmt.Errorf("plugin %s failed validation: %w", spec.Name, err)
	}

	resolvedCfg, err := spec.ResolvedConfig(cfg)
	if err != nil {
		return err
	}

	// Check local pre-flight conditions before touching the remote. Their
	// commands are rendered against the resolved config, like steps.
	condCtx := plugin.NewTemplateContext()
	for k, v := range resolvedCfg {
		condCtx.Config[k] = v
	}
	if err := plugin.NewLocalExecutor(nil).CheckLocalConditions(ctx, spec.Conditions.Local, condCtx); err != nil {
		return fmt.Errorf("local condition: %w", err)
	}

	// (instance.ip is resolved just below; SSH identity for local steps is set up
	// there once we have the IP.)

	// Build template context with instance info and resolved config.
	tmplCtx := plugin.NewTemplateContext()
	instName, instIP := lookupInstanceInfo(ctx, instance)
	tmplCtx.Instance["id"] = instance
	tmplCtx.Instance["name"] = instName
	if instIP != "" {
		tmplCtx.Instance["ip"] = instIP
	}
	for k, v := range resolvedCfg {
		tmplCtx.Config[k] = v
	}

	// Contact the spore up front when there is a remote half or a remote
	// pre-flight condition: the remote conditions are checked there, by spored,
	// before the local provision steps below run (#1040), so a spore that
	// cannot take the plugin is reported before the controller mints a
	// credential or creates an endpoint for it.
	hasRemote := len(spec.Remote.Install) > 0 || len(spec.Remote.Configure) > 0 ||
		len(spec.Remote.Start) > 0
	var push pushTransport
	if hasRemote || len(spec.Conditions.Remote) > 0 {
		push, err = pluginTransportFor(ctx, instance)
		if err != nil {
			return err
		}
		// Gate on the instance being fully provisioned before touching the remote
		// half: spored must be up to receive the request, and cloud-init must
		// have finished (local user created, keys installed, network/DNS ready)
		// or the remote steps race the boot. This is the same deterministic
		// readiness signal `spawn launch` uses. Best-effort: a resolvable instance
		// with SSM lets us wait; otherwise we proceed (e.g. a bare hostname).
		if inst := resolveInstanceViaSpawn(ctx, instance); inst != nil && inst.InstanceID != "" {
			fmt.Println("Waiting for the instance to be ready...")
			if rerr := ensureInstanceReady(ctx, inst.Region, inst.InstanceID, 5*time.Minute); rerr != nil {
				fmt.Fprintf(os.Stderr, "warning: could not confirm instance readiness (%v); proceeding\n", rerr)
			}
		}
	}
	if len(spec.Conditions.Remote) > 0 {
		fmt.Printf("Checking %d remote condition(s) on the instance...\n", len(spec.Conditions.Remote))
		if err := remotePluginPreflight(ctx, push, spec, resolvedCfg); err != nil {
			if !errors.Is(err, errPreflightUnsupported) {
				return err
			}
			// An older spored has no preflight endpoint. If there is a remote half,
			// a spored new enough to evaluate conditions does so before its install
			// phase and the failure is reported below; an older one does not
			// evaluate them at all.
			fmt.Fprintf(os.Stderr, "warning: this instance's spored cannot check remote conditions before install; "+
				"they are checked (by a spored that supports them) only when the remote install runs\n")
		}
	}

	// If the plugin has local steps that SSH to the instance (e.g. spore-sync's
	// mutagen), configure the instance's launch key as the IdentityFile for its
	// IP so the system ssh those steps invoke can authenticate.
	if len(spec.Local.Provision) > 0 && instIP != "" {
		ensureHostIdentity(ctx, instance, instIP)
	}

	// Run local provision steps on the controller. Push steps are buffered
	// rather than delivered immediately, so their values can be handed to spored
	// with the install request below — that guarantees they are present before
	// the remote configure phase runs.
	pushBuf := plugin.NewBufferingPushClient()
	var localOutputs map[string]string
	if len(spec.Local.Provision) > 0 {
		fmt.Println("Running local provision steps...")
		execWithPush := plugin.NewLocalExecutor(pushBuf).WithEnvPassthrough(spec.Local.EnvPassthrough)
		localOutputs, err = execWithPush.RunProvision(ctx, spec.Name, spec.Local.Provision, tmplCtx)
		if err != nil {
			return fmt.Errorf("local provision: %w", err)
		}
	}

	// Record the controller-side footprint so `spawn plugin remove` / `spawn
	// terminate` can later replay the plugin's local deprovision steps (e.g. tear
	// down the mutagen sync or delete the Globus endpoint). We persist the
	// captured outputs because a deprovision step may reference them
	// (e.g. {{ outputs.endpoint_id }}) and they live only here on the controller.
	// A plugin that declares outputs gets a record too, so `spawn plugin
	// status` can show them (#1117).
	if pluginNeedsLocalRecord(spec) {
		saveLocalPluginRecord(instance, ref, spec, prov, tmplCtx.Instance, resolvedCfg, localOutputs)
	}

	// Run the remote half (install → configure → start) on the instance via
	// spored, handing over the resolved config and any buffered pushed values.
	if hasRemote {
		fmt.Println("Running remote install steps on the instance...")
		if err := remotePluginInstall(ctx, push, spec, resolvedCfg, pushBuf.Values(), prov); err != nil {
			return fmt.Errorf("remote install: %w", err)
		}
		// Record provenance on the instance's EC2 tags so an audit can answer
		// "which plugin bytes are on this box" from the AWS control plane, surviving
		// loss of the controller-side local record. Done once the install request
		// is accepted (the bytes are on the box and their provenance is known),
		// independent of whether the plugin's service later reaches Running —
		// matching spored's on-instance state, which also records on failure.
		// Best-effort: a tagging failure never fails the install.
		recordPluginProvenanceTag(ctx, instance, spec, prov)

		if err := waitForPluginReady(ctx, push, instance, spec.Name); err != nil {
			return err
		}
	}

	fmt.Printf("Plugin %s installed on %s.\n", spec.Name, instance)
	renderPluginOutputs(os.Stdout, plugin.DeclaredOutputValues(spec.Outputs, localOutputs))
	fmt.Printf("Use 'spawn plugin status %s --instance %s' to check status.\n", spec.Name, instance)
	return nil
}

// renderPluginOutputs prints a plugin's declared outputs and their values.
func renderPluginOutputs(out io.Writer, outputs []plugin.OutputValue) {
	if len(outputs) == 0 {
		return
	}
	fmt.Fprintf(out, "Outputs:\n")
	for _, o := range outputs {
		v := o.Value
		if !o.Recorded {
			v = "(not recorded: no local provision step captures it)"
		}
		fmt.Fprintf(out, "  %s: %s\n", o.Name, v)
		if o.Description != "" {
			fmt.Fprintf(out, "      %s\n", o.Description)
		}
	}
}

// recordedPluginOutputs returns the declared outputs recorded on this
// controller for plugin name on instance, or nil if there is no record (the
// plugin was installed from another machine, or declares none).
func recordedPluginOutputs(instance, name string) []plugin.OutputValue {
	store, err := plugin.DefaultLocalStore()
	if err != nil {
		return nil
	}
	rec, err := store.Load(instance, name)
	if err != nil {
		return nil
	}
	return plugin.DeclaredOutputValues(rec.DeclaredOutputs, rec.Outputs)
}

// mergePluginOutputs adds the recorded declared outputs to st.Outputs, for
// --output json. spored's own outputs map, if it has values, is kept.
func mergePluginOutputs(st *pluginStateResponse, outputs []plugin.OutputValue) {
	for _, o := range outputs {
		if !o.Recorded {
			continue
		}
		if st.Outputs == nil {
			st.Outputs = map[string]string{}
		}
		if _, ok := st.Outputs[o.Name]; !ok {
			st.Outputs[o.Name] = o.Value
		}
	}
}

// writePluginStatus writes `spawn plugin status`, as JSON or for a human.
func writePluginStatus(out io.Writer, st *pluginStateResponse, outputs []plugin.OutputValue, asJSON bool) error {
	if asJSON {
		mergePluginOutputs(st, outputs)
		return json.NewEncoder(out).Encode(st)
	}
	printPluginStatus(out, st, outputs)
	return nil
}

// printPluginStatus renders `spawn plugin status` for a human.
func printPluginStatus(out io.Writer, st *pluginStateResponse, outputs []plugin.OutputValue) {
	fmt.Fprintf(out, "Plugin:  %s\n", st.Name)
	fmt.Fprintf(out, "Version: %s\n", st.Version)
	fmt.Fprintf(out, "Status:  %s\n", st.Status)
	fmt.Fprintf(out, "Updated: %s\n", st.UpdatedAt.Format(time.RFC3339))
	if st.Error != "" {
		fmt.Fprintf(out, "Error:   %s\n", st.Error)
	}
	if st.LastHealth != nil {
		fmt.Fprintf(out, "Health:  last OK %s\n", st.LastHealth.Format(time.RFC3339))
	}
	if p := st.Provenance; p != nil {
		fmt.Fprintf(out, "Source:  %s\n", describeProvenance(p))
	}
	renderPluginOutputs(out, outputs)
}

// pluginProvenanceTagValue builds the compact EC2-tag value recording a plugin's
// resolved provenance. It stays well under EC2's 256-char tag-value limit: a
// short content-digest prefix, the commit prefix (if pinned), the requested ref,
// and the verification tier reached.
func pluginProvenanceTagValue(spec *plugin.PluginSpec, prov *plugin.Provenance) string {
	parts := []string{"version=" + spec.Version}
	if prov != nil {
		if prov.ContentSHA256 != "" {
			parts = append(parts, "sha256="+shortHash(prov.ContentSHA256))
		}
		if prov.CommitSHA != "" {
			parts = append(parts, "commit="+shortHash(prov.CommitSHA))
		}
		switch {
		case prov.SignatureVerified:
			parts = append(parts, "verify=signature")
		case prov.ManifestVerified:
			parts = append(parts, "verify=manifest")
		default:
			parts = append(parts, "verify=none")
		}
	}
	return strings.Join(parts, ";")
}

// recordPluginProvenanceTag writes a spore:plugin:<name> EC2 tag recording the
// installed plugin's provenance. Best-effort: it resolves the instance to an EC2
// ID + region and skips silently (with a warning) if that's not possible (e.g. a
// bare hostname not backed by a resolvable spore instance).
func recordPluginProvenanceTag(ctx context.Context, instance string, spec *plugin.PluginSpec, prov *plugin.Provenance) {
	inst := resolveInstanceViaSpawn(ctx, instance)
	if inst == nil || inst.InstanceID == "" {
		return // not an EC2-resolvable instance; the local record still holds provenance
	}
	client, err := aws.NewClientWithRegion(ctx, inst.Region)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not record plugin provenance tag: %v\n", err)
		return
	}
	tags := map[string]string{
		"spore:plugin:" + spec.Name: pluginProvenanceTagValue(spec, prov),
	}
	if err := client.UpdateInstanceTags(ctx, inst.Region, inst.InstanceID, tags); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write plugin provenance tag for %s: %v\n", spec.Name, err)
	}
}

// saveLocalPluginRecord persists a controller-side record of an installed
// plugin so its local deprovision steps can be replayed later. Best-effort: a
// failure here is logged but does not fail the install (the plugin is already
// installed; the cost is a potential orphaned local footprint on removal).
func saveLocalPluginRecord(instance, ref string, spec *plugin.PluginSpec, prov *plugin.Provenance, instanceVars, cfg, outputs map[string]string) {
	store, err := plugin.DefaultLocalStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not open local plugin store: %v\n", err)
		return
	}
	rec := &plugin.LocalRecord{
		Name:            spec.Name,
		Ref:             ref,
		InstanceID:      instance,
		Instance:        instanceVars,
		Config:          cfg,
		Outputs:         recordOutputs(spec, outputs),
		DeclaredOutputs: spec.Outputs,
		Deprovision:     spec.Local.Deprovision,
		Reconcile:       spec.Local.Reconcile,
		EnvPassthrough:  spec.Local.EnvPassthrough,
		InstalledAt:     time.Now(),
	}
	if prov != nil {
		rec.ResolvedRef = prov.RequestedRef
		rec.CommitSHA = prov.CommitSHA
		rec.SpecSHA256 = prov.ContentSHA256
	}
	if err := store.Save(rec); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not record local plugin state for %s: %v\n", spec.Name, err)
	}
}

// pluginNeedsLocalRecord reports whether `spawn plugin install` must leave a
// controller-side record: for teardown steps to replay, or declared outputs
// for `spawn plugin status` to show.
func pluginNeedsLocalRecord(spec *plugin.PluginSpec) bool {
	return hasLocalTeardown(spec) || len(spec.Outputs) > 0
}

func hasLocalTeardown(spec *plugin.PluginSpec) bool {
	return len(spec.Local.Deprovision) > 0 || len(spec.Local.Reconcile) > 0
}

// recordOutputs is what a local record keeps of a plugin's captures. A plugin
// with teardown steps keeps them all, since those steps may reference any of
// them. One recorded only for its declared outputs keeps only those, so a
// credential captured to push to the instance is not left on disk for nothing.
func recordOutputs(spec *plugin.PluginSpec, captured map[string]string) map[string]string {
	if hasLocalTeardown(spec) {
		return captured
	}
	kept := map[string]string{}
	for name := range spec.Outputs {
		if v, ok := captured[name]; ok {
			kept[name] = v
		}
	}
	return kept
}

// templateContextFromRecord rebuilds the template context a recorded plugin was
// provisioned with (instance.* + config + outputs), so local deprovision /
// reconcile steps replay against the same values — e.g. a deprovision step like
// `mutagen sync terminate spore-{{ instance.name }}` targets the exact session
// provision created.
func templateContextFromRecord(rec *plugin.LocalRecord) plugin.TemplateContext {
	tmplCtx := plugin.NewTemplateContext()
	for k, v := range rec.Instance {
		tmplCtx.Instance[k] = v
	}
	if tmplCtx.Instance["id"] == "" {
		tmplCtx.Instance["id"] = rec.InstanceID
	}
	for k, v := range rec.Config {
		tmplCtx.Config[k] = v
	}
	for k, v := range rec.Outputs {
		tmplCtx.Outputs[k] = v
	}
	return tmplCtx
}

// runLocalDeprovision replays a recorded plugin's local deprovision steps on the
// controller (e.g. terminate the mutagen sync, delete the Globus endpoint) and
// removes the record. Best-effort: errors are reported but not fatal, since the
// caller is usually tearing the instance down regardless.
func runLocalDeprovision(ctx context.Context, store *plugin.LocalStore, instanceKey string, rec *plugin.LocalRecord) {
	tmplCtx := templateContextFromRecord(rec)

	fmt.Printf("Running local deprovision for plugin %s...\n", rec.Name)
	exec := plugin.NewLocalExecutor(nil).WithEnvPassthrough(rec.EnvPassthrough)
	if err := exec.RunDeprovision(ctx, rec.Deprovision, tmplCtx); err != nil {
		fmt.Fprintf(os.Stderr, "warning: local deprovision for %s failed (local resources may be orphaned): %v\n", rec.Name, err)
	}
	if err := store.Delete(instanceKey, rec.Name); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not remove local plugin record for %s: %v\n", rec.Name, err)
	}
	// Drop the spawn-managed ssh_config identity block for this instance's IP;
	// the instance is being torn down.
	if ip := rec.Instance["ip"]; ip != "" {
		if home, err := os.UserHomeDir(); err == nil {
			_ = sshkey.RemoveHostIdentity(home, ip)
		}
	}
}

// reconcileAllLocalPlugins re-points every recorded plugin on an instance that
// declares local reconcile steps, using the instance's new public IP. Called by
// `spawn start` because a stop/start reassigns the public IP, which an IP-bound
// local footprint (e.g. a mutagen sync session pinned to the old IP) can't
// follow on its own. The stored record is updated with the new IP so a later
// deprovision still targets the right session. Best-effort.
func reconcileAllLocalPlugins(ctx context.Context, newIP string, instanceKeys ...string) {
	store, err := plugin.DefaultLocalStore()
	if err != nil || newIP == "" {
		return
	}

	// Determine up front whether any recorded plugin actually needs reconciling,
	// so we only pay the SSH-readiness wait when there's work to do.
	seen := map[string]bool{}
	hasWork := false
	for _, key := range instanceKeys {
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		recs, _ := store.List(key)
		for _, rec := range recs {
			if len(rec.Reconcile) > 0 {
				hasWork = true
			}
		}
	}
	if !hasWork {
		return
	}

	// Reconcile steps SSH to the instance (mutagen) at the NEW IP; point the
	// spawn-managed ssh_config identity at it so they can authenticate after the
	// stop/start.
	for _, key := range instanceKeys {
		if key != "" {
			ensureHostIdentity(ctx, key, newIP)
			break
		}
	}

	seen = map[string]bool{}
	for _, key := range instanceKeys {
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		recs, err := store.List(key)
		if err != nil {
			continue
		}
		for _, rec := range recs {
			if len(rec.Reconcile) == 0 {
				continue
			}
			if rec.Instance == nil {
				rec.Instance = map[string]string{}
			}
			// Drop the stale ssh_config block for the previous IP before the record
			// is re-pointed, so old-IP blocks don't accumulate across restarts.
			if oldIP := rec.Instance["ip"]; oldIP != "" && oldIP != newIP {
				if home, err := os.UserHomeDir(); err == nil {
					_ = sshkey.RemoveHostIdentity(home, oldIP)
				}
			}
			rec.Instance["ip"] = newIP // reconcile + future deprovision use the new IP

			tmplCtx := templateContextFromRecord(rec)
			fmt.Printf("Reconciling plugin %s to new instance IP %s...\n", rec.Name, newIP)

			// EC2 reports "running" before SSH is ready for full sessions, and the
			// reconcile steps connect over SSH (e.g. mutagen sync create) — retry
			// with backoff rather than a separate SSH probe, since the steps use
			// their own SSH credential path (mutagen's default identities), which a
			// raw `ssh` probe wouldn't replicate.
			exec := plugin.NewLocalExecutor(nil).WithEnvPassthrough(rec.EnvPassthrough)
			var rerr error
			for attempt := 0; attempt < 10; attempt++ {
				if rerr = exec.RunDeprovision(ctx, rec.Reconcile, tmplCtx); rerr == nil {
					break
				}
				select {
				case <-ctx.Done():
					rerr = ctx.Err()
				case <-time.After(6 * time.Second):
					continue
				}
				break
			}
			if rerr != nil {
				fmt.Fprintf(os.Stderr, "warning: reconcile for %s failed (sync may need a manual re-install): %v\n", rec.Name, rerr)
				continue
			}
			if err := store.Save(rec); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not update local plugin record for %s: %v\n", rec.Name, err)
			}
		}
	}
}

// deprovisionAllLocalPlugins replays local deprovision for every recorded plugin
// on an instance, checking each candidate key (an instance may have been
// referenced by ID or by Name at install time). Used by `spawn terminate`.
func deprovisionAllLocalPlugins(ctx context.Context, instanceKeys ...string) {
	store, err := plugin.DefaultLocalStore()
	if err != nil {
		return
	}
	seen := map[string]bool{}
	for _, key := range instanceKeys {
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		recs, err := store.List(key)
		if err != nil || len(recs) == 0 {
			continue
		}
		for _, rec := range recs {
			runLocalDeprovision(ctx, store, key, rec)
		}
	}
}

// remotePluginInstall sends the resolved spec, config, and buffered pushed
// values to spored's install endpoint over an SSH tunnel. spored runs the
// install asynchronously and returns 202; the outcome is polled separately.
func remotePluginInstall(ctx context.Context, t pushTransport, spec *plugin.PluginSpec, cfg, pushed map[string]string, prov *plugin.Provenance) error {
	payload, err := pluginInstallPayload(spec, cfg, pushed, prov)
	if err != nil {
		return err
	}
	status, body, err := t.call(ctx, http.MethodPost, "/v1/plugins/install", payload)
	if err != nil {
		return err
	}
	if status != http.StatusAccepted {
		return fmt.Errorf("install API: HTTP %d: %s", status, strings.TrimSpace(string(body)))
	}
	return nil
}

// pluginInstallPayload is the body of POST /v1/plugins/install, the same over
// every transport.
func pluginInstallPayload(spec *plugin.PluginSpec, cfg, pushed map[string]string, prov *plugin.Provenance) ([]byte, error) {
	specYAML, err := yaml.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("marshal spec: %w", err)
	}
	payload, err := json.Marshal(map[string]interface{}{
		"spec":       string(specYAML),
		"config":     cfg,
		"pushed":     pushed,
		"provenance": prov,
	})
	if err != nil {
		return nil, fmt.Errorf("encode install request: %w", err)
	}
	return payload, nil
}

// errPreflightUnsupported means the instance's spored predates
// POST /v1/plugins/preflight. Such a spored routes the path to
// DELETE /v1/plugins/{name} and answers 405 (or 404).
var errPreflightUnsupported = errors.New("spored has no preflight endpoint")

// errRemoteCondition marks a remote pre-flight condition that failed on the
// instance; the error text is the condition's message and detail.
var errRemoteCondition = errors.New("remote condition failed on the instance")

// remotePluginPreflight asks spored to evaluate spec's conditions.remote on the
// instance, installing nothing (#1040).
func remotePluginPreflight(ctx context.Context, t pushTransport, spec *plugin.PluginSpec, cfg map[string]string) error {
	specYAML, err := yaml.Marshal(spec)
	if err != nil {
		return fmt.Errorf("marshal spec: %w", err)
	}
	payload, err := json.Marshal(map[string]interface{}{"spec": string(specYAML), "config": cfg})
	if err != nil {
		return fmt.Errorf("encode preflight request: %w", err)
	}
	return pluginPreflightVia(ctx, t, payload)
}

// postPluginPreflight sends a preflight request to spored's push API at baseURL
// directly; see pluginPreflightVia.
func postPluginPreflight(ctx context.Context, client *http.Client, baseURL, token string, payload []byte) (*http.Response, error) {
	return nil, pluginPreflightVia(ctx, httpPush{client: client, baseURL: baseURL, token: token}, payload)
}

// pluginPreflightVia sends a preflight request over t and maps the answer to
// an error: nil when every remote condition passed, one wrapping
// errRemoteCondition (and naming the condition's message) when one failed,
// and errPreflightUnsupported for a spored without the endpoint.
func pluginPreflightVia(ctx context.Context, t pushTransport, payload []byte) error {
	status, body, err := t.call(ctx, http.MethodPost, "/v1/plugins/preflight", payload)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusOK:
		return nil
	case http.StatusPreconditionFailed:
		var pr struct {
			Error string `json:"error"`
		}
		msg := strings.TrimSpace(string(body))
		if json.Unmarshal(body, &pr) == nil && pr.Error != "" {
			msg = pr.Error
		}
		return fmt.Errorf("%w: %s", errRemoteCondition, msg)
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return errPreflightUnsupported
	default:
		return fmt.Errorf("preflight API: HTTP %d: %s", status, strings.TrimSpace(string(body)))
	}
}

// waitForPluginReady polls the remote plugin status until it reaches a terminal
// state (running or failed) or the timeout elapses. It returns an error if the
// plugin failed or never became ready.
func waitForPluginReady(ctx context.Context, t pushTransport, instance, name string) error {
	deadline := time.Now().Add(3 * time.Minute)
	var last plugin.PluginStatus
	for time.Now().Before(deadline) {
		st, err := remotePluginStatus(ctx, t, instance, name)
		if err == nil {
			if st.Status != last {
				fmt.Printf("  %s...\n", st.Status)
				last = st.Status
			}
			switch st.Status {
			case plugin.StatusRunning:
				return nil
			case plugin.StatusFailed:
				if st.Error != "" {
					return fmt.Errorf("plugin %s failed on %s: %s", name, instance, st.Error)
				}
				return fmt.Errorf("plugin %s failed on %s", name, instance)
			case plugin.StatusWaitingForPush:
				// Should not happen in the unified flow (pushed values are seeded
				// before configure); surface it rather than spin forever.
				return fmt.Errorf("plugin %s is waiting for a pushed value that was not provided", name)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	return fmt.Errorf("plugin %s did not become ready on %s within the timeout", name, instance)
}

// ── status ────────────────────────────────────────────────────────────────────

var pluginStatusCmd = &cobra.Command{
	Use:   "status <name>",
	Short: "Show status and outputs of a plugin on an instance",
	Long: `Show a plugin's status on an instance, and its declared outputs.

Outputs are the values a plugin's local provision steps capture under a name its
spec declares in outputs: (for example a Globus endpoint ID). They are recorded
on the machine that ran 'spawn plugin install', so they are shown only there.
Values a plugin captures without declaring them are never shown: they are often
credentials it captures only to push to the instance.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if pluginInstance == "" {
			return fmt.Errorf("--instance is required")
		}

		push, err := pluginTransportFor(cmd.Context(), pluginInstance)
		if err != nil {
			return err
		}
		st, err := remotePluginStatus(cmd.Context(), push, pluginInstance, args[0])
		if err != nil {
			return err
		}

		// Outputs are captured on the controller at install, so they come from
		// this machine's local record rather than from spored (#1117).
		outputs := recordedPluginOutputs(pluginInstance, args[0])
		return writePluginStatus(os.Stdout, st, outputs, pluginJSONOutput || getOutputFormat() == "json")
	},
}

// ── remove ────────────────────────────────────────────────────────────────────

var pluginRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Remove a plugin from an instance",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if pluginInstance == "" {
			return fmt.Errorf("--instance is required")
		}
		name := args[0]

		if !confirmYes(pluginRemoveYes, fmt.Sprintf("Remove plugin %s from %s?", name, pluginInstance)) {
			return newAborted("aborted: plugin %s was left installed on %s", name, pluginInstance)
		}

		fmt.Printf("Removing plugin %s from %s...\n", name, pluginInstance)

		// Tear down the controller-side footprint first (mutagen sync, Globus
		// endpoint, …) using the record written at install time, if any. Keyed on
		// the original --instance value the record was saved under.
		if store, err := plugin.DefaultLocalStore(); err == nil {
			if rec, err := store.Load(pluginInstance, name); err == nil {
				runLocalDeprovision(cmd.Context(), store, pluginInstance, rec)
			}
		}

		push, err := pluginTransportFor(cmd.Context(), pluginInstance)
		if err != nil {
			return err
		}
		status, body, err := push.call(cmd.Context(), http.MethodDelete, fmt.Sprintf("/v1/plugins/%s", name), nil)
		if err != nil {
			return err
		}
		warning, respErr := removeOutcome(status, body, name, pluginInstance)
		if respErr != nil {
			return respErr
		}

		fmt.Printf("Plugin %s removed from %s.\n", name, pluginInstance)
		if warning != "" {
			fmt.Fprintf(os.Stderr, "warning: %s; what the plugin started may still be running on %s\n", warning, pluginInstance)
		}
		return nil
	},
}

// removeOutcome reads spored's answer to DELETE /v1/plugins/{name}: the
// warning to print on success, or the error to return. A 404 means the plugin
// is not installed there and says so (#1038); before spored answered 404 for
// that, it answered 500 and the user saw "remove API: HTTP 500 — internal
// error".
func removeOutcome(status int, body []byte, name, instance string) (warning string, err error) {
	switch status {
	case http.StatusOK:
		return removeWarning(body), nil
	case http.StatusNotFound:
		return "", fmt.Errorf("plugin %q not found on %s", name, instance)
	default:
		return "", fmt.Errorf("remove API: HTTP %d — %s", status, strings.TrimSpace(string(body)))
	}
}

// removeWarning is the "warning" spored puts in a remove response when the
// plugin was removed but its remote.stop steps failed (#1018); "" otherwise,
// including from an older spored, which sends none.
func removeWarning(body []byte) string {
	var r struct {
		Warning string `json:"warning"`
	}
	_ = json.Unmarshal(body, &r)
	return r.Warning
}

// ── validate ──────────────────────────────────────────────────────────────────

var pluginValidateCmd = &cobra.Command{
	Use:   "validate <path>...",
	Short: "Validate plugin.yaml spec files (offline)",
	Long: `Statically validate one or more plugin.yaml files without contacting any
instance. Checks schema, semver, known step/condition/config types, that the
containing directory matches the plugin name, and that every {{ config.X }}
template reference points at a declared config parameter.

With --strict, also enforce that the declared permissions: block is consistent
with the plugin's steps (e.g. instance.root=false must have no remote step that
runs as root). --strict requires a permissions: block. The official registry's
CI runs --strict so a published plugin's declared capability surface is enforced.

Examples:
  spawn plugin validate ./plugins/tailscale/plugin.yaml
  spawn plugin validate --strict ./plugins/*/plugin.yaml`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		out := cmd.OutOrStdout()
		validate := plugin.ValidateSpecFile
		if pluginValidateStrict {
			validate = plugin.ValidateSpecFileStrict
		}
		var failed int
		for _, path := range args {
			if err := validate(path); err != nil {
				failed++
				fmt.Fprintf(out, "✗ %s\n", path)
				for _, line := range strings.Split(err.Error(), "\n") {
					fmt.Fprintf(out, "    %s\n", line)
				}
				continue
			}
			fmt.Fprintf(out, "✓ %s\n", path)
		}
		if failed > 0 {
			return fmt.Errorf("%d of %d plugin spec(s) failed validation", failed, len(args))
		}
		return nil
	},
}

var pluginValidateStrict bool

var pluginManifestOut string

var pluginManifestCmd = &cobra.Command{
	Use:   "manifest <plugin-dir>",
	Short: "Generate a release checksum manifest for a plugin (offline)",
	Long: `Generate the checksum manifest (manifest.json) for a plugin directory. The
manifest records the sha256 of the plugin's plugin.yaml so that spawn can verify
a fetched official plugin matches the released bytes. This is the generator side
of the registry supply-chain story: the registry's release workflow runs it and
publishes the output as a GitHub Release asset; spawn verifies against it at
install time. Contacts nothing.

Examples:
  spawn plugin manifest ./plugins/tailscale
  spawn plugin manifest ./plugins/tailscale -o manifest.json`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		m, err := plugin.BuildManifest(args[0])
		if err != nil {
			return err
		}
		data, err := m.Encode()
		if err != nil {
			return err
		}
		if pluginManifestOut == "" || pluginManifestOut == "-" {
			_, err = cmd.OutOrStdout().Write(data)
			return err
		}
		if err := os.WriteFile(pluginManifestOut, data, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", pluginManifestOut, err)
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "wrote %s manifest for %s %s\n", pluginManifestOut, m.Plugin, m.Version)
		return nil
	},
}

// ── push API helpers ──────────────────────────────────────────────────────────

// pluginStateResponse mirrors plugin.PluginState for JSON decoding.
type pluginStateResponse struct {
	Name       string              `json:"name"`
	Version    string              `json:"version"`
	Status     plugin.PluginStatus `json:"status"`
	Config     map[string]string   `json:"config"`
	Outputs    map[string]string   `json:"outputs"`
	UpdatedAt  time.Time           `json:"updated_at"`
	LastHealth *time.Time          `json:"last_health,omitempty"`
	Error      string              `json:"error,omitempty"`
	Provenance *plugin.Provenance  `json:"provenance,omitempty"`
}

func remotePluginList(ctx context.Context, t pushTransport) ([]*pluginStateResponse, error) {
	status, body, err := t.call(ctx, http.MethodGet, "/v1/plugins", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("list plugins: HTTP %d", status)
	}
	var states []*pluginStateResponse
	return states, json.Unmarshal(body, &states)
}

func remotePluginStatus(ctx context.Context, t pushTransport, instance, name string) (*pluginStateResponse, error) {
	status, body, err := t.call(ctx, http.MethodGet, fmt.Sprintf("/v1/plugins/%s/status", name), nil)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, fmt.Errorf("plugin %q not found on %s", name, instance)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("status API: HTTP %d", status)
	}
	var st pluginStateResponse
	return &st, json.Unmarshal(body, &st)
}

// sshReadToken reads the push bearer token from the remote instance.
func sshReadToken(ctx context.Context, instance, keyPath string) (string, error) {
	args := pluginSSHArgs(keyPath)
	args = append(args, instance, "sudo", "cat", "/var/lib/spored/push.token")

	var stdout, stderr bytes.Buffer
	cmd, err := sshtarget.Command(ctx, "ssh", args...)
	if err != nil {
		return "", err
	}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ssh read token: %w — %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// withSSHTunnel opens an SSH tunnel forwarding 127.0.0.1:7777 from the remote,
// calls fn, and returns its result.  The tunnel is torn down when fn returns.
func withSSHTunnel(ctx context.Context, instance, keyPath string, fn func() (*http.Response, error)) (*http.Response, error) {
	tunnelCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	args := pluginSSHArgs(keyPath)
	args = append(args,
		"-N",
		"-L", "7777:127.0.0.1:7777",
		"-o", "ExitOnForwardFailure=yes",
		instance,
	)

	tunnel, err := sshtarget.Command(tunnelCtx, "ssh", args...)
	if err != nil {
		return nil, err
	}
	if err := tunnel.Start(); err != nil {
		return nil, fmt.Errorf("%w: start SSH tunnel: %v", errPushUnreachable, err)
	}

	// Wait for the port-forward to become ready (max 5 s).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:7777", 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	return fn()
}

// ensureHostIdentity makes the instance's launch key the IdentityFile for
// hostIP in a spawn-managed ssh_config include, so local plugin steps that shell
// out to the system ssh (e.g. mutagen, which has no key flag) authenticate to
// ec2-user@hostIP. Unlike loading the key into ssh-agent, an ssh_config
// IdentityFile is honored by every ssh regardless of which agent IdentityAgent
// points at (e.g. a read-only 1Password agent). Best-effort: resolves the key
// from --key or the instance's KeyName (the lookup `spawn connect` uses); if no
// key is found it logs and relies on the user's ambient SSH setup.
func ensureHostIdentity(ctx context.Context, instance, hostIP string) {
	if hostIP == "" {
		return
	}
	keyPath := pluginKeyPath
	if keyPath == "" {
		if inst := resolveInstanceViaSpawn(ctx, instance); inst != nil && inst.KeyName != "" {
			if p, err := findSSHKey(inst.KeyName); err == nil {
				keyPath = p
			}
		}
	}
	if keyPath == "" {
		return // rely on the user's ambient SSH setup
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	if err := sshkey.EnsureHostIdentity(home, hostIP, keyPath); err != nil {
		fmt.Fprintf(os.Stderr, "note: could not configure SSH identity for %s (%v); local SSH-based steps rely on your ambient SSH setup\n", hostIP, err)
	}
}

// removeHostIdentityForIP drops the spawn-managed ssh_config identity block for
// an instance's IP. Called on terminate so a block written by a plugin's local
// provision (e.g. tailscale, which has no deprovision record to trigger cleanup)
// doesn't leak a stale Host entry pointing at a dead address. Best-effort no-op
// when there's no block or no IP.
func removeHostIdentityForIP(ip string) {
	if ip == "" {
		return
	}
	if home, err := os.UserHomeDir(); err == nil {
		_ = sshkey.RemoveHostIdentity(home, ip)
	}
}

// pluginSSHArgs returns common SSH options.
func pluginSSHArgs(keyPath string) []string {
	args := []string{
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ConnectTimeout=10",
		"-o", "BatchMode=yes",
	}
	if keyPath != "" {
		args = append(args, "-i", keyPath)
	}
	return args
}

// lookupInstanceInfo returns the Name and public IP for an instance identifier
// (ID or name), resolved through spawn's own instance lookup — the plugin layer
// never talks to AWS directly. On any failure it falls back to the given
// identifier as the name with an empty IP, so callers degrade gracefully (the
// template context simply won't have instance.ip set, and any step that needs it
// fails loudly at render time).
func lookupInstanceInfo(ctx context.Context, instance string) (name, publicIP string) {
	inst := resolveInstanceViaSpawn(ctx, instance)
	if inst == nil {
		return instance, ""
	}
	name = inst.Name
	if name == "" {
		name = instance
	}
	return name, inst.PublicIP
}

// resolveInstanceViaSpawn resolves an instance identifier (ID or Name) to
// spawn's InstanceInfo using the same client + resolver every other command
// uses. Returns nil on any error (best-effort; callers degrade gracefully).
func resolveInstanceViaSpawn(ctx context.Context, instance string) *aws.InstanceInfo {
	client, err := aws.NewClient(ctx)
	if err != nil {
		return nil
	}
	inst, err := resolveInstance(ctx, client, instance)
	if err != nil {
		return nil
	}
	return inst
}

// ensureInstanceReady blocks until the instance is fully provisioned — spored
// active over SSM, which coincides with cloud-init finishing (local user
// created, keys installed, network/DNS up). This is the same deterministic gate
// `spawn launch` applies; commands that act on an instance right after launch
// (plugin install, reconcile) use it so their work doesn't race the boot.
func ensureInstanceReady(ctx context.Context, region, instanceID string, timeout time.Duration) error {
	client, err := aws.NewClientWithRegion(ctx, region)
	if err != nil {
		return fmt.Errorf("aws client: %w", err)
	}
	return verifySporedReady(ctx, client, region, instanceID, timeout)
}

// parseKeyValuePairs converts []string{"k=v"} to map[string]string.
func parseKeyValuePairs(pairs []string) (map[string]string, error) {
	m := make(map[string]string)
	for _, p := range pairs {
		parts := strings.SplitN(p, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid --config value %q (expected key=value)", p)
		}
		m[parts[0]] = parts[1]
	}
	return m, nil
}

func init() {
	rootCmd.AddCommand(pluginCmd)
	pluginCmd.AddCommand(pluginListCmd)
	pluginCmd.AddCommand(pluginInstallCmd)
	pluginCmd.AddCommand(pluginInspectCmd)
	pluginCmd.AddCommand(pluginStatusCmd)
	pluginCmd.AddCommand(pluginRemoveCmd)
	pluginCmd.AddCommand(pluginValidateCmd)
	pluginValidateCmd.Flags().BoolVar(&pluginValidateStrict, "strict", false, "Also enforce permissions/step consistency (requires a permissions: block)")
	pluginCmd.AddCommand(pluginManifestCmd)
	pluginManifestCmd.Flags().StringVarP(&pluginManifestOut, "output", "o", "", "Write manifest to this file instead of stdout")

	// Shared flags across all subcommands.
	for _, sub := range []*cobra.Command{pluginListCmd, pluginInstallCmd, pluginStatusCmd, pluginRemoveCmd} {
		sub.Flags().StringVarP(&pluginInstance, "instance", "i", "", "Instance ID or hostname (required)")
		sub.Flags().BoolVar(&pluginJSONOutput, "json", false, "JSON output")
		_ = sub.Flags().MarkDeprecated("json", "use --output json instead")
		sub.Flags().StringVar(&pluginKeyPath, "key", "", "Path to SSH private key")
		sub.Flags().StringVar(&pluginSSHUser, "user", "", "SSH username for the instance (default: ec2-user)")
	}

	pluginRemoveCmd.Flags().BoolVarP(&pluginRemoveYes, "yes", "y", false, "Skip the confirmation prompt")

	// Install-only flags.
	pluginInstallCmd.Flags().StringArrayVar(&pluginConfigPairs, "config", nil, "Config as key=value (repeatable)")
	pluginInstallCmd.Flags().BoolVar(&pluginDryRun, "dry-run", false, "Preview the plan without installing (contacts no instance)")

	// --insecure: skip signature/checksum-manifest verification for official
	// versioned refs (dev escape for unreleased/unsigned plugins).
	for _, sub := range []*cobra.Command{pluginInstallCmd, pluginInspectCmd} {
		sub.Flags().BoolVar(&pluginInsecure, "insecure", false, "Skip signature/checksum verification for official plugin releases (unsafe)")
	}
}
