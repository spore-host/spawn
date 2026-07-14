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
	connectRDP        bool
	connectViaSSM     bool
	connectRDPPort    int
	connectSSH        bool
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
	connectCmd.Flags().BoolVar(&connectRDP, "rdp", false, "Windows: open a Remote Desktop (RDP) connection (decrypts the Administrator password)")
	connectCmd.Flags().BoolVar(&connectSSH, "ssh", false, "Windows: SSH in (as Administrator, over OpenSSH) instead of opening a PowerShell-over-SSM session — same SSH path as Linux")
	connectCmd.Flags().BoolVar(&connectViaSSM, "via-ssm", false, "Windows --rdp: tunnel RDP over an SSM port-forwarding session instead of connecting to the public IP")
	connectCmd.Flags().IntVar(&connectRDPPort, "rdp-port", 13389, "Windows --rdp --via-ssm: local port for the SSM RDP tunnel")

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

	// Windows: no SSH-user/key model — fetch+decrypt the Administrator password,
	// print RDP instructions, then open an SSM session (or run a one-shot command
	// via SSM). Triggered by the spawn:os tag written at launch.
	if instance.Tags["spawn:os"] == "windows" {
		return connectWindows(ctx, client, instance, args[1:])
	}

	// Use Session Manager if requested or if no public IP
	if connectSessionMgr || instance.PublicIP == "" {
		return connectViaSessionManager(instance.InstanceID, instance.Region)
	}

	// Determine SSH user: prefer the instance's local-matching user (the
	// spawn:local-username the bootstrap created and installed the key for — "log
	// in as you"), falling back to ec2-user for instances launched before that
	// tag existed. --user overrides.
	user := connectUser
	if user == "" {
		user = instance.Tags["spawn:local-username"]
	}
	if user == "" {
		user = "ec2-user" // older instances / no local-username tag
	}

	// Determine SSH key
	keyPath := connectKey
	if keyPath == "" {
		// Try to find the key based on the instance key name
		keyPath, err = findSSHKey(instance.KeyName)
		if err != nil {
			// We don't hold the instance's launch key (or it has none — the
			// keyless case for instances launched headlessly by lagotto/cohort,
			// where the launcher had no SSH key on disk). Rather than drop
			// straight to Session Manager, try to inject spawn's managed public
			// key over SSM, then SSH with the matching private key. If injection
			// isn't possible (no SSM, no managed key), fall back to SSM shell.
			if injectedKey, ierr := injectSSHKeyViaSSM(ctx, client, instance, user); ierr == nil {
				keyPath = injectedKey
			} else {
				fmt.Fprintf(os.Stderr, "%s: %s: %v\n", i18n.Symbol("warning"), i18n.Tf("spawn.connect.key_not_found", map[string]interface{}{
					"KeyName": instance.KeyName,
				}), err)
				fmt.Fprintf(os.Stderr, "   key injection over SSM unavailable: %v\n", ierr)
				fmt.Fprintf(os.Stderr, "%s\n\n", i18n.T("spawn.connect.fallback_session_manager"))
				return connectViaSessionManager(instance.InstanceID, instance.Region)
			}
		}
	}

	// One-shot mode: args[1:] (after --) form the remote command, wrapped in
	// `bash -c` (Linux default shell).
	var remoteCmd string
	if len(args) > 1 {
		remoteCmd = "bash -c '" + strings.ReplaceAll(strings.Join(args[1:], " "), "'", "'\\''") + "'"
	}

	return sshToInstance(user, instance.PublicIP, keyPath, connectPort, remoteCmd)
}

// sshToInstance runs the ssh client against host as user with keyPath, on the
// given port. If remoteCmd is non-empty it's passed as the one-shot command;
// otherwise ssh opens an interactive session. ControlMaster=no / ControlPath=none
// keep spawn's SSH independent of the user's ~/.ssh/config connection
// multiplexing, so many concurrent `spawn connect` calls don't serialize on one
// shared control socket (#56). Shared by the Linux and Windows (--ssh) paths.
func sshToInstance(user, host, keyPath string, port int, remoteCmd string) error {
	sshArgs := buildSSHCommandArgs(user, host, keyPath, port, remoteCmd)

	fmt.Fprintf(os.Stderr, "%s\n\n", i18n.Tf("spawn.connect.connecting_ssh", map[string]interface{}{
		"Command": "ssh " + strings.Join(sshArgs, " "),
	}))

	sshCmd := exec.Command("ssh", sshArgs...)
	sshCmd.Stdin = os.Stdin
	sshCmd.Stdout = os.Stdout
	sshCmd.Stderr = os.Stderr
	return sshCmd.Run()
}

// buildSSHCommandArgs constructs the ssh argument vector. remoteCmd, if
// non-empty, is appended verbatim as the one-shot command (the caller is
// responsible for any shell wrapping — bash -c on Linux, none on Windows where
// the remote shell is PowerShell). Pure/testable; no exec.
func buildSSHCommandArgs(user, host, keyPath string, port int, remoteCmd string) []string {
	args := []string{
		"-i", keyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ControlMaster=no",
		"-o", "ControlPath=none",
		"-p", fmt.Sprintf("%d", port),
		fmt.Sprintf("%s@%s", user, host),
	}
	if remoteCmd != "" {
		args = append(args, remoteCmd)
	}
	return args
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

// connectWindows handles `spawn connect` for Windows instances. There is no
// spored/SSH-user model on Windows yet (#77): the credential is the Administrator
// password (decrypted from GetPasswordData with the RSA launch key), and the
// transport is SSM. It prints RDP instructions + the password, then either opens
// an interactive SSM PowerShell session or, for a one-shot `connect -- <cmd>`,
// runs the command via SSM RunCommand.
func connectWindows(ctx context.Context, client *aws.Client, instance *aws.InstanceInfo, command []string) error {
	region := instance.Region
	id := instance.InstanceID

	// --ssh: SSH straight in, the same way Linux connect does. The imported
	// Windows AMI runs OpenSSH and trusts spawn's launch key for the
	// Administrator account (see buildWindowsUserData), so this is plain ssh to
	// the public IP — no SSM, no plugin. The remote shell is PowerShell (set as
	// the OpenSSH DefaultShell), so a one-shot command runs as PowerShell.
	if connectSSH {
		if instance.PublicIP == "" {
			return fmt.Errorf("--ssh needs a public IP; this instance has none (use plain `spawn connect %s` for a PowerShell-over-SSM session, or `--rdp --via-ssm`)", instance.Name)
		}
		user := connectUser
		if user == "" {
			user = "Administrator" // the account the Windows bootstrap trusts the key for
		}
		keyPath := connectKey
		if keyPath == "" {
			k, kerr := findSSHKey(instance.KeyName)
			if kerr != nil {
				return fmt.Errorf("--ssh needs the launch key %q to authenticate, and it isn't on this machine: %w", instance.KeyName, kerr)
			}
			keyPath = k
		}
		// The remote command (if any) runs in PowerShell — no bash -c wrap.
		return sshToInstance(user, instance.PublicIP, keyPath, connectPort, strings.Join(command, " "))
	}

	// One-shot command mode → SSM RunCommand (PowerShell).
	if len(command) > 0 {
		ps := strings.Join(command, " ")
		fmt.Fprintf(os.Stderr, "Running on %s via SSM (PowerShell)...\n", instance.Name)
		res, err := client.RunPowerShell(ctx, region, id, ps, 5*time.Minute)
		if err != nil {
			return fmt.Errorf("ssm run-command: %w", err)
		}
		if res.Stdout != "" {
			fmt.Print(res.Stdout)
		}
		if res.Stderr != "" {
			fmt.Fprint(os.Stderr, res.Stderr)
		}
		if res.Status != "Success" {
			return fmt.Errorf("remote command status: %s", res.Status)
		}
		return nil
	}

	// --rdp: open a Remote Desktop connection (direct to the public IP, or over
	// an SSM port-forward tunnel). Decrypts the Administrator password first.
	if connectRDP {
		return connectWindowsRDP(ctx, client, instance)
	}

	// Interactive: resolve the Administrator password (SSM-set, or GetPasswordData
	// fallback), print RDP info, then drop into an SSM PowerShell session.
	password, perr := resolveWindowsAdminPassword(ctx, client, instance)
	if perr != nil {
		fmt.Fprintf(os.Stderr, "%s %v\n", i18n.Symbol("warning"), perr)
	}
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Printf("  Windows instance %s\n", instance.Name)
	fmt.Printf("  Administrator user:  Administrator\n")
	if password != "" {
		fmt.Printf("  Administrator pass:  %s\n", password)
	}
	if instance.PublicIP != "" {
		fmt.Printf("  RDP:                 mstsc /v:%s   (or any RDP client → %s:3389)\n", instance.PublicIP, instance.PublicIP)
	} else {
		fmt.Printf("  RDP:                 no public IP — use SSM port forwarding:\n")
		fmt.Printf("                       aws ssm start-session --target %s --region %s \\\n", id, region)
		fmt.Printf("                         --document-name AWS-StartPortForwardingSession \\\n")
		fmt.Printf("                         --parameters portNumber=3389,localPortNumber=13389\n")
	}
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	fmt.Fprintf(os.Stderr, "Opening an SSM PowerShell session (Ctrl-D to exit)...\n\n")
	return connectViaSessionManager(id, region)
}

// resolveWindowsAdminPassword obtains a usable Administrator password for a
// Windows instance, preferring spawn's SSM-set path and falling back to EC2's
// GetPasswordData (#201).
//
// Why SSM-first: EC2Launch's `setAdminAccount` generates the GetPasswordData-
// retrievable password only on the first boot after a Sysprep, then disables it.
// An instance launched from a warm AMI (re-imaged, never re-Sysprepped — #98)
// therefore never produces a retrievable password, so GetPasswordData times out
// (the #201 field bug). Rather than depend on that, spawn owns the credential: if
// the SSM agent is Online it sets a fresh random password directly. This works
// uniformly on warm and base AMIs.
//
// The fallback to GetPasswordData (RSA-decrypted with the launch key) covers the
// case where SSM isn't reachable (no instance profile, agent not up) but the
// instance did generate a retrievable password — i.e. a base AMI without SSM.
func resolveWindowsAdminPassword(ctx context.Context, client *aws.Client, instance *aws.InstanceInfo) (string, error) {
	region, id := instance.Region, instance.InstanceID

	// Prefer SSM: wait a bounded time for the agent to come Online, then set the
	// password. WaitForSSMOnline fails fast (ErrSSMUnreachable) when there's no
	// instance profile, so a base AMI without SSM falls through to GetPasswordData
	// quickly rather than burning the whole timeout.
	fmt.Fprintf(os.Stderr, "Waiting for SSM to come online to set the Administrator password...\n")
	if err := client.WaitForSSMOnlineProgress(ctx, region, id, 12*time.Minute, func(elapsed time.Duration) {
		fmt.Fprintf(os.Stderr, "  still waiting for SSM (%s)...\n", elapsed.Round(time.Second))
	}); err != nil {
		fmt.Fprintf(os.Stderr, "%s SSM not available (%v); falling back to EC2 GetPasswordData.\n", i18n.Symbol("warning"), err)
		return windowsPasswordViaGetPasswordData(ctx, client, instance)
	}

	fmt.Fprintf(os.Stderr, "Setting a fresh Administrator password over SSM...\n")
	pw, err := client.SetWindowsAdminPasswordViaSSM(ctx, region, id, 5*time.Minute)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s could not set the password over SSM (%v); falling back to EC2 GetPasswordData.\n", i18n.Symbol("warning"), err)
		return windowsPasswordViaGetPasswordData(ctx, client, instance)
	}
	return pw, nil
}

// windowsPasswordViaGetPasswordData is the legacy path: wait for EC2 to publish
// the encrypted password blob, then RSA-decrypt it with the launch key. Used when
// SSM is unavailable (#201 fallback).
func windowsPasswordViaGetPasswordData(ctx context.Context, client *aws.Client, instance *aws.InstanceInfo) (string, error) {
	region, id := instance.Region, instance.InstanceID
	fmt.Fprintf(os.Stderr, "Retrieving Windows Administrator password (this can take a few minutes after launch)...\n")
	blob, err := client.WaitForPasswordData(ctx, region, id, 12*time.Minute)
	if err != nil {
		return "", fmt.Errorf("could not get Windows password data (instance may still be on first boot): %w", err)
	}
	keyPath, rerr := findSSHKey(instance.KeyName)
	if rerr != nil {
		return "", fmt.Errorf("could not locate the launch key %q to decrypt the password: %w", instance.KeyName, rerr)
	}
	password, derr := aws.DecryptWindowsPassword(blob, keyPath)
	if derr != nil {
		return "", fmt.Errorf("could not decrypt the Administrator password (need the RSA launch key): %w", derr)
	}
	return password, nil
}

// connectWindowsRDP opens a Remote Desktop connection to a Windows instance. It
// resolves the Administrator password (SSM-set, or GetPasswordData fallback —
// resolveWindowsAdminPassword), then connects either directly to the public IP
// (default when one exists) or over an SSM port-forwarding tunnel (--via-ssm, or
// automatically when there's no public IP). The password is printed for the user
// to paste into the RDP login (RDP can't be pre-seeded cross-platform). (#95/#201)
func connectWindowsRDP(ctx context.Context, client *aws.Client, instance *aws.InstanceInfo) error {
	region, id := instance.Region, instance.InstanceID

	password, err := resolveWindowsAdminPassword(ctx, client, instance)
	if err != nil {
		return err
	}

	// Decide transport: explicit --via-ssm, or no public IP → tunnel.
	useSSM := connectViaSSM || instance.PublicIP == ""

	host := instance.PublicIP
	if useSSM {
		host = fmt.Sprintf("localhost:%d", connectRDPPort)
		fmt.Fprintf(os.Stderr, "Opening an SSM port-forward to RDP (3389) on local port %d...\n", connectRDPPort)
		fmt.Fprintf(os.Stderr, "Leave this running; it forwards %s → %s:3389.\n", host, id)
		// Start the tunnel in the background and open the RDP client against it.
		go func() { _ = startRDPTunnel(id, region, connectRDPPort) }()
		time.Sleep(3 * time.Second) // give the session a moment to establish
	}

	printRDPCredentials(instance.Name, host, password)
	return launchRDPClient(host)
}

// startRDPTunnel shells out to the AWS CLI to open an SSM port-forwarding
// session from localPort to the instance's RDP port (3389). Same AWS-CLI +
// session-manager-plugin dependency as connectViaSessionManager.
func startRDPTunnel(instanceID, region string, localPort int) error {
	if _, err := exec.LookPath("aws"); err != nil {
		return i18n.Te("spawn.connect.error.aws_cli_not_found", nil)
	}
	cmd := exec.Command("aws", "ssm", "start-session",
		"--target", instanceID,
		"--region", region,
		"--document-name", "AWS-StartPortForwardingSession",
		"--parameters", fmt.Sprintf("portNumber=3389,localPortNumber=%d", localPort),
	)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stderr, os.Stderr
	return cmd.Run()
}

// printRDPCredentials shows the connection target + Administrator password.
func printRDPCredentials(name, host, password string) {
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Printf("  Windows instance %s — Remote Desktop\n", name)
	fmt.Printf("  Host:                %s\n", host)
	fmt.Printf("  User:                Administrator\n")
	fmt.Printf("  Password:            %s\n", password)
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()
}

// rdpClientCommand returns the OS-appropriate command+args to open an RDP client
// for the given host ("ip" or "localhost:port"). Pure/testable: no execution.
func rdpClientCommand(goos, host string) (string, []string) {
	switch goos {
	case "windows":
		// mstsc takes /v:host[:port]
		return "mstsc", []string{"/v:" + host}
	case "darwin":
		// Microsoft Remote Desktop registers the rdp:// URL scheme; `open` hands off.
		return "open", []string{"rdp://full%20address=s:" + host}
	default:
		// Linux: prefer xfreerdp if present (caller falls back to instructions).
		return "xfreerdp", []string{"/v:" + host}
	}
}

// launchRDPClient opens the platform RDP client for host; if it can't, it prints
// manual instructions rather than failing (the credentials are already shown).
func launchRDPClient(host string) error {
	bin, args := rdpClientCommand(runtime.GOOS, host)
	if _, err := exec.LookPath(bin); err != nil {
		fmt.Fprintf(os.Stderr, "Could not find an RDP client (%s). Connect manually to %s as Administrator with the password above.\n", bin, host)
		return nil
	}
	fmt.Fprintf(os.Stderr, "Launching RDP client (%s) → %s ...\n", bin, host)
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command -- bin/args come from rdpClientCommand, which returns hardcoded client names (xfreerdp/open/mstsc) plus the host; exec.Command runs no shell, so there is no command injection.
	cmd := exec.Command(bin, args...)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Could not launch %s: %v. Connect manually to %s.\n", bin, err, host)
	}
	return nil
}

// findSSHKey resolves a usable private key for an EC2 key name. It delegates to
// sshkey.Resolve — the single resolver shared by connect, status, and queue —
// which checks spawn-managed keys (~/.spawn/keys) first, then falls back to the
// user's ~/.ssh keys for back-compat.
// injectSSHKeyViaSSM ensures spawn's managed keypair exists locally and appends
// its public key to the instance's authorized_keys over SSM RunShellScript, so
// `spawn connect` can SSH into an instance whose launch key we don't hold —
// including keyless instances launched headlessly by lagotto/cohort. It returns
// the local private key path to use with ssh. Errors (no SSM agent, no SSM
// permissions, command failure) are returned so the caller can fall back to a
// plain Session Manager shell.
func injectSSHKeyViaSSM(ctx context.Context, client *aws.Client, instance *aws.InstanceInfo, user string) (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	// Use spawn's managed ED25519 key (created on demand, idempotent). The user
	// here is the SSH login user, which is also how spawn names its key locally.
	kp, err := sshkey.EnsureKey(homeDir, user, sshkey.ED25519)
	if err != nil {
		return "", fmt.Errorf("ensure managed key: %w", err)
	}
	pub, err := os.ReadFile(kp.PublicKeyPath)
	if err != nil {
		return "", fmt.Errorf("read managed public key: %w", err)
	}
	pubLine := strings.TrimSpace(string(pub))
	if pubLine == "" {
		return "", fmt.Errorf("managed public key is empty")
	}

	script, err := authorizedKeyInjectionScript(user, pubLine)
	if err != nil {
		return "", err
	}

	fmt.Fprintf(os.Stderr, "%s no local key for this instance — injecting spawn's key over SSM...\n", i18n.Symbol("info"))
	res, err := client.RunShellScript(ctx, instance.Region, instance.InstanceID, script, 90*time.Second)
	if err != nil {
		return "", err
	}
	if res.Status != "Success" {
		return "", fmt.Errorf("key injection command %s: %s", res.Status, strings.TrimSpace(res.Stderr))
	}
	return kp.PrivateKeyPath, nil
}

// authorizedKeyInjectionScript builds the shell script that appends pubLine to
// the login user's authorized_keys, idempotently and with correct ownership and
// permissions. Pulled out as a pure function so the (security-sensitive) script
// can be unit-tested without an SSM round-trip. pubLine must be a single SSH
// public-key line; it's rejected if it contains a single quote (which the key
// alphabet never does) so it can't break out of the single-quoted literal.
func authorizedKeyInjectionScript(user, pubLine string) (string, error) {
	if strings.ContainsAny(user, "'\"$`\\ \t\n") {
		return "", fmt.Errorf("unsafe ssh user %q", user)
	}
	if strings.ContainsAny(pubLine, "'\n\r") {
		return "", fmt.Errorf("unsafe public key content")
	}
	return fmt.Sprintf(`set -e
u=%q
home=$(getent passwd "$u" | cut -d: -f6)
if [ -z "$home" ]; then echo "no home for user $u" >&2; exit 1; fi
mkdir -p "$home/.ssh"
chmod 700 "$home/.ssh"
touch "$home/.ssh/authorized_keys"
chmod 600 "$home/.ssh/authorized_keys"
key='%s'
grep -qF "$key" "$home/.ssh/authorized_keys" || echo "$key" >> "$home/.ssh/authorized_keys"
chown -R "$u":"$u" "$home/.ssh"
`, user, pubLine), nil
}

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
