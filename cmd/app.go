package cmd

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	htmpl "html/template"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/spore-host/libs/catalog"
	spawnclient "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/platform"
)

// ── app command flags ────────────────────────────────────────────────────────

var (
	appLaunchName         string
	appLaunchInstanceType string
	appLaunchRegion       string
	appLaunchSpot         bool
	appLaunchTTL          string
	appLaunchIdleTimeout  string
	appLaunchNoOpen       bool   // --no-open: write session file but don't open browser
	appLaunchVersion      string // --app-version: container image tag (#290)
	appLaunchImage        string // --image: ad-hoc image binding, BYO (spore-host#392)
	appCatalogPath        string // --catalog: local overlay file path (spore-host#392)
)

// ── command tree ─────────────────────────────────────────────────────────────

var appGroupCmd = &cobra.Command{
	Use:   "app",
	Short: "Launch and manage catalog applications via NICE DCV",
	Long: `Launch streamable research applications in the cloud.

Each application is pre-configured with the right instance type, NICE DCV
for browser-based streaming, and automatic idle termination.

Apps you can launch depend on your account: public images are available to
everyone; private images appear only if your account can pull them. Bring your
own image with --image, or add bindings in ~/.spawn/catalog.yaml (--catalog).

Examples:
  spawn app list                       # show apps launchable from your account
  spawn app launch paraview            # launch ParaView on a GPU instance
  spawn app launch igv --region us-west-2
  spawn app launch paraview --spot --ttl 4h
  spawn app launch paraview --image 123456789012.dkr.ecr.us-east-1.amazonaws.com/paraview:5.13.2`,
}

var appListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all streamable applications in the catalog",
	RunE:  runAppList,
}

var appLaunchCmd = &cobra.Command{
	Use:   "launch <app-name>",
	Short: "Launch a catalog application via NICE DCV",
	Args:  cobra.ExactArgs(1),
	RunE:  runAppLaunch,
}

func init() {
	rootCmd.AddCommand(appGroupCmd)
	appGroupCmd.AddCommand(appListCmd, appLaunchCmd)

	appLaunchCmd.Flags().StringVar(&appLaunchName, "name", "", "Session name (default: <app>-<timestamp>)")
	appLaunchCmd.Flags().StringVar(&appLaunchInstanceType, "instance-type", "", "Override instance type (default: first catalog family + .xlarge)")
	appLaunchCmd.Flags().StringVar(&appLaunchRegion, "region", "", "AWS region (default: from AWS config)")
	appLaunchCmd.Flags().BoolVar(&appLaunchSpot, "spot", false, "Use Spot pricing")
	appLaunchCmd.Flags().StringVar(&appLaunchTTL, "ttl", "", "Hard termination deadline (e.g. 4h, 8h)")
	appLaunchCmd.Flags().StringVar(&appLaunchIdleTimeout, "idle-timeout", "", "Stop when DCV has no clients for this duration (default: catalog default)")
	appLaunchCmd.Flags().BoolVar(&appLaunchNoOpen, "no-open", false, "Write session file but do not open browser automatically")
	appLaunchCmd.Flags().StringVar(&appLaunchVersion, "app-version", "", "App container image tag to launch (default: catalog default; see 'spawn app list')")
	appLaunchCmd.Flags().StringVar(&appLaunchImage, "image", "", "Launch a BYO container image for this app (overrides the catalog binding), e.g. 123456789012.dkr.ecr.us-east-1.amazonaws.com/paraview:5.13.2")

	// --catalog applies to both `list` and `launch`: it points the catalog at a
	// local overlay (BYO images, spore-host#392), applied before any catalog read.
	appGroupCmd.PersistentFlags().StringVar(&appCatalogPath, "catalog", "", "Local catalog overlay file (default: $SPAWN_CATALOG or ~/.spawn/catalog.yaml)")
	appGroupCmd.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		if appCatalogPath != "" {
			catalog.SetOverlayPath(appCatalogPath)
			catalog.Reload()
		}
		// Surface a malformed/missing overlay loudly but non-fatally — the embedded
		// catalog still works (the overlay is opt-in convenience, not required).
		if err := catalog.LoadError(); err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  catalog overlay: %v (using built-in catalog)\n", err)
		}
		return nil
	}
}

// ── spawn app list ────────────────────────────────────────────────────────────

func runAppList(cmd *cobra.Command, args []string) error {
	// Per-account view (#392): an app is shown only if its image resolves for this
	// account — public always, private only if owned by the caller. Account lookup
	// is best-effort: with no creds it's "", which lists public apps only.
	callerAccount := ""
	if client, err := spawnclient.NewClient(context.Background()); err == nil {
		if acct, err := client.GetAccountID(context.Background()); err == nil {
			callerAccount = acct
		}
	}
	all := catalog.List()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tDESCRIPTION\tGPU\tSTATUS\tVERSION\tFAMILIES")
	shown, recipes := 0, 0
	for _, app := range all {
		show, status := classifyForList(&app, callerAccount)
		if !show {
			continue // private image this account can't pull (#392)
		}
		shown++
		gpu := "no"
		if app.GPU {
			gpu = "yes"
		}
		// STATUS: "launchable" (image resolves / legacy command) vs "recipe"
		// (buildable definition — bake it, then bind via overlay or --image, #392).
		statusLabel := "launchable"
		version := "—"
		if status == appStatusRecipe {
			statusLabel = "recipe available"
			recipes++
		} else if app.Containerized() {
			version = app.TagDefault
			if len(app.TagsAvailable) > 1 {
				version += " (+)"
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			app.Name,
			app.Description,
			gpu,
			statusLabel,
			version,
			strings.Join(app.InstanceFamilies, ", "),
		)
	}
	if shown == 0 {
		fmt.Fprintln(os.Stderr, "No applications available for this account.")
		return nil
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if recipes > 0 {
		fmt.Fprintf(os.Stderr, "\n%d app(s) are \"recipe available\": build the image (see infra/amis/containers/<app>),\nthen bind it in ~/.spawn/catalog.yaml or launch with --image <ref>.\n", recipes)
	}
	return nil
}

// ── spawn app launch ──────────────────────────────────────────────────────────

func runAppLaunch(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	appArg := args[0]

	// 1. Resolve catalog entry
	entry, ok := catalog.Lookup(appArg)
	if !ok {
		return fmt.Errorf("application %q not found in catalog — run 'spawn app list' to see available apps", appArg)
	}

	fmt.Fprintf(os.Stderr, "Launching %s — %s\n", entry.Name, entry.Description)

	// 2. Create AWS client
	client, err := spawnclient.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("create AWS client: %w", err)
	}

	// 3. Resolve region
	region := appLaunchRegion
	if region == "" {
		// Use the client's configured region
		cfg, err := client.GetConfig(ctx)
		if err != nil || cfg.Region == "" {
			region = "us-east-1"
		} else {
			region = cfg.Region
		}
	}

	// 4. Pick instance type
	instanceType := appLaunchInstanceType
	if instanceType == "" {
		if len(entry.InstanceFamilies) == 0 {
			return fmt.Errorf("no instance families defined for %s in catalog", entry.Name)
		}
		instanceType = entry.InstanceFamilies[0] + ".xlarge"
		fmt.Fprintf(os.Stderr, "Instance type: %s (default; override with --instance-type)\n", instanceType)
	}

	// 4b. --image: ad-hoc BYO binding (spore-host#392). Rebinds this app to a
	// user-supplied image (e.g. their own private ECR) without a catalog/overlay
	// edit. The ref may include a tag (image:tag); a bare ref uses --app-version
	// or "latest". Sets visibility by inference so private ECR triggers auth.
	if appLaunchImage != "" {
		img, tag := splitImageRef(appLaunchImage)
		entry.Image = img
		entry.Visibility = "" // re-infer from the new host
		if tag != "" {
			entry.TagDefault = tag
			entry.TagsAvailable = []string{tag}
		}
	}

	// 5. Resolve image + base AMI (container catalog, #290).
	//
	// Container apps run a Docker image (entry.Image:tag) on the shared
	// spore-dcv-base AMI (entry.BaseAMIs[region]) — no per-app AMI. The image tag
	// is the requested --app-version validated against the catalog, or the
	// default. A not-yet-containerized app (no Image) still launches via its
	// legacy launch_command on a standard AL2023 AMI.
	imageTag := ""
	if entry.Containerized() {
		imageTag, err = entry.ResolveTag(appLaunchVersion)
		if err != nil {
			return err // names the available versions
		}
	} else if appLaunchVersion != "" {
		return fmt.Errorf("--app-version is not supported for %s (not a containerized app)", entry.Name)
	} else if entry.LaunchCommand == "" {
		// No image (catalog/overlay/--image) and no legacy launch command → nothing
		// to launch. This is the definition-only case (#392): the global catalog
		// may ship an app's hardware spec without an image; the user supplies one.
		return fmt.Errorf("no image configured for %s — supply one with --image <ref>, or add a binding to ~/.spawn/catalog.yaml (see 'spawn app list')", entry.Name)
	}

	// Launchability filter (#392): refuse a private image this account can't pull.
	if entry.Containerized() {
		callerAccount, _ := client.GetAccountID(ctx)
		if ok, reason := appResolvable(entry, callerAccount); !ok {
			return fmt.Errorf("cannot launch %s: %s — its image %q is not pullable from this account", entry.Name, reason, entry.Image)
		}
	}

	ami := ""
	if entry.BaseAMIs != nil {
		ami = entry.BaseAMIs[region]
	}
	if ami == "" {
		if entry.Containerized() {
			return fmt.Errorf("no spore-dcv-base AMI for %s in %s — build/share it (infra/amis/dcv-gpu-al2023.pkr.hcl) or try --region us-east-1", entry.Name, region)
		}
		fmt.Fprintf(os.Stderr, "No base AMI for %s in %s — using standard AL2023 AMI (install DCV manually)\n", entry.Name, region)
		ami, err = client.GetAL2023AMI(ctx, region, "x86_64", false)
		if err != nil {
			return fmt.Errorf("get AL2023 AMI: %w", err)
		}
	}

	// 6. Session name
	sessionName := appLaunchName
	if sessionName == "" {
		sessionName = fmt.Sprintf("%s-%d", entry.Name, time.Now().Unix())
	}

	// 7. Idle timeout: flag > catalog default > none
	idleTimeout := appLaunchIdleTimeout
	if idleTimeout == "" && entry.IdleTimeoutDefault != "" {
		idleTimeout = entry.IdleTimeoutDefault
		fmt.Fprintf(os.Stderr, "Idle timeout: %s (from catalog; override with --idle-timeout)\n", idleTimeout)
	}

	// 8. SSH key pair — resolve local key and import to AWS if needed
	plat, err := platform.Detect()
	if err != nil {
		return fmt.Errorf("detect platform: %w", err)
	}
	keyName, err := setupSSHKey(ctx, client, region, ami, plat)
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  SSH key setup failed: %v — launching without key (DCV only)\n", err)
		keyName = ""
	}

	// 9. IAM instance profile — spored needs EC2/tag permissions to write spawn:ready-url
	iamProfile := ""
	if p, err := client.SetupSporedIAMRole(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  IAM role setup failed: %v — spored may not be able to write tags\n", err)
	} else {
		iamProfile = p
	}

	// 10. Security group for DCV (port 8443)
	vpcID, err := client.GetDefaultVPC(ctx, region)
	if err != nil {
		return fmt.Errorf("get default VPC: %w", err)
	}
	dcvSGID, err := client.CreateOrGetDCVSecurityGroup(ctx, region, vpcID)
	if err != nil {
		return fmt.Errorf("create DCV security group: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Security group: %s (spawn-dcv, port 8443 open)\n", dcvSGID)

	// 9. DCV session ID (fixed "console" — DCV default session name)
	dcvSessionID := "console"

	// 10. DCV user-data: start DCV server + create session. Container apps (#290)
	// pre-pull the image and run it into the DCV display as the session init;
	// legacy apps use the baked launch_command.
	var dcvUserData string
	if entry.Containerized() {
		image := entry.Image + ":" + imageTag
		private := entry.ImageVisibility() == catalog.VisibilityPrivate
		fmt.Fprintf(os.Stderr, "App image: %s (%s)\n", image, entry.ImageVisibility())
		dcvUserData = buildContainerDCVUserData(image, entry.GPU, private, region, dcvSessionID)
	} else {
		dcvUserData = buildDCVUserData(entry.LaunchCommand, dcvSessionID)
	}

	// 14. Build LaunchConfig
	lc := spawnclient.LaunchConfig{
		Name:               sessionName,
		DNSName:            sessionName, // enables FQDN in ready-url → wildcard cert matches
		InstanceType:       instanceType,
		Region:             region,
		AMI:                ami,
		KeyName:            keyName,
		IamInstanceProfile: iamProfile,
		Spot:               appLaunchSpot,
		TTL:                appLaunchTTL,
		IdleTimeout:        idleTimeout,
		OnComplete:         "stop", // stop (not terminate) so session can be restarted
		SecurityGroupIDs:   []string{dcvSGID},
		UserData:           dcvUserData,
		DCVSessionID:       dcvSessionID,
		AppName:            entry.Name,
		RootVolumeSizeGiB:  40, // AL2023 base AMI built with 40 GB root
	}

	// 12. Launch
	fmt.Fprintf(os.Stderr, "Launching %s in %s...\n", instanceType, region)
	result, err := client.Launch(ctx, lc)
	if err != nil {
		return fmt.Errorf("launch failed: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Instance: %s\n", result.InstanceID)

	// 13. Wait for public IP (EC2 often doesn't assign it before RunInstances returns)
	host := result.PublicIP
	if host == "" {
		fmt.Fprintf(os.Stderr, "Waiting for public IP...")
		for i := 0; i < 30; i++ {
			time.Sleep(3 * time.Second)
			fmt.Fprintf(os.Stderr, ".")
			ip, err := client.GetInstancePublicIP(ctx, region, result.InstanceID)
			if err == nil && ip != "" {
				host = ip
				fmt.Fprintf(os.Stderr, " %s\n", host)
				break
			}
		}
		if host == "" {
			fmt.Fprintf(os.Stderr, " (no public IP assigned)\n")
		}
	}

	// 14. Poll for spawn:ready-url written by spored's DCV token verifier.
	// spored starts a tiny HTTP server, waits for DCV, generates a token, writes the tag.
	dnsName := sessionName // spored will register a DNS name; fall back to IP
	authToken := ""
	if entry.DCVEnabled {
		fmt.Fprintf(os.Stderr, "Waiting for DCV session URL")
		// Boot + user-data + DCV start + spored init takes ~3-4 minutes.
		// Poll for up to 5 minutes (60 × 5s).
		var lastStatus string
		for i := 0; i < 60; i++ {
			time.Sleep(5 * time.Second)
			fmt.Fprintf(os.Stderr, ".")
			instances, err := client.ListInstances(ctx, region, "running")
			if err != nil {
				continue
			}
			scan := scanDCVReady(instances, result.InstanceID)
			if scan.dnsName != "" {
				dnsName = scan.dnsName
			}
			lastStatus = scan.status
			if scan.token != "" {
				authToken = scan.token
			}
			if scan.host != "" {
				host = scan.host
			}
			if authToken != "" {
				fmt.Fprintf(os.Stderr, " ready\n")
				break
			}
			// A NAMED terminal failure (spored recorded WHY) — stop polling and
			// report the specific cause instead of spinning to the generic timeout
			// (spawn#282). dcv-waiting / empty status keep polling.
			if dcvStatusTerminal(lastStatus) {
				fmt.Fprintf(os.Stderr, " failed\n")
				break
			}
		}
		if authToken == "" {
			fmt.Fprintf(os.Stderr, "%s\n", dcvFailureMessage(lastStatus, result.InstanceID))
		}
	}

	// 15. Write session HTML file
	sessionFile, err := writeSessionHTML(result.InstanceID, sessionName, dnsName, host, entry.Name, entry.Description, instanceType, authToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  Failed to write session file: %v\n", err)
	} else {
		fmt.Fprintf(os.Stdout, "\n✅  %s is ready\n", entry.Name)
		fmt.Fprintf(os.Stdout, "   Session: %s\n", sessionName)
		fmt.Fprintf(os.Stdout, "   File:    %s\n", sessionFile)
		fmt.Fprintf(os.Stdout, "   Reconnect: spawn app launch %s --name %s\n\n", entry.Name, sessionName)

		// 15. Open in browser
		if !appLaunchNoOpen {
			if err := openBrowser(sessionFile); err != nil {
				fmt.Fprintf(os.Stderr, "⚠️  Could not open browser automatically: %v\n", err)
				fmt.Fprintf(os.Stderr, "   Open manually: file://%s\n", sessionFile)
			}
		}
	}

	return nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

// containerRunPath is where the per-launch container-run wrapper is installed.
// DCV invokes it as the session --init, so it runs in DCV's session environment.
const containerRunPath = "/usr/local/bin/spore-app-run"

// containerRunWrapper returns a shell script that runs the app container into the
// *DCV virtual session's* display (#263). DCV sets DISPLAY/XAUTHORITY in the
// --init process environment for the per-session Xdcv server — which is NOT host
// :0, the bug that made the first container launch fail with "Unable to open X
// display :0". The wrapper reads those at session-launch time and passes them
// into the container, bind-mounting the X socket dir and the xauth file. Pure, so
// it's unit-tested without Docker/EC2.
func containerRunWrapper(image string, gpu bool) string {
	gpuFlag := ""
	if gpu {
		gpuFlag = "--gpus all "
	}
	// $DISPLAY/$XAUTHORITY come from DCV's session env. Fall back to :0 only if
	// DCV didn't set DISPLAY (e.g. a console session). The xauth file is mounted
	// read-only at a fixed path so the container user (uid 1000) can authenticate.
	return fmt.Sprintf(`#!/bin/bash
# spore container app runner — executed by DCV as the session --init (#263).
# DCV exports DISPLAY/XAUTHORITY for the virtual session; pass them to the container.
set -u
DISP="${DISPLAY:-:0}"
XAUTH="${XAUTHORITY:-$HOME/.Xauthority}"
XAUTH_ARGS=""
if [ -f "$XAUTH" ]; then
  XAUTH_ARGS="-e XAUTHORITY=/tmp/.xauth -v $XAUTH:/tmp/.xauth:ro"
fi
exec /usr/bin/docker run --rm %s--network host \
  -e DISPLAY="$DISP" $XAUTH_ARGS \
  -v /tmp/.X11-unix:/tmp/.X11-unix \
  %s
`, gpuFlag, image)
}

// buildContainerDCVUserData builds user-data for a containerized app (#290): it
// pre-pulls the image, installs the session-display-aware run wrapper (#263), and
// starts a DCV session whose --init is that wrapper.
func buildContainerDCVUserData(image string, gpu, private bool, region, sessionID string) string {
	wrapper := containerRunWrapper(image, gpu)
	// For a private-ECR image, authenticate the Docker client with the instance
	// role before pulling (spore-host#392). Public images skip this (anonymous
	// pull). Cross-account private images additionally need the repo policy to
	// grant this account — if that's missing the pull fails with a clear error.
	login := ""
	if private {
		login = fmt.Sprintf(`echo 'Authenticating to private ECR (%s)...'
aws ecr get-login-password --region %s | /usr/bin/docker login --username AWS --password-stdin %s 2>&1 || echo 'WARNING: ECR login failed; private pull will fail'
`, region, region, ecrRegistryHost(image))
	}
	// preCreate runs in the user-data (root) before create-session: (optionally
	// log in to ECR), pull the image, and install the wrapper DCV invokes as --init.
	preCreate := fmt.Sprintf(`%secho 'Pre-pulling %s...'
/usr/bin/docker pull %s || echo 'WARNING: docker pull failed; session init will retry the run'
cat > %s <<'EOFAPPRUN'
%sEOFAPPRUN
chmod +x %s
`, login, image, image, containerRunPath, wrapper, containerRunPath)
	return buildDCVUserDataWithInit(containerRunPath, sessionID, preCreate, image)
}

// buildDCVUserData returns a base64-encoded user-data script that starts DCV and creates a session.
// spored is already pre-installed in the catalog AMI.
func buildDCVUserData(launchCommand, sessionID string) string {
	return buildDCVUserDataWithInit(launchCommand, sessionID, "", launchCommand)
}

// buildDCVUserDataWithInit is the shared user-data core: update spored, install
// the DCV TLS cert, start spored + dcvserver, optionally run preCreate (e.g. a
// container pre-pull), then create the DCV session with initCommand as its
// --init. label is a human string echoed for logging. Returns base64 user-data.
func buildDCVUserDataWithInit(initCommand, sessionID, preCreate, label string) string {
	script := fmt.Sprintf(`#!/bin/bash
set -e

# Detect region and architecture
REGION=$(curl -sf -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' http://169.254.169.254/latest/api/token | xargs -I{} curl -sf -H 'X-aws-ec2-metadata-token: {}' http://169.254.169.254/latest/meta-data/placement/region 2>/dev/null || echo us-east-1)
ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')

# Update spored from S3 to pick up the latest version (AMI may have an older copy).
# Try project-prefixed path first, then fall back to legacy path
curl -fsSL "https://spawn-binaries-${REGION}.s3.amazonaws.com/spawn/spored-linux-${ARCH}" -o /tmp/spored-new 2>/dev/null || \
  curl -fsSL "https://spawn-binaries-${REGION}.s3.amazonaws.com/spored-linux-${ARCH}" -o /tmp/spored-new
if [ -f /tmp/spored-new ]; then
  chmod +x /tmp/spored-new && mv /tmp/spored-new /usr/local/bin/spored || true
fi

# Install wildcard TLS cert for DCV before dcvserver starts (avoids restart race).
# Cert is under s3://spawn-certs-<region>/<account-base36>/{cert,key}.pem
ACCOUNT_ID=$(curl -sf -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' http://169.254.169.254/latest/api/token | xargs -I{} curl -sf -H 'X-aws-ec2-metadata-token: {}' http://169.254.169.254/latest/dynamic/instance-identity/document | python3 -c "import sys,json; print(json.load(sys.stdin)['accountId'])" 2>/dev/null || echo "")
if [ -n "$ACCOUNT_ID" ]; then
  ACCOUNT_B36=$(python3 -c "import sys; n=int('$ACCOUNT_ID'); r=''; n=n if n else 0
while n: n,d=divmod(n,36); r=chr(48+d if d<10 else 87+d)+r
print(r or '0')" 2>/dev/null || echo "")
  if [ -n "$ACCOUNT_B36" ]; then
    CERT_BUCKET="spawn-certs-${REGION}"
    DCV_CERT_DIR="/var/lib/dcv/.config/NICE/dcv/private"
    aws s3 cp "s3://${CERT_BUCKET}/${ACCOUNT_B36}/cert.pem" "${DCV_CERT_DIR}/dcv.pem" 2>/dev/null && \
    aws s3 cp "s3://${CERT_BUCKET}/${ACCOUNT_B36}/key.pem"  "${DCV_CERT_DIR}/dcv.key" 2>/dev/null && \
    chown dcv:dcv "${DCV_CERT_DIR}/dcv.pem" "${DCV_CERT_DIR}/dcv.key" && \
    chmod 600 "${DCV_CERT_DIR}/dcv.pem" "${DCV_CERT_DIR}/dcv.key" && \
    systemctl restart dcvserver && sleep 5 && \
    echo "DCV TLS cert installed for *.${ACCOUNT_B36}.spore.host" || \
    echo "DCV TLS cert not available — using self-signed"
  fi
fi

# Start spored (lifecycle daemon — provides DCV token verifier on :8444, idle
# detection, DNS). Install a systemd unit instead of 'spored monitor': that
# subcommand was removed (current spored runs as bare 'spored'), so the old
# nohup line exited immediately and the token verifier never came up (#264).
# Mirrors the canonical unit in pkg/launcher/bootstrap.go. Must be up before DCV
# so the :8444 verifier is listening when DCV initializes.
cat > /etc/systemd/system/spored.service <<'EOFSPORED'
[Unit]
Description=Spawn Agent - Instance self-monitoring
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
Environment=SPORE_DNS_SIGV4=1
ExecStart=/usr/local/bin/spored
Restart=on-failure
RestartSec=10
TimeoutStopSec=30
StandardOutput=journal
StandardError=journal
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
EOFSPORED
systemctl daemon-reload
systemctl enable spored
systemctl start spored
echo "spored started via systemd"

# Start NICE DCV server (cert already configured above)
systemctl enable dcvserver
systemctl start dcvserver

# Wait for DCV to initialize
sleep 15

# The DCV AMI auto-creates a console session (owner: dcv). Close it so we can
# create one with the correct owner (ec2-user) and init command (the application).
dcv close-session console 2>/dev/null || true
sleep 2

# App-specific preparation (e.g. container image pre-pull) before the session starts.
%s
# Create application streaming session owned by ec2-user with the app as init
dcv create-session \
    --type virtual \
    --name %s \
    --owner ec2-user \
    --init %q \
    %s 2>/dev/null || true

echo "DCV session '%s' created for: %s"
`, preCreate, sessionID, initCommand, sessionID, sessionID, label)
	return base64.StdEncoding.EncodeToString([]byte(script))
}

// waitForDCV polls the DCV endpoint until it responds or the timeout is exceeded.
func waitForDCV(ctx context.Context, host string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 5 * time.Second}
	url := fmt.Sprintf("https://%s:8443", host)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		// DCV returns a page even with TLS errors; we just need TCP open
		resp, err := client.Get(url) //nolint:noctx
		if err == nil {
			resp.Body.Close()
			return nil
		}
		// Connection refused → keep waiting; TLS error → DCV is up
		if strings.Contains(err.Error(), "tls") || strings.Contains(err.Error(), "certificate") {
			return nil
		}
		time.Sleep(10 * time.Second)
	}
	return fmt.Errorf("DCV not ready after %v", timeout)
}

// getSessionsDir returns the path to ~/.spawn/sessions/, creating it if necessary.
func getSessionsDir() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home directory: %w", err)
	}
	dir := filepath.Join(homeDir, ".spawn", "sessions")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("create sessions directory: %w", err)
	}
	return dir, nil
}

// findSessionFile scans the sessions directory for an HTML file containing instanceID.
// Returns the file path if found, empty string otherwise.
func findSessionFile(dir, instanceID string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".html") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), instanceID) {
			return path
		}
	}
	return ""
}

// writeSessionHTML writes the DCV session HTML file to ~/.spawn/sessions/ and returns the path.
func writeSessionHTML(instanceID, sessionName, dnsName, publicIP, appName, appDesc, instanceType, authToken string) (string, error) {
	dir, err := getSessionsDir()
	if err != nil {
		return "", err
	}

	// Generate a short random session ID for the file name and tab title
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	shortID := hex.EncodeToString(b)

	host := dnsName
	if publicIP != "" {
		host = publicIP
	}

	data := sessionHTMLData{
		SessionID:    shortID,
		InstanceID:   instanceID,
		AppName:      appName,
		AppDesc:      appDesc,
		InstanceType: instanceType,
		Host:         host,
		DCVPort:      8443,
		LaunchedAt:   time.Now().Format("2006-01-02 15:04 UTC"),
		AuthToken:    authToken,
	}

	tmpl, err := htmpl.New("session").Parse(sessionHTMLTemplate)
	if err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}

	path := filepath.Join(dir, fmt.Sprintf("%s-%s.html", appName, shortID))
	f, err := os.Create(path)
	if err != nil {
		return "", fmt.Errorf("create session file: %w", err)
	}
	defer f.Close()

	if err := tmpl.Execute(f, data); err != nil {
		return "", fmt.Errorf("write session file: %w", err)
	}

	return path, nil
}

type sessionHTMLData struct {
	SessionID    string
	InstanceID   string
	AppName      string
	AppDesc      string
	InstanceType string
	Host         string
	DCVPort      int
	LaunchedAt   string
	AuthToken    string // spawn:ready-token from EC2 tag; empty until #289 is implemented
}

// openBrowser opens a file or URL in the default browser.
func openBrowser(path string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", path)
	case "linux":
		cmd = exec.Command("xdg-open", path)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", path)
	default:
		return fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
	return cmd.Start()
}

// ── session HTML template ─────────────────────────────────────────────────────

// sessionHTMLTemplate is the DCV session launcher page.
// When the instance is reachable: probes DCV, then redirects to DCV's own web client
// with the auth token — DCV's built-in UI handles the connection (no SDK/mixed-content issues).
// When the instance is stopped (idle/TTL): shows "session paused" with a Restart button.
// Title format "spore:<id> — <app> (<instanceID>)" enables tab reuse by spawn connect.
var sessionHTMLTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>spore:{{.SessionID}} — {{.AppName}} ({{.InstanceID}})</title>
  <style>
    * { box-sizing: border-box; margin: 0; padding: 0; }
    body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif;
           background: #0f0f0f; color: #e0e0e0; height: 100vh;
           display: flex; flex-direction: column; align-items: center; justify-content: center; }
    .spinner { width: 48px; height: 48px; border: 4px solid #333;
               border-top: 4px solid #42d4d4; border-radius: 50%;
               animation: spin 1s linear infinite; margin-bottom: 1.5rem; }
    @keyframes spin { to { transform: rotate(360deg); } }
    h2 { font-size: 1.4rem; font-weight: 600; margin-bottom: 0.5rem; }
    p  { color: #888; font-size: 0.9rem; margin-bottom: 1.5rem; }
    .btn { background: #4059e5; color: #fff; border: none; padding: 0.65rem 1.4rem;
           border-radius: 8px; font-size: 0.9rem; font-weight: 600;
           cursor: pointer; text-decoration: none; display: inline-block; }
    .btn:hover { opacity: 0.85; }
    footer { position: fixed; bottom: 0; left: 0; right: 0; padding: 0.4rem 1rem;
             background: rgba(0,0,0,0.6); font-size: 0.75rem; color: #555;
             display: flex; justify-content: space-between; }
  </style>
</head>
<body>
  <div class="spinner" id="spinner"></div>
  <h2 id="title">Connecting to {{.AppName}}…</h2>
  <p id="msg">Checking session at {{.Host}}:{{.DCVPort}}</p>
  <a class="btn" id="action-btn" href="#" style="display:none"></a>

  <footer>
    <span>spore:{{.SessionID}} — {{.AppName}} ({{.InstanceType}}) — launched {{.LaunchedAt}}</span>
    <span>Instance: {{.InstanceID}}</span>
  </footer>

  <script>
  const DCV_HOST  = '{{.Host}}';
  const DCV_PORT  = {{.DCVPort}};
  const AUTH_TOKEN = '{{.AuthToken}}';
  const APP_NAME  = '{{.AppName}}';
  const dcvBase   = 'https://' + DCV_HOST + ':' + DCV_PORT;
  // DCV: sessionId in URL hash (#console), authToken in page query string
  // CSS injected into DCV index.html handles viewport filling
  const dcvURL    = dcvBase + '/?' + (AUTH_TOKEN ? 'authToken=' + AUTH_TOKEN + '&' : '') + 'fullscreen=true#console';

  function showPaused(reason) {
    document.getElementById('spinner').style.display = 'none';
    document.getElementById('title').textContent = 'Session paused';
    document.getElementById('msg').textContent = reason || '{{.AppName}} was stopped due to inactivity.';
    const btn = document.getElementById('action-btn');
    btn.textContent = 'Restart Session';
    btn.style.display = 'inline-block';
    btn.onclick = (e) => {
      e.preventDefault();
      btn.textContent = 'Starting…';
      btn.style.opacity = '0.6';
      document.getElementById('msg').textContent = 'Starting {{.AppName}}…';
      document.getElementById('spinner').style.display = 'block';
      document.getElementById('title').textContent = 'Restarting session';
      // Try spawn:// URL scheme (registered at install time).
      window.location.href = 'spawn://connect/{{.InstanceID}}';
      pollAndConnect();
    };
  }

  // When AUTH_TOKEN is set, spored confirmed DCV is ready — redirect immediately.
  // When reconnecting (no token), probe first to detect if the instance is still up.
  if (AUTH_TOKEN) {
    window.location.href = dcvURL;
  } else {
    tryConnect();
  }

  async function tryConnect() {
    try {
      await fetch(dcvBase + '/favicon.ico', { mode: 'no-cors', signal: AbortSignal.timeout(8000) });
      window.location.href = dcvURL;
    } catch (_) {
      showPaused('{{.AppName}} is not running. Start it again to open a new session.');
    }
  }

  async function pollAndConnect() {
    for (let i = 0; i < 36; i++) {
      try {
        await fetch(dcvBase + '/favicon.ico', { mode: 'no-cors', signal: AbortSignal.timeout(8000) });
        window.location.href = dcvURL;
        return;
      } catch (_) {
        document.getElementById('msg').textContent =
          'Waiting for {{.AppName}} to start… (' + (i+1) + ' of 36)';
        await new Promise(r => setTimeout(r, 10000));
      }
    }
    showPaused('{{.AppName}} did not start in time. Try again or check your session.');
  }
  </script>
</body>
</html>
`
