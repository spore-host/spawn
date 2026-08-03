package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spore-host/libs/pricing"
	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/launcher"
	"github.com/spore-host/spawn/pkg/platform"
	"github.com/spore-host/spawn/pkg/service"
)

// `spawn service` is the DCV-free long-lived-HTTP-service verb (spawn#409).
//
// It fills the gap between the two existing shapes: `spawn launch --command` is
// batch (fire, complete, reap) and `spawn app` is long-lived but DCV-coupled,
// with readiness expressed as an EC2 tag holding a browser URL. Neither can serve
// "run an HTTP binary and let me talk to it."
//
// spawn learns nothing about the workload. They meet at one JSON line on the
// child's stdout and one SSH port-forward — so any HTTP binary that prints the
// readiness line is spawnable by this one verb, and no workload is named
// anywhere in spawn.

var (
	serviceInstanceType  string
	serviceAMI           string
	serviceTTL           string
	serviceIdleTimeout   string
	serviceName          string
	serviceSpot          bool
	serviceUpload        string
	serviceRemotePath    string
	serviceAddrArgs      string
	serviceBootTimeout   time.Duration
	serviceLocalPort     int
	serviceOpenBrowser   bool
	serviceDryRun        bool
	serviceUser          string
	serviceKey           string
	serviceExistingHost  string
	serviceSubnetID      string
	serviceSecurityGroup []string
)

var serviceCmd = &cobra.Command{
	Use:   "service <command> [args...]",
	Short: "Run a long-lived HTTP service on an instance and tunnel to it",
	Long: `Launch an instance, run a long-lived HTTP service on it, and open a local
tunnel to whatever port the service chose (spawn#409).

The service must print exactly one JSON readiness line to stdout when it starts
serving:

  {"event":"ready","addr":"127.0.0.1:54321","provenance":{"sourceHash":"…"}}

  event   discriminator, so ordinary log lines are ignored
  addr    the address the service ACTUALLY bound, after :0 resolution
  token   optional access credential, carried into the printed URL

The service picks its own port — only it knows what is free — and announces it,
so spawn never polls and hopes. spawn appends "--addr 127.0.0.1:0" to your command
(change it with --addr-args) and forwards a local port to the announced address.

spawn holds the tunnel until you interrupt it or the instance's lifetime ends,
then terminates the instance. The service listens on the instance's loopback and
is reachable only through the tunnel — it is never exposed to the internet.

Examples:
  # Launch an instance, upload a binary, serve it, tunnel to it
  spawn service ./my-server --instance-type m7i.large --upload ./my-server --ttl 2h

  # Run something already baked into the AMI
  spawn service /opt/tools/dashboard --instance-type m7i.large --ttl 30m

  # Use an instance that is already running (it is not terminated afterwards)
  spawn service ./my-server --host my-box --upload ./my-server

  # Preview without launching
  spawn service ./my-server --instance-type m7i.large --ttl 1h --dry-run

Full contract, including how to make a binary spawnable:
https://github.com/spore-host/spawn/blob/main/docs/service-readiness-contract.md`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		addrArgs := serviceAddrArgsFor(cmd)

		if serviceDryRun {
			return renderServiceDryRun(os.Stdout, args, addrArgs)
		}

		client, err := aws.NewClient(ctx)
		if err != nil {
			return fmt.Errorf("init AWS client: %w", err)
		}
		return runServiceReal(ctx, os.Stdout, client, args, addrArgs)
	},
}

// serviceAddrArgsFor resolves the listen-address arguments appended to the
// command.
//
// An explicitly empty --addr-args means "append nothing" — for a service that
// takes its address from a config file or a different flag spelling. That can't
// be expressed as "" (which pkg/service reads as "use the default"), so it is
// passed as a space, which the remote command builder trims away.
func serviceAddrArgsFor(cmd *cobra.Command) string {
	if cmd.Flags().Changed("addr-args") && serviceAddrArgs == "" {
		return " "
	}
	return serviceAddrArgs
}

// serviceLaunchConfig maps the service flags to a launch config for one
// instance. Pure and testable: no AWS, no exec.
//
// AMI, UserData, and KeyName are deliberately left for launcher.Provision and the
// caller to fill, matching the DCV-free path `spawn task run` uses. No workload
// command is embedded either: unlike a batch task, a service is started over the
// SSH session so its stdout streams back here — via user-data it would go to
// /var/log/spawn-command.log on the box instead, where the readiness line is of
// no use to a launcher.
func serviceLaunchConfig(name, region string) aws.LaunchConfig {
	return aws.LaunchConfig{
		InstanceType:     serviceInstanceType,
		Region:           region,
		AMI:              serviceAMI,
		Name:             name,
		Spot:             serviceSpot,
		TTL:              serviceTTL,
		IdleTimeout:      serviceIdleTimeout,
		SubnetID:         serviceSubnetID,
		SecurityGroupIDs: serviceSecurityGroup,
		// A service does not "complete" — it serves until its lifetime ends — so
		// the TTL is what ends it, and it must terminate rather than stop: a
		// stopped instance still holds a billable EBS volume, and "everything dies
		// eventually" is about termination.
		OnComplete: "terminate",
		Tags:       map[string]string{"spawn:workload": "service"},
	}
}

// renderServiceDryRun prints the plan without launching anything.
func renderServiceDryRun(out io.Writer, args []string, addrArgs string) error {
	cfg := serviceLaunchConfig(serviceEffectiveName(), spawnRegion)
	// Show the TTL default a real run would get, so a dry run doesn't
	// under-report what bounds the spend.
	if applyIdleTimeoutDefault(&cfg) {
		defer fmt.Fprintln(out, "\nNote: with no --ttl or --idle-timeout, a 1h idle timeout is applied automatically.")
	}

	fmt.Fprintln(out, "DRY RUN — nothing will be launched.")
	fmt.Fprintln(out)
	if serviceExistingHost != "" {
		fmt.Fprintf(out, "Host:        %s (already running; not launched and not terminated)\n", serviceExistingHost)
	} else {
		fmt.Fprintf(out, "Instance:    %s in %s\n", orDash(cfg.InstanceType), orDash(cfg.Region))
		if cfg.Spot {
			fmt.Fprintln(out, "Purchase:    spot (cheaper; an interruption ends the service)")
		}
		fmt.Fprintf(out, "Lifetime:    ttl=%s idle=%s on-complete=%s\n",
			orDash(cfg.TTL), orDash(cfg.IdleTimeout), cfg.OnComplete)
	}
	fmt.Fprintf(out, "Command:     %s\n", serviceRemoteCommand(args, addrArgs))
	if serviceUpload != "" {
		fmt.Fprintf(out, "Upload:      %s → %s\n", serviceUpload, serviceEffectiveRemotePath())
	}
	fmt.Fprintf(out, "Readiness:   one JSON line on stdout, within %s\n", serviceEffectiveBootTimeout())
	fmt.Fprintf(out, "Tunnel:      127.0.0.1:%s → the address the service announces\n", localPortLabel())

	if cfg.Region != "" && cfg.InstanceType != "" && serviceExistingHost == "" {
		if rate := pricing.GetEC2HourlyRate(cfg.Region, cfg.InstanceType); rate > 0 {
			fmt.Fprintf(out, "Rate:        $%.4f/hr on-demand\n", rate)
			if d, err := time.ParseDuration(cfg.TTL); err == nil && d > 0 {
				fmt.Fprintf(out, "Max cost:    ~$%.2f (rate × ttl)\n", rate*d.Hours())
			}
		}
	}
	fmt.Fprintln(out, "\nRe-run without --dry-run to launch.")
	return nil
}

// serviceRemoteCommand renders the full remote command line for DISPLAY: the
// user's argv, shell quoted, with the listen-address arguments appended.
//
// The appending is pkg/service's job at run time (RemoteOptions.AddrFlagArgs), so
// this is only ever what the user is shown — passing an already-appended command
// to RunRemote would append them twice.
func serviceRemoteCommand(args []string, addrArgs string) string {
	if addrArgs = strings.TrimSpace(addrArgs); addrArgs != "" {
		return shellQuoteArgs(args) + " " + addrArgs
	}
	return shellQuoteArgs(args)
}

// runServiceReal does the whole ladder: launch (or attach), wait for SSH, upload
// the binary if asked, start the service, read its readiness line, forward a
// port, print the URL, hold, and reap.
func runServiceReal(ctx context.Context, out io.Writer, client *aws.Client, args []string, addrArgs string) error {
	region := spawnRegion
	if region == "" {
		region = client.Config().Region
	}

	var (
		instance *aws.InstanceInfo
		launched bool
		err      error
	)
	if serviceExistingHost != "" {
		instance, err = resolveInstance(ctx, client, serviceExistingHost)
		if err != nil {
			return err
		}
	} else {
		if region == "" {
			return errors.New("no region: pass --region or configure a default AWS region")
		}
		instance, err = launchServiceInstance(ctx, out, client, region)
		if err != nil {
			return err
		}
		launched = true
	}

	// Reap what we launched, on every exit path. An instance launched for a
	// service that then failed to start is exactly the zombie the TTL exists to
	// catch — catching it here costs cents instead of an hour.
	if launched {
		defer terminateService(ctx, out, client, instance)
	}

	target, err := resolveSSHTarget(ctx, client, instance, serviceUser, serviceKey, 0)
	if err != nil {
		return err
	}

	command, err := serviceCommandWithUpload(ctx, target, args)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "Starting the service; waiting up to %s for its readiness line...\n", serviceEffectiveBootTimeout())
	rem, err := service.RunRemote(ctx, service.RemoteOptions{
		Target:       target,
		Command:      command,
		AddrFlagArgs: addrArgs,
		BootTimeout:  serviceEffectiveBootTimeout(),
		LocalPort:    serviceLocalPort,
		// The service's own output goes to OUR stderr, never stdout: stdout is
		// reserved for this command's own result, so `-o json` stays parseable no
		// matter how chatty the workload is.
		Stdout: os.Stderr,
		Stderr: os.Stderr,
	})
	if err != nil {
		return fmt.Errorf("the service never became ready: %w", err)
	}
	defer func() { _ = rem.Stop() }()

	if err := reportServiceReady(out, instance, rem); err != nil {
		return err
	}
	if serviceOpenBrowser {
		// Best-effort: a headless machine has no browser, and that is not a
		// reason to tear down a working service.
		if berr := openBrowser(rem.LocalURL()); berr != nil {
			fmt.Fprintf(os.Stderr, "note: could not open a browser (%v) — use the URL above.\n", berr)
		}
	}
	return holdService(ctx, out, rem.Done(), func() bool {
		return serviceInstanceGone(ctx, client, instance)
	})
}

// reportServiceReady prints the ready service, in the caller's chosen format.
func reportServiceReady(out io.Writer, instance *aws.InstanceInfo, rem *service.Remote) error {
	if getOutputFormat() == "json" {
		// local_url carries the access token when the service requires one — this
		// is the one output that has to, or the caller cannot connect. local_addr is
		// the same endpoint without the credential, for logging.
		return json.NewEncoder(out).Encode(struct {
			InstanceID string `json:"instance_id"`
			Region     string `json:"region"`
			LocalURL   string `json:"local_url"`
			LocalAddr  string `json:"local_addr"`
			RemoteAddr string `json:"remote_addr"`
			TTL        string `json:"ttl,omitempty"`
		}{
			InstanceID: instance.InstanceID,
			Region:     instance.Region,
			LocalURL:   rem.LocalURL(),
			LocalAddr:  rem.Tunnel.LocalAddr,
			RemoteAddr: rem.Ready.Addr,
			TTL:        serviceTTL,
		})
	}

	fmt.Fprintf(out, "\n✅ Service ready\n")
	fmt.Fprintf(out, "   URL:       %s\n", rem.LocalURL())
	fmt.Fprintf(out, "   Instance:  %s in %s\n", instance.InstanceID, instance.Region)
	fmt.Fprintf(out, "   Forward:   %s → %s (the instance's loopback)\n", rem.Tunnel.LocalAddr, rem.Ready.Addr)
	if prov := serviceProvenance(rem.Ready); prov != "" {
		fmt.Fprintf(out, "   Built:     %s\n", prov)
	}
	fmt.Fprintf(out, "\nPress Ctrl-C to stop the service")
	if serviceExistingHost == "" {
		fmt.Fprintf(out, " and terminate the instance")
	}
	fmt.Fprintln(out, ".")
	return nil
}

// holdService blocks until the user interrupts, the session ends, or the context
// is cancelled.
//
// It exists because the service is reachable only through this process's tunnel:
// exiting early would leave a running instance serving something nobody can
// reach. All three exits lead to the same teardown.
//
// sessionDone is the service's Done channel and instanceGone reports whether
// lifetime enforcement already reaped the instance; taking both as parameters
// keeps the exit classification below testable without AWS.
func holdService(ctx context.Context, out io.Writer, sessionDone <-chan struct{}, instanceGone func() bool) error {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)

	select {
	case <-sig:
		fmt.Fprintf(out, "\nInterrupted — shutting the service down.\n")
		return nil
	case <-sessionDone:
		// The session ended on its own. Two very different causes look identical
		// from here — the instance's lifetime expired (the designed end) or the
		// connection dropped (a failure) — so ask EC2 which it was rather than
		// guessing. A wrong guess either cries wolf on every TTL expiry or hides a
		// real drop.
		if instanceGone() {
			fmt.Fprintf(out, "\nThe instance's lifetime ended and it was reaped.\n")
			return nil
		}
		fmt.Fprintf(out, "\nThe SSH session to the service ended.\n")
		return errors.New("the service's session ended while the instance was still running")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// serviceInstanceGone reports whether the instance is no longer running, i.e.
// whether lifetime enforcement already reaped it.
func serviceInstanceGone(ctx context.Context, client *aws.Client, instance *aws.InstanceInfo) bool {
	cur, err := resolveInstance(ctx, client, instance.InstanceID)
	if err != nil {
		// A lookup failure is not evidence of a reap. Treating an API hiccup as a
		// completed reap would silence exactly the case worth reporting — a dropped
		// session while the instance is still up and still billing.
		return false
	}
	return instanceStateIsGone(cur.State)
}

// instanceStateIsGone classifies an EC2 instance state as "no longer serving".
//
// "stopping"/"stopped" count: an on-complete=stop path or a manual stop ends the
// service just as surely as a terminate, even though the instance still exists.
func instanceStateIsGone(state string) bool {
	switch state {
	case "running", "pending":
		return false
	default:
		return true
	}
}

// terminateService terminates the instance the service ran on, loudly on failure:
// a failed terminate is a billing leak, and the TTL is then the only backstop
// left.
func terminateService(ctx context.Context, out io.Writer, client *aws.Client, instance *aws.InstanceInfo) {
	// The caller's context may already be cancelled (Ctrl-C, deadline) — but the
	// instance still has to die, so the teardown gets its own.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()

	fmt.Fprintf(out, "Terminating %s...\n", instance.InstanceID)
	if err := client.Terminate(ctx, instance.Region, instance.InstanceID); err != nil {
		fmt.Fprintf(os.Stderr, "\n⚠️  FAILED to terminate %s: %v\n", instance.InstanceID, err)
		fmt.Fprintf(os.Stderr, "   The instance is still running and still costing money.\n")
		fmt.Fprintf(os.Stderr, "   Terminate it now:  spawn terminate %s\n", instance.InstanceID)
		fmt.Fprintf(os.Stderr, "   Its lifetime will also reap it; verify with: spawn orphans\n")
		return
	}
	fmt.Fprintf(out, "✅ Terminated %s\n", instance.InstanceID)
}

// launchServiceInstance launches the instance a service will run on, through the
// mandatory-TTL guard so it inherits the whole lifecycle safety net.
func launchServiceInstance(ctx context.Context, out io.Writer, client *aws.Client, region string) (*aws.InstanceInfo, error) {
	if serviceInstanceType == "" {
		return nil, errors.New("--instance-type is required (or pass --host to use an instance that is already running)")
	}
	cfg := serviceLaunchConfig(serviceEffectiveName(), region)

	// Cost safety, non-negotiable: this is a FLAG-driven verb, so unlike the
	// spec-driven task path it has no schema requiring a TTL. Without this guard,
	// `spawn service` with no --ttl would run until someone noticed the bill.
	if err := guardZombieInstance(&cfg); err != nil {
		return nil, err
	}

	// A service is reached over SSH, so the instance needs a key: the keyless
	// SSM-only shape the headless path can use cannot carry a port-forward.
	plat, err := platform.Detect()
	if err != nil {
		return nil, fmt.Errorf("detect platform: %w", err)
	}
	if cfg.AMI == "" {
		// Resolved here rather than left to Provision because the key setup below
		// needs the AMI to pick the right key for the OS.
		ami, err := client.GetRecommendedAMI(ctx, region, cfg.InstanceType)
		if err != nil {
			return nil, fmt.Errorf("auto-detect an AMI for %s in %s: %w", cfg.InstanceType, region, err)
		}
		cfg.AMI = ami
	}
	keyName, err := setupSSHKey(ctx, client, region, cfg.AMI, plat)
	if err != nil {
		return nil, fmt.Errorf("set up an SSH key: %w", err)
	}
	cfg.KeyName = keyName
	pubKey, err := spawnPublicKeyForUserData(plat, keyName)
	if err != nil {
		return nil, fmt.Errorf("read the SSH public key: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Launching %s in %s...\n", cfg.InstanceType, region)
	result, err := launcher.Provision(ctx, client, cfg, launcher.Options{
		Username:  plat.GetUsername(),
		PublicKey: pubKey,
	})
	if err != nil {
		return nil, fmt.Errorf("launch the service instance: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Launched %s; waiting for it to run...\n", result.InstanceID)

	stub := &aws.InstanceInfo{InstanceID: result.InstanceID, Region: region}
	if err := client.WaitForRunning(ctx, region, result.InstanceID, 5*time.Minute); err != nil {
		// Terminate before returning: the instance exists and is billing whether or
		// not it ever reached running, and the caller gets no handle to clean up.
		terminateService(ctx, out, client, stub)
		return nil, fmt.Errorf("instance %s did not reach running: %w", result.InstanceID, err)
	}

	inst, err := resolveInstance(ctx, client, result.InstanceID)
	if err != nil {
		terminateService(ctx, out, client, stub)
		return nil, err
	}
	if inst.PublicIP != "" {
		fmt.Fprintf(os.Stderr, "Waiting for SSH on %s...\n", inst.PublicIP)
		waitForSSHReady(ctx, inst.PublicIP, 3*time.Minute)
	}
	return inst, nil
}

// serviceCommandWithUpload uploads --upload if given and returns the remote
// command to run — WITHOUT the listen-address arguments, which pkg/service
// appends. The uploaded path is substituted for the local one when the command IS
// the uploaded file.
//
// The upload exists because the service usually isn't in the AMI, and launching a
// box you then have to provision by hand makes the verb useless on its own.
func serviceCommandWithUpload(ctx context.Context, target service.SSHTarget, args []string) (string, error) {
	if serviceUpload == "" {
		return shellQuoteArgs(args), nil
	}
	remotePath := serviceEffectiveRemotePath()
	fmt.Fprintf(os.Stderr, "Uploading %s → %s:%s\n", serviceUpload, target.Host, remotePath)
	if err := uploadToInstance(ctx, target, serviceUpload, remotePath); err != nil {
		return "", fmt.Errorf("upload %s: %w", serviceUpload, err)
	}
	return shellQuoteArgs(substituteUploadedPath(args, serviceUpload, remotePath)), nil
}

// substituteUploadedPath points the command at the uploaded copy when the command
// IS the uploaded file. If it isn't, the upload is a data or auxiliary file and
// the command is left alone.
//
// The comparison is on the unquoted argv, before quoting: comparing quoted
// strings would miss the match for any path needing an escape.
func substituteUploadedPath(args []string, localPath, remotePath string) []string {
	if len(args) == 0 || args[0] != localPath {
		return args
	}
	out := append([]string{}, args...)
	out[0] = remotePath
	return out
}

// uploadToInstance copies a local file to the instance and makes it executable.
func uploadToInstance(ctx context.Context, target service.SSHTarget, localPath, remotePath string) error {
	if _, err := os.Stat(localPath); err != nil {
		return fmt.Errorf("local file: %w", err)
	}
	if err := runQuietly(ctx, "scp", scpArgs(target, localPath, remotePath)); err != nil {
		return err
	}
	// chmod +x unconditionally: the common case is a binary, an executable data
	// file is harmless, and doing it here keeps a chmod out of the user's command.
	return runQuietly(ctx, "ssh", service.SSHExecArgs(target, "chmod +x "+shellQuoteArgs([]string{remotePath})))
}

// scpArgs builds the scp argument vector. Pure and testable; no exec.
//
// The options match SSHExecArgs so an upload can't succeed against a host the
// service session would then fail to reach, or the reverse.
func scpArgs(target service.SSHTarget, localPath, remotePath string) []string {
	args := []string{
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ConnectTimeout=10",
		"-o", "BatchMode=yes",
		"-o", "ControlMaster=no",
		"-o", "ControlPath=none",
	}
	if target.KeyPath != "" {
		args = append(args, "-i", target.KeyPath)
	}
	if target.Port != 0 && target.Port != 22 {
		// scp spells the port -P; -p means "preserve times".
		args = append(args, "-P", strconv.Itoa(target.Port))
	}
	return append(args, localPath, target.User+"@"+target.Host+":"+remotePath)
}

// runQuietly runs a helper command, surfacing its output only on failure — where
// it is the entire diagnosis.
func runQuietly(ctx context.Context, name string, args []string) error {
	combined, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		if trimmed := strings.TrimSpace(string(combined)); trimmed != "" {
			return fmt.Errorf("%s failed: %w: %s", name, err, trimmed)
		}
		return fmt.Errorf("%s failed: %w", name, err)
	}
	return nil
}

// serviceProvenance renders the readiness line's provenance for display: the
// answer to "what exactly did I just spawn?".
//
// The fields are treated as opaque key/values on purpose — provenance is the
// workload's shape, not spawn's, and all of it is optional to us.
func serviceProvenance(ev *service.ReadyEvent) string {
	if len(ev.Provenance) == 0 {
		return ""
	}
	var fields map[string]any
	if err := json.Unmarshal(ev.Provenance, &fields); err != nil {
		return ""
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys) // stable line; map order is random
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, fields[k]))
	}
	return strings.Join(parts, " ")
}

func serviceEffectiveName() string {
	if serviceName != "" {
		return serviceName
	}
	return "spawn-service"
}

func serviceEffectiveRemotePath() string {
	if serviceRemotePath != "" {
		return serviceRemotePath
	}
	return "/tmp/spawn-service-bin"
}

func serviceEffectiveBootTimeout() time.Duration {
	if serviceBootTimeout > 0 {
		return serviceBootTimeout
	}
	return service.DefaultBootTimeout
}

func localPortLabel() string {
	if serviceLocalPort > 0 {
		return strconv.Itoa(serviceLocalPort)
	}
	return "<free port>"
}

func init() {
	rootCmd.AddCommand(serviceCmd)

	serviceCmd.Flags().StringVar(&serviceInstanceType, "instance-type", "", "EC2 instance type to launch (required unless --host is given)")
	serviceCmd.Flags().StringVar(&serviceAMI, "ami", "", "AMI to launch (default: the recommended AMI for the instance type)")
	serviceCmd.Flags().StringVar(&serviceTTL, "ttl", "", "Terminate the instance after this long (e.g. 2h). With no --ttl and no --idle-timeout, a 1h idle timeout is applied")
	serviceCmd.Flags().StringVar(&serviceIdleTimeout, "idle-timeout", "", "Terminate the instance after this much idleness (e.g. 30m)")
	serviceCmd.Flags().StringVar(&serviceName, "name", "", "Name tag for the instance (default: spawn-service)")
	serviceCmd.Flags().BoolVar(&serviceSpot, "spot", false, "Launch as a spot instance (cheaper; an interruption ends the service)")
	serviceCmd.Flags().StringVar(&serviceSubnetID, "subnet-id", "", "Subnet to launch into")
	serviceCmd.Flags().StringSliceVar(&serviceSecurityGroup, "security-group-ids", nil, "Security groups for the instance")

	serviceCmd.Flags().StringVar(&serviceUpload, "upload", "", "Local file to copy to the instance before starting (usually the service binary)")
	serviceCmd.Flags().StringVar(&serviceRemotePath, "remote-path", "", "Where --upload lands on the instance (default: /tmp/spawn-service-bin)")
	serviceCmd.Flags().StringVar(&serviceAddrArgs, "addr-args", "--addr 127.0.0.1:0",
		"Arguments telling the service where to listen; pass an empty string to append nothing")
	serviceCmd.Flags().DurationVar(&serviceBootTimeout, "boot-timeout", 0,
		"How long to wait for the readiness line (default 2m; raise it for a service that does heavy work before serving)")
	serviceCmd.Flags().IntVar(&serviceLocalPort, "local-port", 0, "Local port for the tunnel (default: a free port)")
	serviceCmd.Flags().BoolVar(&serviceOpenBrowser, "open", false, "Open the service URL in a browser once it is ready")
	serviceCmd.Flags().BoolVar(&serviceDryRun, "dry-run", false, "Print the plan without launching anything")

	serviceCmd.Flags().StringVar(&serviceExistingHost, "host", "",
		"Run on an instance that is already running (name or instance ID); it is not terminated afterwards")
	serviceCmd.Flags().StringVar(&serviceUser, "user", "", "SSH user (default: the instance's spawn:local-username tag, else ec2-user)")
	serviceCmd.Flags().StringVar(&serviceKey, "key", "", "SSH private key (default: the instance's launch key)")
}
