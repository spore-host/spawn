package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
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

// shouldAttemptDNSRegistration decides whether the launch flow should even
// try to register DNS (#549). It's a pure function so the guard's decision
// table is directly unit-testable without running a real launch:
//
//   - no DNS name requested (--dns / --name never set one) → never.
//   - SSH readiness wasn't confirmed (--wait-for-ssh=false, #56) → never;
//     that path has its own distinct skip message and is checked by the
//     caller before this function is even reached.
//   - dnsConfig.IsEnabled() is false (--no-dns, or dns.enabled: false in
//     ~/.spawn/config.yaml, with CLI flag taking precedence per
//     DNSConfig.IsEnabled/LoadDNSConfig) → never.
//   - otherwise → yes.
func shouldAttemptDNSRegistration(dnsName string, waitForSSH bool, dnsConfig *spawnconfig.DNSConfig) bool {
	if dnsName == "" || !waitForSSH || dnsConfig == nil {
		return false
	}
	return dnsConfig.IsEnabled()
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

// resolveHost is the DNS lookup registerDNS's controller-side pre-check uses
// (#548). Tests substitute a fake so a permanently-unresolvable-hostname case
// can be exercised without touching the network or waiting on real DNS.
var resolveHost = func(host string) ([]string, error) {
	return net.LookupHost(host)
}

// isPermanentResolutionFailure reports whether err represents a definitive
// "this name does not exist" (NXDOMAIN-class) failure, as opposed to a
// transient resolver hiccup (timeout, temporarily unreachable server, no
// network yet) that might well succeed on a later attempt. Go's net.DNSError
// carries this distinction directly via IsNotFound (added for exactly this
// purpose) — IsTimeout/IsTemporary and everything else are NOT permanent.
// This split is the crux of #548: treating every resolution failure as
// permanent would break genuinely-transient early-boot cases (resolver not
// populated yet), while treating every failure as transient (today's bug)
// burns the full retry deadline on names that can never resolve.
func isPermanentResolutionFailure(err error) bool {
	if err == nil {
		return false
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return dnsErr.IsNotFound
	}
	return false
}

// endpointHostname extracts the hostname portion of a DNS API endpoint URL.
// Returns "" (not an error) for a malformed/hostless endpoint — callers treat
// that as "can't pre-check this one, fall back to the normal retry loop".
func endpointHostname(apiEndpoint string) string {
	u, err := url.Parse(apiEndpoint)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// preflightDNSEndpoint is the #548 controller-side pre-check: it resolves the
// DNS API endpoint's hostname BEFORE registerDNS commits the guest to a retry
// loop that can burn up to 4 minutes (real instance-hours) of an SSH exec.
// DNS is DNS: a hostname that's NXDOMAIN here (a stale/placeholder
// dns.api_endpoint, or an account never wired for spore.host DNS — which
// cmd/launch_single.go's caller comment calls the *expected* case for
// unauthorized accounts) will never resolve on the guest instance either, so
// there's nothing to gain from waiting it out server-side.
//
// Returns a non-nil error ONLY for a definitive NXDOMAIN
// (isPermanentResolutionFailure) — the caller should skip the retry loop and
// surface this as the non-fatal warning. Any other outcome (resolves fine, a
// malformed/hostless endpoint, or a transient lookup error such as a timeout
// or unreachable resolver) returns nil: the normal SSH retry loop must still
// run, because THAT loop's whole reason to exist is the guest's own DNS
// resolver taking up to ~90s to populate after boot — a transient failure on
// the controller says nothing about that and must not short-circuit it.
func preflightDNSEndpoint(apiEndpoint string) error {
	host := endpointHostname(apiEndpoint)
	if host == "" {
		return nil
	}
	if _, err := resolveHost(host); isPermanentResolutionFailure(err) {
		return fmt.Errorf("DNS API endpoint hostname %q does not resolve (%w) — this is expected in accounts not wired for spore.host DNS", host, err)
	}
	return nil
}

func registerDNS(plat *platform.Platform, keyName, instanceID, publicIP, recordName, domain, apiEndpoint string) (string, error) {
	// #548: skip the guest-side SSH retry loop entirely when the endpoint is
	// permanently unresolvable — see preflightDNSEndpoint's doc comment for
	// why only a definitive NXDOMAIN short-circuits, not a transient failure.
	if err := preflightDNSEndpoint(apiEndpoint); err != nil {
		return "", err
	}

	// Build SSH command to register DNS from within the instance
	sshScript := fmt.Sprintf(`
# Get IMDSv2 token
TOKEN=$(curl -X PUT "http://169.254.169.254/latest/api/token" -H "X-aws-ec2-metadata-token-ttl-seconds: 21600" -s 2>/dev/null)

# Get instance identity
IDENTITY_DOC=$(curl -H "X-aws-ec2-metadata-token: $TOKEN" -s http://169.254.169.254/latest/dynamic/instance-identity/document 2>/dev/null | base64 -w0)
IDENTITY_SIG=$(curl -H "X-aws-ec2-metadata-token: $TOKEN" -s http://169.254.169.254/latest/dynamic/instance-identity/signature 2>/dev/null | tr -d '\n')
PUBLIC_IP=$(curl -H "X-aws-ec2-metadata-token: $TOKEN" -s http://169.254.169.254/latest/meta-data/public-ipv4 2>/dev/null)

# Call DNS API. Force IPv4 (-4): the Lambda URL is dual-stack (A + AAAA) but
# IPv4-only instances have no IPv6 route. Capture the HTTP status and body
# separately so a failure surfaces the real reason (e.g. 403 from the IAM-auth'd
# updater when this account isn't authorized) instead of a generic "call failed".
# Session-level readiness (user account created, DNS resolver populated) is
# handled by the caller retrying the whole SSH exec, so a single attempt here.
HTTP_BODY=$(curl -4 -s -w "\n%%{http_code}" -X POST %s \
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

	// Execute, retrying the WHOLE SSH exec (a fresh session each attempt). The
	// shallow port-22 readiness check the launch flow uses returns before
	// cloud-init has finished — so at first the local user's authorized_keys may
	// not exist yet (SSH Permission denied) and the instance's DNS resolver may
	// not yet resolve public names (curl exit 6). Both are session-level:
	// retrying inside one early session can't recover, but re-establishing a new
	// session does. The early-boot DNS window on a fresh instance is variable and
	// has been observed past 90s, so retry up to a few minutes; DNS registration
	// is non-fatal, so this is a bounded best-effort wait, not a launch blocker.
	var output []byte
	deadline := time.Now().Add(4 * time.Minute)
	for {
		cmd := exec.Command("ssh", sshArgs...)
		output, err = cmd.CombinedOutput()
		transient := err != nil || bytes.Contains(output, []byte("curl exit 6")) ||
			bytes.Contains(output, []byte("curl exit 7")) || bytes.Contains(output, []byte("curl exit 28"))
		if !transient || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Second)
	}
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
