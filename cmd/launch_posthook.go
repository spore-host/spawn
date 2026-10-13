package cmd

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
	"github.com/spore-host/spawn/pkg/plugin"
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

// dnsRegistrationEnabled resolves --no-dns and dns.enabled in
// ~/.spawn/config.yaml (#549), the flag winning. A config file that cannot be
// read leaves DNS on, as it was before either control existed.
func dnsRegistrationEnabled() bool {
	cfg, err := spawnconfig.LoadDNSConfig(noDNS)
	if err != nil {
		return !noDNS
	}
	return cfg.IsEnabled()
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
//
// A timeout returns a non-nil error and is NOT fatal — the instance is up and
// spored is enforcing its lifecycle — but the caller must know, because the
// steps after this one (DNS registration, #741) reach the instance over SSH and
// cannot work if this failed.
//
// It used to return nothing at all, which made the timeout path and the success
// path the same statement and the failure literally unrepresentable (#740). The
// caller then marked the step complete unconditionally, so an unreachable
// instance printed `✅ Waiting for SSH (120.0s)` — the timeout, to the tenth of
// a second, rendered as success — and the launch went on to SSH to a host it had
// just failed to reach, for another four minutes, in silence.
func waitForSSHReady(ctx context.Context, host string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	addr := net.JoinHostPort(host, "22")
	var lastErr error
	for {
		conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			// The dial error is the useful half ("connection refused" means
			// something is listening on the host but not on 22; a timeout means
			// nothing answered at all), so carry it rather than only the deadline.
			return fmt.Errorf("SSH (port 22) on %s did not become reachable within %s: %w",
				host, timeout, lastErr)
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
// sporedCheck is how a launch confirms spored came up on the instance.
type sporedCheck int

const (
	sporedCheckNone    sporedCheck = iota // not verified (--wait-for-ssh=false)
	sporedCheckLinux                      // `spored status` via AWS-RunShellScript
	sporedCheckWindows                    // the spored service via AWS-RunPowerShellScript
)

// sporedVerificationFor decides how — and whether — to verify spored after a
// launch. It is a function of the INSTANCE's OS and nothing else (#882).
//
// It used to be decided inline as `waitForSSH && plat.OS != "windows"`, where
// plat is platform.Detect() and plat.OS is runtime.GOOS: the OS of the machine
// RUNNING spawn. So the instance's verification depended on the user's laptop.
// From macOS, a Windows instance got the Linux check — SSM refused it with
// UnsupportedPlatformType and the launch reported "spored never came up" about a
// spore that was healthy (observed, i-061da267b7a9442de); with --terminate-on-error
// that terminates it. From a Windows workstation, a Linux instance got no check
// at all. The workstation's OS is deliberately not a parameter here, so it cannot
// creep back in.
func sporedVerificationFor(waitForSSH bool, targetOS string) sporedCheck {
	if !waitForSSH {
		return sporedCheckNone
	}
	if strings.EqualFold(targetOS, "windows") {
		return sporedCheckWindows
	}
	return sporedCheckLinux
}

// sporedVerifyTimeout is the budget for a check. Windows gets longer: its first
// boot also installs the AWS CLI from an MSI before spored can be fetched.
func sporedVerifyTimeout(c sporedCheck) time.Duration {
	if c == sporedCheckWindows {
		return 10 * time.Minute
	}
	return 5 * time.Minute
}

// windowsSporedRunning exits 0 only if the spored service exists and is Running.
// Asks the Service Control Manager rather than running spored.exe, because a
// verified-install refusal (#879) leaves no spored.exe to run — and "no binary"
// must read as not-ready, not as an execution error.
const windowsSporedRunning = `$s = Get-Service spored -ErrorAction SilentlyContinue; if ($s -and $s.Status -eq 'Running') { exit 0 } else { Write-Output ('spored service: ' + $(if ($s) { $s.Status } else { 'not installed' })); exit 1 }`

// sporedProbe returns the probe for a check: whether it runs as PowerShell
// (AWS-RunPowerShellScript) or shell (AWS-RunShellScript), and the command. Pure,
// so the mapping that sent a Linux document to a Windows instance is testable.
func sporedProbe(check sporedCheck) (powershell bool, command string) {
	if check == sporedCheckWindows {
		return true, windowsSporedRunning
	}
	return false, "/usr/local/bin/spored status"
}

// verifySporedReady confirms spored is up on a Linux instance. Kept with its
// original signature for its other callers; launches use verifySporedReadyOn.
func verifySporedReady(ctx context.Context, client *aws.Client, region, instanceID string, timeout time.Duration) error {
	return verifySporedReadyOn(ctx, client, region, instanceID, sporedCheckLinux, timeout)
}

// verifySporedReadyOn confirms spored is up, probing with the check that matches
// the instance's OS.
func verifySporedReadyOn(ctx context.Context, client *aws.Client, region, instanceID string, check sporedCheck, timeout time.Duration) error {
	powershell, command := sporedProbe(check)
	probe := func(ctx context.Context) (*aws.SSMRunResult, error) {
		if powershell {
			return client.RunPowerShell(ctx, region, instanceID, command, 30*time.Second)
		}
		return client.RunShellScript(ctx, region, instanceID, command, 30*time.Second)
	}

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
		res, err := probe(ctx)
		if err == nil && res.Status == "Success" {
			return nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("spored status: %s: %s", res.Status,
				strings.TrimSpace(strings.TrimSpace(res.Stdout)+" "+strings.TrimSpace(res.Stderr)))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("spored did not become ready within %s: %w", timeout, lastErr)
		case <-ticker.C:
		}
	}
}
