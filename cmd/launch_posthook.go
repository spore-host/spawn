package cmd

import (
	"context"
	"errors"
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

// verifySporedReadyOn confirms spored came up on a freshly launched instance
// (#50). spored is installed asynchronously by the bootstrap, so a failed install
// (bad download, signature or checksum mismatch, a bucket-less region) is
// otherwise invisible to `spawn launch` — leaving a running instance with no TTL,
// idle or cost enforcement. A non-nil return means it is not known to be up.
//
// The evidence is spored's own heartbeat tag (#1053): spored stamps
// spawn:last-heartbeat on every tick, on Linux and Windows alike, so its
// appearance proves spored is running without asking anything else on the
// instance. It is read with ec2:DescribeInstances, which the IAM baseline already
// grants. The SSM agent used to be the only channel, so an AMI that does not ship
// amazon-ssm-agent (Rocky, RHEL) failed every launch with "spored never came
// up" about a healthy spored. SSM is still consulted, as a second signal that
// may answer first, but its absence or refusal is never the verdict.
func verifySporedReadyOn(ctx context.Context, client *aws.Client, region, instanceID string, check sporedCheck, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return verifySporedWith(ctx, clientSporedSignals{client, region, instanceID, check}, timeout, realVerifyClock())
}

// sporedSignals is what verification may ask about an instance. It is the seam
// the tests drive: each method is one AWS round trip.
type sporedSignals interface {
	// liveness is one ec2:DescribeInstances: heartbeat tag, profile, state.
	liveness(ctx context.Context) (aws.SporedLiveness, error)
	// ssmOnline is one ssm:DescribeInstanceInformation.
	ssmOnline(ctx context.Context) (bool, error)
	// probe runs the OS's spored check over SSM (SendCommand + GetCommandInvocation).
	probe(ctx context.Context) (*aws.SSMRunResult, error)
}

type clientSporedSignals struct {
	client     *aws.Client
	region, id string
	check      sporedCheck
}

func (s clientSporedSignals) liveness(ctx context.Context) (aws.SporedLiveness, error) {
	return s.client.DescribeSporedLiveness(ctx, s.region, s.id)
}

func (s clientSporedSignals) ssmOnline(ctx context.Context) (bool, error) {
	return s.client.SSMOnline(ctx, s.region, s.id)
}

func (s clientSporedSignals) probe(ctx context.Context) (*aws.SSMRunResult, error) {
	powershell, command := sporedProbe(s.check)
	if powershell {
		return s.client.RunPowerShell(ctx, s.region, s.id, command, 30*time.Second)
	}
	return s.client.RunShellScript(ctx, s.region, s.id, command, 30*time.Second)
}

// verifyClock is the time verification runs on, injectable so tests count polls
// instead of measuring wall-clock time.
type verifyClock struct {
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

func realVerifyClock() verifyClock {
	return verifyClock{
		now: time.Now,
		sleep: func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		},
	}
}

// sporedVerifyPollInterval is the gap between polls. spored writes its first
// heartbeat about a minute after it starts, so polling faster buys nothing.
const sporedVerifyPollInterval = 10 * time.Second

// sporedUnverifiableError means verification could not be carried out — the
// caller's credentials refused the call that reads the evidence. It says nothing
// about the instance, so it must never be reported as "spored never came up"
// (#1126).
type sporedUnverifiableError struct{ err error }

func (e *sporedUnverifiableError) Error() string {
	return "can't verify spored: " + e.err.Error() + "; the instance itself may be fine"
}

func (e *sporedUnverifiableError) Unwrap() error { return e.err }

// sporedTerminalStates are states from which spored can no longer report.
var sporedTerminalStates = map[string]bool{
	"shutting-down": true, "terminated": true, "stopping": true, "stopped": true,
}

// verifySporedWith polls the heartbeat tag (and, while it is usable, SSM) until
// one of them shows spored up, or the budget runs out.
func verifySporedWith(ctx context.Context, sig sporedSignals, timeout time.Duration, clk verifyClock) error {
	start := clk.now()
	deadline := start.Add(timeout)

	var (
		ssmDenied   error // SSM refused the caller; it is not asked again
		ssmWasUp    bool  // the agent has registered at least once
		lastProbe   error // the latest SSM probe failure, once registered
		lastDescErr error // the latest non-authorization DescribeInstances failure
	)

	for {
		l, err := sig.liveness(ctx)
		switch {
		case err != nil && aws.IsAccessDenied(err):
			// The one channel that does not depend on the AMI is closed to
			// the caller: nothing this loop can learn would be evidence.
			return &sporedUnverifiableError{err: err}
		case err != nil:
			lastDescErr = err
		case l.Heartbeat != "":
			return nil
		case sporedTerminalStates[l.State]:
			return fmt.Errorf("instance is %s and spored never reported a heartbeat", l.State)
		case !l.HasInstanceProfile:
			// Without a profile spored has no credentials to tag with and the SSM
			// agent none to register with: dead, not slow.
			return fmt.Errorf("%w: instance has no IAM instance profile, so spored cannot write its heartbeat and the SSM agent cannot register — failing fast instead of waiting out %s",
				aws.ErrSSMUnreachable, timeout)
		}

		// The second signal. SSM can answer before the first heartbeat, but on
		// an AMI without the agent it never will, and a caller without the ssm:
		// actions cannot ask; neither is a verdict on spored.
		if ssmDenied == nil {
			up := ssmWasUp
			if !up {
				var oerr error
				up, oerr = sig.ssmOnline(ctx)
				if aws.IsAccessDenied(oerr) {
					ssmDenied, up = oerr, false
				}
				ssmWasUp = up
			}
			if up {
				res, perr := sig.probe(ctx)
				switch {
				case aws.IsAccessDenied(perr):
					ssmDenied = perr
				case perr == nil && res.Status == "Success":
					return nil
				case perr != nil:
					lastProbe = perr
				default:
					lastProbe = fmt.Errorf("spored status: %s: %s", res.Status,
						strings.TrimSpace(strings.TrimSpace(res.Stdout)+" "+strings.TrimSpace(res.Stderr)))
				}
			}
		}

		if !clk.now().Before(deadline) {
			return sporedTimeoutError(timeout, ssmDenied, ssmWasUp, lastProbe, lastDescErr)
		}
		wait := sporedVerifyPollInterval
		if rem := deadline.Sub(clk.now()); rem < wait {
			wait = rem
		}
		if err := clk.sleep(ctx, wait); err != nil {
			return sporedTimeoutError(timeout, ssmDenied, ssmWasUp, lastProbe, lastDescErr)
		}
	}
}

// sporedTimeoutError is the "spored did not come up" verdict, with what each
// channel saw so the reader can tell a dead spored from a quiet SSM.
func sporedTimeoutError(timeout time.Duration, ssmDenied error, ssmWasUp bool, lastProbe, lastDescErr error) error {
	msg := fmt.Sprintf("spored did not become ready within %s: no %s tag (spored writes it about a minute after it starts)",
		timeout, aws.HeartbeatTag)
	if lastDescErr != nil {
		msg += fmt.Sprintf("; last DescribeInstances error: %v", lastDescErr)
	}
	switch {
	case ssmDenied != nil:
		msg += fmt.Sprintf("; SSM could not be used as a second check: %v", ssmDenied)
	case !ssmWasUp:
		msg += "; the SSM agent never registered either (expected on AMIs without amazon-ssm-agent)"
	case lastProbe != nil:
		msg += fmt.Sprintf("; over SSM: %v", lastProbe)
	}
	return errors.New(msg + ". Check the boot log with `spawn logs <instance-id> --console`")
}

// sporedVerificationFailure turns a failed verification into the launch's error,
// terminating the instance first under --terminate-on-error. A verification the
// caller's credentials refused is reported as exactly that (#1126): the instance
// may be healthy, so it is not called "never came up".
func sporedVerificationFailure(ctx context.Context, client *aws.Client, region, instanceID string, terminateOnError bool, err error) error {
	var unverifiable *sporedUnverifiableError
	what := "spored never came up"
	if errors.As(err, &unverifiable) {
		what = "spored could not be verified"
	}
	if terminateOnError {
		fmt.Fprintf(os.Stderr, "\n⚠️  %s; terminating %s (--terminate-on-error)\n", what, instanceID)
		if terr := client.Terminate(ctx, region, instanceID); terr != nil {
			fmt.Fprintf(os.Stderr, "   terminate failed — DELETE IT MANUALLY (spawn terminate %s): %v\n", instanceID, terr)
		}
		return fmt.Errorf("instance %s launched but %s (terminated): %w", instanceID, what, err)
	}
	if unverifiable != nil {
		return fmt.Errorf("instance %s launched but %s — until spored is confirmed, treat it as having no TTL/idle safety net; "+
			"check it (spawn status %s) or terminate it (spawn terminate %s): %w",
			instanceID, what, instanceID, instanceID, err)
	}
	return fmt.Errorf("instance %s launched but %s — it has NO TTL/idle safety net; "+
		"inspect it (spawn connect %s) or terminate it (spawn terminate %s). Re-run with --terminate-on-error to auto-terminate: %w",
		instanceID, what, instanceID, instanceID, err)
}
