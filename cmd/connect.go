package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spore-host/libs/i18n"
	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/sshkey"
)

var (
	connectUser       string
	connectKey        string
	connectPort       int
	connectSessionMgr bool
	connectNoStart    bool
)

var connectCmd = &cobra.Command{
	Use:     "connect <instance-id> [-- <command>...]",
	RunE:    runConnect,
	Aliases: []string{"ssh"},
	Args:    cobra.MinimumNArgs(1),
	// Short and Long will be set after i18n initialization
}

func init() {
	rootCmd.AddCommand(connectCmd)

	connectCmd.Flags().StringVar(&connectUser, "user", "", "SSH username (default: ec2-user)")
	connectCmd.Flags().StringVar(&connectKey, "key", "", "SSH private key path")
	connectCmd.Flags().IntVar(&connectPort, "port", 22, "SSH port")
	connectCmd.Flags().BoolVar(&connectSessionMgr, "session-manager", false, "Use AWS Session Manager instead of SSH")
	connectCmd.Flags().BoolVar(&connectNoStart, "no-start", false, "Do not automatically start a stopped/hibernated instance")

	// Register completion for instance ID argument
	connectCmd.ValidArgsFunction = completeInstanceID
}

func runConnect(cmd *cobra.Command, args []string) error {
	instanceIdentifier := args[0]
	ctx := context.Background()

	// Create AWS client
	client, err := aws.NewClient(ctx)
	if err != nil {
		return i18n.Te("error.aws_client_init", err)
	}

	// Resolve instance (by ID or name)
	instance, err := resolveInstance(ctx, client, instanceIdentifier)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "%s\n", i18n.Tf("spawn.connect.found_instance", map[string]interface{}{
		"Region": instance.Region,
		"State":  instance.State,
	}))

	// DCV application streaming session — open/focus browser instead of SSH.
	if instance.Tags["spawn:dcv-session-id"] != "" {
		return connectDCV(ctx, client, instance)
	}

	// Auto-start stopped/hibernated instances unless --no-start is set.
	if instance.State == "stopped" || instance.State == "stopping" {
		if connectNoStart {
			return fmt.Errorf("instance is %s — use --no-start=false or start it manually with: spawn start %s", instance.State, instanceIdentifier)
		}
		fmt.Fprintf(os.Stderr, "Instance is %s — starting it...\n", instance.State)
		if err := client.StartInstance(ctx, instance.Region, instance.InstanceID); err != nil {
			return fmt.Errorf("start instance: %w", err)
		}
		fmt.Fprintf(os.Stderr, "Waiting for instance to reach running state")
		for i := 0; i < 30; i++ {
			time.Sleep(5 * time.Second)
			fmt.Fprintf(os.Stderr, ".")
			instances, err := client.ListInstances(ctx, instance.Region, "running")
			if err != nil {
				continue
			}
			for idx := range instances {
				if instances[idx].InstanceID == instance.InstanceID {
					instance = &instances[idx]
					fmt.Fprintf(os.Stderr, " running\n\n")
					goto instanceReady
				}
			}
		}
		return fmt.Errorf("instance did not reach running state within 2.5 minutes")
	instanceReady:
	}

	// Non-startable states (pending, shutting-down, terminated)
	if instance.State != "running" {
		return i18n.Te("spawn.connect.error.not_running", nil, map[string]interface{}{
			"State": instance.State,
		})
	}

	// Use Session Manager if requested or if no public IP
	if connectSessionMgr || instance.PublicIP == "" {
		return connectViaSessionManager(instance.InstanceID, instance.Region)
	}

	// Determine SSH user
	user := connectUser
	if user == "" {
		user = "ec2-user" // Default for Amazon Linux
	}

	// Determine SSH key
	keyPath := connectKey
	if keyPath == "" {
		// Try to find the key based on the instance key name
		keyPath, err = findSSHKey(instance.KeyName)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %s: %v\n", i18n.Symbol("warning"), i18n.Tf("spawn.connect.key_not_found", map[string]interface{}{
				"KeyName": instance.KeyName,
			}), err)
			fmt.Fprintf(os.Stderr, "%s\n\n", i18n.T("spawn.connect.fallback_session_manager"))
			return connectViaSessionManager(instance.InstanceID, instance.Region)
		}
	}

	// Build SSH command. ControlMaster=no / ControlPath=none keep spawn's SSH
	// independent of the user's ~/.ssh/config connection multiplexing, so many
	// concurrent `spawn connect` calls don't serialize on one shared control
	// socket (#56).
	sshArgs := []string{
		"-i", keyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ControlMaster=no",
		"-o", "ControlPath=none",
		"-p", fmt.Sprintf("%d", connectPort),
		fmt.Sprintf("%s@%s", user, instance.PublicIP),
	}

	// One-shot mode: args[1:] (after --) form the remote command.
	// Wrap in `bash -c '...'` so the remote shell interprets operators (&&, ;,
	// &, pipes) correctly and backgrounded processes (&) don't cause SSH to
	// exit 255 (fixes #315). Interactive mode leaves sshArgs unchanged.
	if len(args) > 1 {
		remoteCmd := strings.Join(args[1:], " ")
		// Escape any single quotes in the command before wrapping
		remoteCmd = strings.ReplaceAll(remoteCmd, "'", "'\\''")
		sshArgs = append(sshArgs, "bash -c '"+remoteCmd+"'")
	}

	fmt.Fprintf(os.Stderr, "%s\n\n", i18n.Tf("spawn.connect.connecting_ssh", map[string]interface{}{
		"Command": "ssh " + strings.Join(sshArgs, " "),
	}))

	// Execute SSH
	sshCmd := exec.Command("ssh", sshArgs...)
	sshCmd.Stdin = os.Stdin
	sshCmd.Stdout = os.Stdout
	sshCmd.Stderr = os.Stderr

	return sshCmd.Run()
}

func connectViaSessionManager(instanceID, region string) error {
	// Check if AWS CLI and Session Manager plugin are installed
	_, err := exec.LookPath("aws")
	if err != nil {
		return i18n.Te("spawn.connect.error.aws_cli_not_found", nil)
	}

	fmt.Fprintf(os.Stderr, "%s\n\n", i18n.T("spawn.connect.connecting_session_manager"))

	// Build AWS SSM start-session command
	ssmCmd := exec.Command("aws", "ssm", "start-session",
		"--target", instanceID,
		"--region", region,
	)

	ssmCmd.Stdin = os.Stdin
	ssmCmd.Stdout = os.Stdout
	ssmCmd.Stderr = os.Stderr

	err = ssmCmd.Run()
	if err != nil {
		return i18n.Te("spawn.connect.error.session_manager_failed", err)
	}

	return nil
}

// findSSHKey resolves a usable private key for an EC2 key name. It delegates to
// sshkey.Resolve — the single resolver shared by connect, status, and queue —
// which checks spawn-managed keys (~/.spawn/keys) first, then falls back to the
// user's ~/.ssh keys for back-compat.
func findSSHKey(keyName string) (string, error) {
	if keyName == "" {
		return "", i18n.Te("spawn.connect.error.no_key_name", nil)
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	path, err := sshkey.Resolve(homeDir, keyName)
	if err != nil {
		return "", i18n.Te("spawn.connect.error.key_not_found_for_name", nil, map[string]interface{}{
			"KeyName": keyName,
		})
	}
	return path, nil
}

// ── DCV session connect ───────────────────────────────────────────────────────

// connectDCV handles reconnecting to a NICE DCV application streaming session.
// It wakes a stopped instance, tries to focus an existing browser tab, then
// falls back to opening the session HTML file or the raw DCV URL.
func connectDCV(ctx context.Context, client *aws.Client, instance *aws.InstanceInfo) error {
	appName := instance.Tags["spawn:app-name"]
	if appName == "" {
		appName = "app"
	}

	// Wake the instance if it has been stopped by idle timeout.
	if instance.State == "stopped" {
		fmt.Fprintf(os.Stderr, "Instance is stopped — starting it back up...\n")
		if err := client.StartInstance(ctx, instance.Region, instance.InstanceID); err != nil {
			return fmt.Errorf("start instance: %w", err)
		}
		fmt.Fprintf(os.Stderr, "Waiting for instance to reach running state")
		for i := 0; i < 30; i++ {
			time.Sleep(5 * time.Second)
			fmt.Fprintf(os.Stderr, ".")
			instances, err := client.ListInstances(ctx, instance.Region, "running")
			if err != nil {
				continue
			}
			for idx := range instances {
				if instances[idx].InstanceID == instance.InstanceID {
					instance = &instances[idx]
					fmt.Fprintf(os.Stderr, " running\n")
					goto instanceRunning
				}
			}
		}
		return fmt.Errorf("instance did not reach running state within 2.5 minutes")
	instanceRunning:
	}

	// Wait for spored to write a fresh spawn:ready-url (new token after restart)
	fmt.Fprintf(os.Stderr, "Waiting for DCV session")
	var readyURL, authToken string
	for i := 0; i < 60; i++ {
		time.Sleep(5 * time.Second)
		fmt.Fprintf(os.Stderr, ".")
		instances, err := client.ListInstances(ctx, instance.Region, "running")
		if err != nil {
			continue
		}
		for idx := range instances {
			if instances[idx].InstanceID != instance.InstanceID {
				continue
			}
			instance = &instances[idx]
			if url := instance.Tags["spawn:ready-url"]; url != "" {
				if idx2 := strings.Index(url, "authToken="); idx2 >= 0 {
					authToken = url[idx2+10:]
					// strip any trailing fragment
					if amp := strings.Index(authToken, "&"); amp >= 0 {
						authToken = authToken[:amp]
					}
					if hash := strings.Index(authToken, "#"); hash >= 0 {
						authToken = authToken[:hash]
					}
				}
				readyURL = url
			}
		}
		if authToken != "" {
			fmt.Fprintf(os.Stderr, " ready\n")
			break
		}
	}
	if authToken == "" {
		fmt.Fprintf(os.Stderr, " (timed out)\n")
	}

	// Try to focus an existing browser tab containing this instance ID.
	if focusDCVTab(instance.InstanceID) {
		fmt.Fprintf(os.Stdout, "✓ Reconnected to %s session.\n", appName)
		return nil
	}

	// Find the session HTML file and update it with the new token, then open it.
	sessionsDir, err := getSessionsDir()
	if err == nil {
		if path := findSessionFile(sessionsDir, instance.InstanceID); path != "" {
			if authToken != "" {
				// Rewrite with fresh token so the page redirects with valid auth
				if err := updateSessionHTMLToken(path, authToken, instance); err == nil {
					fmt.Fprintf(os.Stderr, "Opening session: %s\n", path)
					return openBrowser(path)
				}
			}
			fmt.Fprintf(os.Stderr, "Opening session: %s\n", path)
			return openBrowser(path)
		}
	}

	// Final fallback: open DCV URL directly with the new token.
	if readyURL != "" {
		fmt.Fprintf(os.Stderr, "Opening DCV: %s\n", readyURL)
		return openBrowser(readyURL)
	}
	host := instance.PublicIP
	if dns := instance.Tags["spawn:dns-name"]; dns != "" {
		host = dns + ".c0zxr0ao.spore.host"
	}
	if host == "" {
		return fmt.Errorf("instance has no public IP or DNS name")
	}
	return openBrowser(fmt.Sprintf("https://%s:8443", host))
}

// updateSessionHTMLToken rewrites the AUTH_TOKEN constant in a session HTML file.
func updateSessionHTMLToken(path, token string, instance *aws.InstanceInfo) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	content := string(data)
	// Replace AUTH_TOKEN value
	start := strings.Index(content, "const AUTH_TOKEN = '")
	if start < 0 {
		return fmt.Errorf("AUTH_TOKEN not found in session file")
	}
	start += len("const AUTH_TOKEN = '")
	end := strings.Index(content[start:], "'")
	if end < 0 {
		return fmt.Errorf("AUTH_TOKEN end not found")
	}
	content = content[:start] + token + content[start+end:]
	return os.WriteFile(path, []byte(content), 0644)
}

// focusDCVTab tries to bring an existing browser tab containing instanceID to
// the foreground. Returns true if a tab was found and focused.
func focusDCVTab(instanceID string) bool {
	switch runtime.GOOS {
	case "darwin":
		return focusDCVTabMacOS(instanceID)
	case "linux":
		// wmctrl -a searches window titles; our HTML title contains the instance ID.
		return exec.Command("wmctrl", "-a", instanceID).Run() == nil
	}
	return false
}

func focusDCVTabMacOS(instanceID string) bool {
	// AppleScript: search Chrome then Safari for a tab whose title contains instanceID.
	for _, browser := range []string{"Google Chrome", "Safari"} {
		var prop string
		if browser == "Safari" {
			prop = "name of current tab of w"
		} else {
			prop = "title of t"
		}
		_ = prop // used in template below

		var script string
		if browser == "Safari" {
			script = fmt.Sprintf(`
tell application "Safari"
  repeat with w in windows
    repeat with t in tabs of w
      if name of t contains %q then
        set current tab of w to t
        activate
        return "found"
      end if
    end repeat
  end repeat
end tell
return "not found"`, instanceID)
		} else {
			script = fmt.Sprintf(`
tell application "Google Chrome"
  repeat with w in windows
    repeat with t in tabs of w
      if title of t contains %q then
        set active tab of w to t
        activate
        return "found"
      end if
    end repeat
  end repeat
end tell
return "not found"`, instanceID)
		}

		out, err := exec.Command("osascript", "-e", script).Output()
		if err == nil && strings.TrimSpace(string(out)) == "found" {
			return true
		}
	}
	return false
}
