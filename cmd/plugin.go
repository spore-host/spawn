package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/plugin"
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
)

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
		sshHost, err := resolvePluginSSHHost(cmd.Context(), pluginInstance)
		if err != nil {
			return err
		}
		states, err := remotePluginList(cmd.Context(), sshHost)
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

Requires SSH access to the instance (the same key used by 'spawn plugin status').

Plugin ref formats:
  name                  official registry (spore-host/spore-plugins)
  name@v1.2.0           pinned to git tag
  github:user/repo/name custom GitHub repository
  ./path/to/plugin.yaml local file`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
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

func runPluginInstall(ctx context.Context, ref, instance string, cfg map[string]string) error {
	resolver := plugin.DefaultResolver()
	spec, err := resolver.Resolve(ctx, ref)
	if err != nil {
		return fmt.Errorf("resolve plugin %q: %w", ref, err)
	}

	fmt.Printf("Installing %s %s on %s...\n", spec.Name, spec.Version, instance)

	// Check local pre-flight conditions before touching the remote.
	localExec := plugin.NewLocalExecutor(nil)
	if err := localExec.CheckLocalConditions(spec.Conditions.Local); err != nil {
		return fmt.Errorf("local condition: %w", err)
	}

	resolvedCfg, err := spec.ResolvedConfig(cfg)
	if err != nil {
		return err
	}

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

	// Run local provision steps on the controller. Push steps are buffered
	// rather than delivered immediately, so their values can be handed to spored
	// with the install request below — that guarantees they are present before
	// the remote configure phase runs.
	pushBuf := plugin.NewBufferingPushClient()
	var localOutputs map[string]string
	if len(spec.Local.Provision) > 0 {
		fmt.Println("Running local provision steps...")
		execWithPush := plugin.NewLocalExecutor(pushBuf)
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
	if len(spec.Local.Deprovision) > 0 {
		saveLocalPluginRecord(instance, ref, spec, tmplCtx.Instance, resolvedCfg, localOutputs)
	}

	// Run the remote half (install → configure → start) on the instance via
	// spored, handing over the resolved config and any buffered pushed values.
	hasRemote := len(spec.Remote.Install) > 0 || len(spec.Remote.Configure) > 0 ||
		len(spec.Remote.Start) > 0
	if hasRemote {
		sshHost, err := resolvePluginSSHHost(ctx, instance)
		if err != nil {
			return err
		}
		fmt.Println("Running remote install steps on the instance...")
		if err := remotePluginInstall(ctx, sshHost, spec, resolvedCfg, pushBuf.Values()); err != nil {
			return fmt.Errorf("remote install: %w", err)
		}
		if err := waitForPluginReady(ctx, sshHost, spec.Name); err != nil {
			return err
		}
	}

	fmt.Printf("Plugin %s installed on %s.\n", spec.Name, instance)
	fmt.Printf("Use 'spawn plugin status %s --instance %s' to check status.\n", spec.Name, instance)
	return nil
}

// saveLocalPluginRecord persists a controller-side record of an installed
// plugin so its local deprovision steps can be replayed later. Best-effort: a
// failure here is logged but does not fail the install (the plugin is already
// installed; the cost is a potential orphaned local footprint on removal).
func saveLocalPluginRecord(instance, ref string, spec *plugin.PluginSpec, instanceVars, cfg, outputs map[string]string) {
	store, err := plugin.DefaultLocalStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not open local plugin store: %v\n", err)
		return
	}
	rec := &plugin.LocalRecord{
		Name:        spec.Name,
		Ref:         ref,
		InstanceID:  instance,
		Instance:    instanceVars,
		Config:      cfg,
		Outputs:     outputs,
		Deprovision: spec.Local.Deprovision,
		InstalledAt: time.Now(),
	}
	if err := store.Save(rec); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not record local plugin state for %s: %v\n", spec.Name, err)
	}
}

// runLocalDeprovision replays a recorded plugin's local deprovision steps on the
// controller (e.g. terminate the mutagen sync, delete the Globus endpoint) and
// removes the record. Best-effort: errors are reported but not fatal, since the
// caller is usually tearing the instance down regardless.
func runLocalDeprovision(ctx context.Context, store *plugin.LocalStore, instanceKey string, rec *plugin.LocalRecord) {
	tmplCtx := plugin.NewTemplateContext()
	// Replay with the exact instance.* values provision used (name/ip/id), so a
	// deprovision step like `mutagen sync terminate spore-{{ instance.name }}`
	// targets the same session that provision created.
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

	fmt.Printf("Running local deprovision for plugin %s...\n", rec.Name)
	exec := plugin.NewLocalExecutor(nil)
	if err := exec.RunDeprovision(ctx, rec.Deprovision, tmplCtx); err != nil {
		fmt.Fprintf(os.Stderr, "warning: local deprovision for %s failed (local resources may be orphaned): %v\n", rec.Name, err)
	}
	if err := store.Delete(instanceKey, rec.Name); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not remove local plugin record for %s: %v\n", rec.Name, err)
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
func remotePluginInstall(ctx context.Context, instance string, spec *plugin.PluginSpec, cfg, pushed map[string]string) error {
	specYAML, err := yaml.Marshal(spec)
	if err != nil {
		return fmt.Errorf("marshal spec: %w", err)
	}

	payload, err := json.Marshal(map[string]interface{}{
		"spec":   string(specYAML),
		"config": cfg,
		"pushed": pushed,
	})
	if err != nil {
		return fmt.Errorf("encode install request: %w", err)
	}

	token, err := sshReadToken(ctx, instance, pluginKeyPath)
	if err != nil {
		return err
	}

	_, err = withSSHTunnel(ctx, instance, pluginKeyPath, func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			"http://127.0.0.1:7777/v1/plugins/install", bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusAccepted {
			body, _ := io.ReadAll(resp.Body)
			return resp, fmt.Errorf("install API: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		return resp, nil
	})
	return err
}

// waitForPluginReady polls the remote plugin status until it reaches a terminal
// state (running or failed) or the timeout elapses. It returns an error if the
// plugin failed or never became ready.
func waitForPluginReady(ctx context.Context, instance, name string) error {
	deadline := time.Now().Add(3 * time.Minute)
	var last plugin.PluginStatus
	for time.Now().Before(deadline) {
		st, err := remotePluginStatus(ctx, instance, name)
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
	Short: "Show status of a plugin on an instance",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if pluginInstance == "" {
			return fmt.Errorf("--instance is required")
		}

		sshHost, err := resolvePluginSSHHost(cmd.Context(), pluginInstance)
		if err != nil {
			return err
		}
		st, err := remotePluginStatus(cmd.Context(), sshHost, args[0])
		if err != nil {
			return err
		}

		if pluginJSONOutput || getOutputFormat() == "json" {
			return json.NewEncoder(os.Stdout).Encode(st)
		}

		fmt.Printf("Plugin:  %s\n", st.Name)
		fmt.Printf("Version: %s\n", st.Version)
		fmt.Printf("Status:  %s\n", st.Status)
		fmt.Printf("Updated: %s\n", st.UpdatedAt.Format(time.RFC3339))
		if st.Error != "" {
			fmt.Printf("Error:   %s\n", st.Error)
		}
		if st.LastHealth != nil {
			fmt.Printf("Health:  last OK %s\n", st.LastHealth.Format(time.RFC3339))
		}
		return nil
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
			fmt.Println("Aborted.")
			return nil
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

		sshHost, err := resolvePluginSSHHost(cmd.Context(), pluginInstance)
		if err != nil {
			return err
		}

		token, err := sshReadToken(cmd.Context(), sshHost, pluginKeyPath)
		if err != nil {
			return fmt.Errorf("read token: %w", err)
		}

		var respErr error
		_, err = withSSHTunnel(cmd.Context(), sshHost, pluginKeyPath, func() (*http.Response, error) {
			url := fmt.Sprintf("http://127.0.0.1:7777/v1/plugins/%s", name)
			req, err := http.NewRequestWithContext(cmd.Context(), http.MethodDelete, url, nil)
			if err != nil {
				return nil, err
			}
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return nil, err
			}
			if resp.StatusCode == http.StatusNotFound {
				respErr = fmt.Errorf("plugin %q not found on %s", name, pluginInstance)
			} else if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				respErr = fmt.Errorf("remove API: HTTP %d — %s", resp.StatusCode, string(body))
			}
			return resp, nil
		})
		if err != nil {
			return fmt.Errorf("SSH tunnel: %w", err)
		}
		if respErr != nil {
			return respErr
		}

		fmt.Printf("Plugin %s removed from %s.\n", name, pluginInstance)
		return nil
	},
}

// ── validate ──────────────────────────────────────────────────────────────────

var pluginValidateCmd = &cobra.Command{
	Use:   "validate <path>...",
	Short: "Validate plugin.yaml spec files (offline)",
	Long: `Statically validate one or more plugin.yaml files without contacting any
instance. Checks schema, semver, known step/condition/config types, that the
containing directory matches the plugin name, and that every {{ config.X }}
template reference points at a declared config parameter.

Examples:
  spawn plugin validate ./plugins/tailscale/plugin.yaml
  spawn plugin validate ./plugins/*/plugin.yaml`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		out := cmd.OutOrStdout()
		var failed int
		for _, path := range args {
			if err := plugin.ValidateSpecFile(path); err != nil {
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
}

func remotePluginList(ctx context.Context, instance string) ([]*pluginStateResponse, error) {
	token, err := sshReadToken(ctx, instance, pluginKeyPath)
	if err != nil {
		return nil, err
	}

	var states []*pluginStateResponse
	_, err = withSSHTunnel(ctx, instance, pluginKeyPath, func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:7777/v1/plugins", nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return resp, fmt.Errorf("list plugins: HTTP %d", resp.StatusCode)
		}
		return resp, json.NewDecoder(resp.Body).Decode(&states)
	})
	return states, err
}

func remotePluginStatus(ctx context.Context, instance, name string) (*pluginStateResponse, error) {
	token, err := sshReadToken(ctx, instance, pluginKeyPath)
	if err != nil {
		return nil, err
	}

	var st pluginStateResponse
	_, err = withSSHTunnel(ctx, instance, pluginKeyPath, func() (*http.Response, error) {
		url := fmt.Sprintf("http://127.0.0.1:7777/v1/plugins/%s/status", name)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode == http.StatusNotFound {
			return resp, fmt.Errorf("plugin %q not found on %s", name, instance)
		}
		if resp.StatusCode != http.StatusOK {
			return resp, fmt.Errorf("status API: HTTP %d", resp.StatusCode)
		}
		return resp, json.NewDecoder(resp.Body).Decode(&st)
	})
	return &st, err
}

// sshReadToken reads the push bearer token from the remote instance.
func sshReadToken(ctx context.Context, instance, keyPath string) (string, error) {
	args := pluginSSHArgs(keyPath)
	args = append(args, instance, "sudo", "cat", "/var/lib/spored/push.token")

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "ssh", args...)
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

	tunnel := exec.CommandContext(tunnelCtx, "ssh", args...)
	if err := tunnel.Start(); err != nil {
		return nil, fmt.Errorf("start SSH tunnel: %w", err)
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

// lookupInstanceInfo returns the EC2 Name tag and public IP for an instance ID
// in a single DescribeInstances call. On any failure it falls back to the given
// identifier for the name and an empty IP, so callers degrade gracefully (the
// template context simply won't have instance.ip set, and any step that needs it
// fails loudly at render time).
func lookupInstanceInfo(ctx context.Context, instance string) (name, publicIP string) {
	name, publicIP, _ = lookupInstanceDetails(ctx, instance)
	return name, publicIP
}

// lookupInstanceDetails resolves an EC2 instance ID to its Name tag, public IP,
// and SSH login user (the spawn:local-username tag, defaulting to ec2-user). For
// a non-ID identifier (a hostname/IP the user passed directly) it returns the
// identifier as the name with an empty IP/user.
func lookupInstanceDetails(ctx context.Context, instance string) (name, publicIP, user string) {
	if !strings.HasPrefix(instance, "i-") {
		return instance, "", ""
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return instance, "", ""
	}
	out, err := ec2.NewFromConfig(cfg).DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instance},
	})
	if err != nil || len(out.Reservations) == 0 || len(out.Reservations[0].Instances) == 0 {
		return instance, "", ""
	}
	inst := out.Reservations[0].Instances[0]
	name = instance
	for _, tag := range inst.Tags {
		switch aws.ToString(tag.Key) {
		case "Name":
			name = aws.ToString(tag.Value)
		case "spawn:local-username":
			user = aws.ToString(tag.Value)
		}
	}
	return name, aws.ToString(inst.PublicIpAddress), user
}

// resolvePluginSSHHost turns the --instance value into a host SSH can reach. When
// it is an EC2 instance ID, it is resolved to user@public-ip (the plugin push
// API is only reachable over SSH). A value that is already a hostname/IP (or
// user@host) is returned unchanged. Errors if an instance ID has no public IP.
//
// The SSH user defaults to ec2-user (matching `spawn connect`), since that is
// the account the EC2 key pair authorizes; --user overrides it.
func resolvePluginSSHHost(ctx context.Context, instance string) (string, error) {
	if !strings.HasPrefix(instance, "i-") {
		return instance, nil // already a hostname/IP/user@host
	}
	_, ip, _ := lookupInstanceDetails(ctx, instance)
	if ip == "" {
		return "", fmt.Errorf("instance %s has no public IP to SSH to (plugin commands need SSH access)", instance)
	}
	user := pluginSSHUser
	if user == "" {
		user = "ec2-user"
	}
	return user + "@" + ip, nil
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
	pluginCmd.AddCommand(pluginStatusCmd)
	pluginCmd.AddCommand(pluginRemoveCmd)
	pluginCmd.AddCommand(pluginValidateCmd)

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
}
