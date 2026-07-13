package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/platform"
	"github.com/spore-host/spawn/pkg/plugin"
	"github.com/spore-host/spawn/pkg/sshkey"
	"gopkg.in/yaml.v3"
)

// LaunchConfig is the YAML structure for --config files passed to spawn launch.
type LaunchConfig struct {
	Plugins []plugin.Declaration `yaml:"plugins"`
}

// loadLaunchConfig reads a launch config YAML file.
func loadLaunchConfig(path string) (*LaunchConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read launch config %s: %w", path, err)
	}
	var cfg LaunchConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse launch config %s: %w", path, err)
	}
	return &cfg, nil
}

func isTerminal(f *os.File) bool {
	stat, err := f.Stat()
	if err != nil {
		return false
	}
	return (stat.Mode() & os.ModeCharDevice) != 0
}

// launchInputMode selects how `spawn launch` obtains its configuration.
type launchInputMode int

const (
	modeFlags  launchInputMode = iota // explicit flags (--instance-type set)
	modeWizard                        // interactive TTY wizard
	modePipe                          // truffle JSON piped on stdin
)

// launchMode decides the input mode from the --interactive flag, whether
// --instance-type was given, and whether stdin is a TTY.
//
//   - explicit --interactive, or no instance type on a TTY → wizard.
//   - no instance type and stdin is a pipe → pipe (consume truffle JSON).
//   - otherwise (instance type given) → flags, and stdin is NOT read.
//
// The last rule is the #34 fix: a caller that passes --instance-type with a
// piped, non-TTY stdin (e.g. a Java/ProcessBuilder subprocess) must use flags
// mode, not try to parse an empty stdin as JSON.
func launchMode(interactive bool, instanceType string, stdinIsTTY bool) launchInputMode {
	if interactive || (instanceType == "" && stdinIsTTY) {
		return modeWizard
	}
	if instanceType == "" && !stdinIsTTY {
		return modePipe
	}
	return modeFlags
}

func registerDNS(plat *platform.Platform, keyName, instanceID, publicIP, recordName, domain, apiEndpoint string) (string, error) {
	// Build SSH command to register DNS from within the instance
	sshScript := fmt.Sprintf(`
# Get IMDSv2 token
TOKEN=$(curl -X PUT "http://169.254.169.254/latest/api/token" -H "X-aws-ec2-metadata-token-ttl-seconds: 21600" -s 2>/dev/null)

# Get instance identity
IDENTITY_DOC=$(curl -H "X-aws-ec2-metadata-token: $TOKEN" -s http://169.254.169.254/latest/dynamic/instance-identity/document 2>/dev/null | base64 -w0)
IDENTITY_SIG=$(curl -H "X-aws-ec2-metadata-token: $TOKEN" -s http://169.254.169.254/latest/dynamic/instance-identity/signature 2>/dev/null | tr -d '\n')
PUBLIC_IP=$(curl -H "X-aws-ec2-metadata-token: $TOKEN" -s http://169.254.169.254/latest/meta-data/public-ipv4 2>/dev/null)

# Call DNS API. Capture the HTTP status and body separately so a failure
# surfaces the real reason (e.g. 403 from the IAM-auth'd updater when this
# account isn't authorized) instead of a generic "call failed".
HTTP_BODY=$(curl -s -w "\n%%{http_code}" -X POST %s \
  -H "Content-Type: application/json" \
  -d "{
    \"instance_identity_document\": \"$IDENTITY_DOC\",
    \"instance_identity_signature\": \"$IDENTITY_SIG\",
    \"record_name\": \"%s\",
    \"ip_address\": \"$PUBLIC_IP\",
    \"action\": \"UPSERT\"
  }" 2>&1)
CURL_RC=$?
HTTP_CODE=$(printf '%%s' "$HTTP_BODY" | tail -n1)
HTTP_JSON=$(printf '%%s' "$HTTP_BODY" | sed '$d')
if [ "$CURL_RC" -ne 0 ]; then
  printf '{"success":false,"error":"could not reach DNS API (curl exit %%s): %%s"}' "$CURL_RC" "$(printf '%%s' "$HTTP_BODY" | tr -d '\n\"' | cut -c1-200)"
elif [ "$HTTP_CODE" = "200" ] || [ "$HTTP_CODE" = "201" ]; then
  printf '%%s' "$HTTP_JSON"
else
  printf '{"success":false,"error":"DNS API returned HTTP %%s: %%s"}' "$HTTP_CODE" "$(printf '%%s' "$HTTP_JSON" | tr -d '\n\"' | cut -c1-200)"
fi
`, apiEndpoint, recordName)

	// Execute SSH command using the same keypair registered with EC2 (resolved
	// via the shared resolver: spawn-managed key first, then ~/.ssh fallback).
	sshKeyPath, err := sshkey.Resolve(plat.HomeDir, keyName)
	if err != nil {
		sshKeyPath = plat.SSHKeyPath // back-compat last resort
	}
	// SSH as the local-matching user the bootstrap created (the same
	// $LOCAL_USERNAME whose authorized_keys got the spawn public key). This is
	// spawn's design: the instance provisions a user mirroring the controller's
	// login, and `spawn connect` uses it — so DNS registration must use it too.
	username := plat.GetUsername()

	// Build SSH command arguments. ControlMaster=no / ControlPath=none ensure
	// spawn's own SSH never piggybacks the user's ~/.ssh/config connection
	// multiplexing — otherwise many concurrent launches/connects serialize on
	// one shared control socket (#56).
	sshArgs := []string{
		"-i", sshKeyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=10",
		"-o", "LogLevel=ERROR",
		"-o", "ControlMaster=no",
		"-o", "ControlPath=none",
		fmt.Sprintf("%s@%s", username, publicIP),
		sshScript,
	}

	// Execute
	cmd := exec.Command("ssh", sshArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to execute SSH command: %w (output: %s)", err, string(output))
	}

	// Parse response
	var response struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
		Message string `json:"message"`
		Record  string `json:"record"`
	}

	if err := json.Unmarshal([]byte(strings.TrimSpace(string(output))), &response); err != nil {
		return "", fmt.Errorf("failed to parse DNS API response: %w (output: %s)", err, string(output))
	}

	if !response.Success {
		return "", fmt.Errorf("%s", response.Error)
	}

	return response.Record, nil
}

// writeOutputID writes sweep/instance ID to file for workflow integration
func writeOutputID(id, filepath string) error {
	if filepath == "" {
		return nil
	}
	return os.WriteFile(filepath, []byte(id+"\n"), 0644)
}

// waitForSSHReady polls TCP port 22 until it accepts a connection or the
// deadline passes. This replaces a fixed sleep with an actual readiness probe:
// it returns the instant SSH is reachable and is bounded so it can't hang.
// Best-effort — a timeout is not fatal (the user can still connect later).
func waitForSSHReady(ctx context.Context, host string, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	addr := net.JoinHostPort(host, "22")
	for {
		conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = conn.Close()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// verifySporedReady polls `spored status` over SSM until the agent responds or
// the deadline passes (#50). spored is installed asynchronously by cloud-init, so
// this is the launch-time confirmation that it actually came up — a non-nil
// return means it never did within the window (failed install, etc.), which the
// caller treats as a launch failure (a spored-less instance has no TTL safety
// net). Uses SSM RunShellScript so it works for both keyed and keyless instances;
// an SSM error early on (agent still registering) is retried, not fatal.
func verifySporedReady(ctx context.Context, client *aws.Client, region, instanceID string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// FIRST wait for the SSM agent to REGISTER (PingStatus=Online) before sending
	// any command (#277). On a fresh AL2023/Graviton spot instance the agent can
	// take a while to come up; sending `spored status` before then means every
	// SendCommand fails until the whole gate times out with an opaque "context
	// deadline exceeded". WaitForSSMOnline polls DescribeInstanceInformation and,
	// crucially, fails FAST if the instance has no IAM instance profile (the agent
	// can then never register) rather than waiting out the timeout. Give it the
	// bulk of the budget; reserve time for the status poll below.
	onlineTimeout := timeout - 30*time.Second
	if onlineTimeout < 30*time.Second {
		onlineTimeout = timeout / 2
	}
	if err := client.WaitForSSMOnline(ctx, region, instanceID, onlineTimeout); err != nil {
		return fmt.Errorf("SSM agent never came online (can't verify spored): %w", err)
	}

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	var lastErr error
	for {
		// `spored status` exits 0 when the daemon is up and answering. Run it over
		// SSM (the agent runs as root, so no sudo needed). The agent is Online by
		// now, so SendCommand reaches it instead of timing out.
		res, err := client.RunShellScript(ctx, region, instanceID, "/usr/local/bin/spored status", 30*time.Second)
		if err == nil && res.Status == "Success" {
			return nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("spored status: %s: %s", res.Status, strings.TrimSpace(res.Stderr))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("spored did not become ready within %s: %w", timeout, lastErr)
		case <-ticker.C:
		}
	}
}
