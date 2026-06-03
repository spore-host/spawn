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
	"text/tabwriter"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/plugin"
)

// plugin-level flags shared across subcommands
var (
	pluginInstance    string
	pluginJSONOutput  bool // deprecated: use --output json
	pluginKeyPath     string
	pluginConfigPairs []string
	pluginRemoveYes   bool
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
		states, err := remotePluginList(cmd.Context(), pluginInstance)
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

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
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

	fmt.Printf("Installing %s v%s on %s...\n", spec.Name, spec.Version, instance)

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
	tmplCtx.Instance["id"] = instance
	tmplCtx.Instance["name"] = lookupInstanceName(ctx, instance)
	for k, v := range resolvedCfg {
		tmplCtx.Config[k] = v
	}

	// Run local provision steps (may capture outputs and push values remotely).
	if len(spec.Local.Provision) > 0 {
		fmt.Println("Running local provision steps...")
		pushClient := plugin.NewSSHTunnelPushClient(instance, pluginKeyPath)
		execWithPush := plugin.NewLocalExecutor(pushClient)
		if _, err := execWithPush.RunProvision(ctx, spec.Name, spec.Local.Provision, tmplCtx); err != nil {
			return fmt.Errorf("local provision: %w", err)
		}
	}

	fmt.Printf("Plugin %s installed on %s.\n", spec.Name, instance)
	fmt.Printf("Use 'spawn plugin status %s --instance %s' to check status.\n", spec.Name, instance)
	return nil
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

		st, err := remotePluginStatus(cmd.Context(), pluginInstance, args[0])
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

		token, err := sshReadToken(cmd.Context(), pluginInstance, pluginKeyPath)
		if err != nil {
			return fmt.Errorf("read token: %w", err)
		}

		var respErr error
		_, err = withSSHTunnel(cmd.Context(), pluginInstance, pluginKeyPath, func() (*http.Response, error) {
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

// lookupInstanceName returns the EC2 Name tag for an instance ID, or the
// instance value itself if it is not an EC2 instance ID or the lookup fails.
func lookupInstanceName(ctx context.Context, instance string) string {
	if !strings.HasPrefix(instance, "i-") {
		return instance
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return instance
	}
	out, err := ec2.NewFromConfig(cfg).DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instance},
	})
	if err != nil || len(out.Reservations) == 0 || len(out.Reservations[0].Instances) == 0 {
		return instance
	}
	for _, tag := range out.Reservations[0].Instances[0].Tags {
		if aws.ToString(tag.Key) == "Name" {
			return aws.ToString(tag.Value)
		}
	}
	return instance
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
	}

	pluginRemoveCmd.Flags().BoolVarP(&pluginRemoveYes, "yes", "y", false, "Skip the confirmation prompt")

	// Install-only flags.
	pluginInstallCmd.Flags().StringArrayVar(&pluginConfigPairs, "config", nil, "Config as key=value (repeatable)")
}
