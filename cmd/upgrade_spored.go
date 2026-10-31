package cmd

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spore-host/libs/i18n"
	"github.com/spore-host/libs/update"
	"github.com/spore-host/spawn/internal/shellmin"
	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/launchconfig"
	"github.com/spore-host/spawn/pkg/launcher"
	"github.com/spore-host/spawn/pkg/sporedinstall"
)

var (
	upgradeSporedVersion string
	upgradeSporedForce   bool
	upgradeSporedYes     bool
	upgradeSporedTimeout time.Duration
)

var upgradeSporedCmd = &cobra.Command{
	Use:   "upgrade-spored <instance-id|name>",
	Short: "Upgrade the spored agent on a running instance in place",
	Long: `Replace the spored lifecycle agent on a running instance with a newer
release WITHOUT terminating/relaunching the instance and without losing spored's
lifecycle state (the TTL deadline, accumulated compute-seconds, and the
completion / pre-stop / idle / FSx config live in EC2 tags that the new spored
re-reads on boot, and the settings fixed at launch in the launch config object
cached on the instance).

An upgrade across v0.150.0, the first spored that reads those settings only from
the launch config object, is refused when the instance keeps them in the other
place: tags on an instance launched by an older spawn, or an object an older
spored would not read.

The default target is this spawn's own version, the spored a launch by this
spawn installs (for a development build of spawn, which has no published spored,
the latest release); pin another with --version. A downgrade is refused unless
--allow-downgrade is given. The swap is driven over SSM
(keyless — works on private-subnet / no-public-IP instances), so the instance
must have the SSM agent online and an instance profile (the spored role attaches
AmazonSSMManagedInstanceCore, so spawn-launched instances already qualify).

The TTL deadline is absolute and tag-stored, so it is NOT reset by the restart —
an instance mid-life keeps its original termination time. Linux only for now
(Windows spored upgrade is a follow-up, #234).`,
	Args:              cobra.ExactArgs(1),
	RunE:              runUpgradeSpored,
	ValidArgsFunction: completeInstanceID,
}

func init() {
	rootCmd.AddCommand(upgradeSporedCmd)
	f := upgradeSporedCmd.Flags()
	f.StringVar(&upgradeSporedVersion, "version", "", "Target spored version (e.g. 0.64.0); default: this spawn's version (latest release for a dev build)")
	// --allow-downgrade replaces the old --force, whose "force" wording collided
	// with cleanup's execute-vs-preview meaning; here it only permits a downgrade
	// (#315). --force kept as a hidden deprecated alias bound to the same var.
	f.BoolVar(&upgradeSporedForce, "allow-downgrade", false, "Allow a downgrade (target older than the running version)")
	f.BoolVar(&upgradeSporedForce, "force", false, "Deprecated alias for --allow-downgrade")
	_ = f.MarkDeprecated("force", "use --allow-downgrade")
	f.BoolVarP(&upgradeSporedYes, "yes", "y", false, "Skip the confirmation prompt")
	f.DurationVar(&upgradeSporedTimeout, "timeout", 5*time.Minute, "How long to wait for the on-instance upgrade to complete")
}

func runUpgradeSpored(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	out := cmd.OutOrStdout()

	client, err := aws.NewClient(ctx)
	if err != nil {
		return i18n.Te("error.aws_client_init", err)
	}

	instance, err := resolveInstance(ctx, client, args[0])
	if err != nil {
		return err
	}
	if instance.State != "running" {
		return fmt.Errorf("instance %s is %s; spored can only be upgraded on a running instance", instance.InstanceID, instance.State)
	}

	target, err := upgradeSporedTarget(upgradeSporedVersion, launcher.SporedVersion())
	if err != nil {
		return err
	}
	if target == "" {
		res := update.CheckNow("spawn", version())
		if res == nil {
			return fmt.Errorf("could not determine the latest spored version (GitHub unreachable); pin one with --version")
		}
		target = res.LatestVersion
	}

	// Where this instance's launch-fixed settings are, and whether the target
	// spored reads them there (#1359).
	if err := launchConfigUpgradeGuard(instance.Tags, target); err != nil {
		return err
	}

	// What's running now? Prefer the tag (no exec); it's written by spored on boot
	// (#234). Empty when an older spored that predates the tag is running — then we
	// can't compare, so we proceed (and --allow-downgrade is not required).
	running := instance.Tags["spawn:spored-version"]
	if running != "" && !upgradeSporedForce {
		if cmp := compareVersions(target, running); cmp == 0 {
			fmt.Fprintf(out, "%s spored is already at v%s on %s — nothing to do.\n", i18n.Symbol("success"), target, instance.InstanceID)
			return nil
		} else if cmp < 0 {
			if upgradeSporedVersion == "" {
				return fmt.Errorf("this spawn is v%s, older than the running spored v%s, and upgrades to its own version by default — "+
					"refusing a downgrade (upgrade spawn, pass --version, or --allow-downgrade)", target, running)
			}
			return fmt.Errorf("target v%s is older than the running v%s — refusing a downgrade (pass --force to override)", target, running)
		}
	}

	runningLabel := running
	if runningLabel == "" {
		runningLabel = "unknown"
	}
	fmt.Fprintf(out, "Upgrade spored on %s: v%s → v%s\n", instance.InstanceID, runningLabel, target)
	if !confirmYes(upgradeSporedYes, "Proceed with the in-place upgrade?") {
		return fmt.Errorf("aborted")
	}

	script := buildSporedUpgradeScript(target)

	fmt.Fprintf(os.Stderr, "%s upgrading spored over SSM (this stops + restarts the agent)...\n", i18n.Symbol("info"))
	res, err := client.RunShellScript(ctx, instance.Region, instance.InstanceID, script, upgradeSporedTimeout)
	if err != nil {
		return fmt.Errorf("upgrade command did not complete: %w", err)
	}
	if res.Status != "Success" {
		return fmt.Errorf("upgrade failed (%s):\n%s\n%s", res.Status, strings.TrimSpace(res.Stdout), strings.TrimSpace(res.Stderr))
	}

	// The script prints the new version on success; surface it.
	fmt.Fprint(out, res.Stdout)

	// Record the new version in the tag so status reflects it immediately without
	// waiting for the freshly-restarted spored to rewrite it.
	if err := client.UpdateInstanceTags(ctx, instance.Region, instance.InstanceID, map[string]string{
		"spawn:spored-version": target,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "%s upgraded, but could not update the spawn:spored-version tag: %v\n", i18n.Symbol("warning"), err)
	}

	fmt.Fprintf(out, "%s spored upgraded to v%s on %s (lifecycle state preserved).\n", i18n.Symbol("success"), target, instance.InstanceID)
	return nil
}

// launchConfigUpgradeGuard refuses an upgrade that would leave an instance's
// launch-fixed spored settings (#1359: notifications, webhooks, active ports
// and processes, the web app, idle CPU, metrics and tracing details) where the
// target spored does not read them:
//
//   - a spored from launchconfig.FirstSporedVersion reads them only from the
//     launch config object spawn:config names, never from tags, so it is
//     refused on an instance that has them as tags and no spawn:config (one
//     launched by an older spawn). Reading the tags after all would be the
//     tag fallback the same-version contract rules out, and running without
//     them would drop its notifications and webhooks and, without its
//     --active-ports/--active-processes, could stop an instance in use as
//     idle;
//   - an older spored reads them only from tags, so it is refused on an
//     instance that names an object.
//
// An instance with none of these settings has nothing to lose either way.
func launchConfigUpgradeGuard(tags map[string]string, target string) error {
	readsObject := compareVersions(target, launchconfig.FirstSporedVersion) >= 0
	_, hasObject := tags[launchconfig.Tag]
	var asTags []string
	for k := range tags {
		if launchconfig.IsSetting(k) {
			asTags = append(asTags, k)
		}
	}
	sort.Strings(asTags)
	switch {
	case readsObject && !hasObject && len(asTags) > 0:
		pin, why := tagsReadingSpored(tags)
		return fmt.Errorf("refusing to install spored v%s: this instance was launched by a spawn older than v%s and "+
			"carries its spored settings as tags (%s); spored v%s and later read them only from a launch config object, "+
			"which this instance does not have, so they would stop applying. Pin a spored that reads tags: "+
			"--version %s (%s; #1359)", target, launchconfig.FirstSporedVersion, strings.Join(asTags, ", "),
			launchconfig.FirstSporedVersion, pin, why)
	case !readsObject && hasObject:
		return fmt.Errorf("refusing to install spored v%s: this instance's spored settings are in a launch config object "+
			"(%s), which spored before v%s does not read, so they would stop applying (#1359)",
			target, launchconfig.Tag, launchconfig.FirstSporedVersion)
	}
	return nil
}

// tagsReadingSpored names the spored launchConfigUpgradeGuard tells a
// tags-only instance to pin, and why (#1423). spawn:version is the launching
// spawn's version only when spawn itself launched the instance
// (spawn:created-by=spawn): spored is versioned in lockstep with spawn, so that
// is the spored it installed. Another launcher writes its own version there
// (spawn-ts writes created-by=spawn-ts and its library version, e.g. 0.7.0,
// for which no spored exists), so it is not read for them. Otherwise the
// spored the instance reports running (spawn:spored-version), and failing
// that the frozen latest, the last spored that reads the settings from tags
// (#1419). A version that is not a release, or that reads only the launch
// config object, is skipped.
func tagsReadingSpored(tags map[string]string) (version, why string) {
	readsTags := func(v string) string {
		v = sporedinstall.ReleaseVersion(v)
		if v == "" || compareVersions(v, launchconfig.FirstSporedVersion) >= 0 {
			return ""
		}
		return v
	}
	if tags["spawn:created-by"] == "spawn" {
		if v := readsTags(tags["spawn:version"]); v != "" {
			return v, "the spored of the spawn that launched it"
		}
	}
	if v := readsTags(tags["spawn:spored-version"]); v != "" {
		return v, "the spored it reports running"
	}
	return sporedinstall.FrozenLatestVersion, "the last spored that reads them from tags"
}

// upgradeSporedTarget resolves the version to upgrade to: --version when given,
// else this spawn's own release (#1412). spored is versioned in lockstep with
// spawn (same release tag, see .goreleaser.yaml), and a launch by this spawn
// installs exactly that spored, so the default upgrade brings an instance to the
// pair this spawn ships rather than to whatever was released last, which this
// spawn was never tested with. "" (a dev build, no --version) means the caller
// asks GitHub for the latest release, as before.
//
// --version must be a release (MAJOR.MINOR.PATCH): it names the
// spawn/versions/<v>/ objects the script installs, and is rendered into it.
func upgradeSporedTarget(flag, own string) (string, error) {
	if flag == "" {
		return own, nil
	}
	v := sporedinstall.ReleaseVersion(flag)
	if v == "" {
		return "", fmt.Errorf("--version %q is not a spawn release (want MAJOR.MINOR.PATCH, e.g. 0.64.0)", flag)
	}
	return v, nil
}

// buildSporedUpgradeScript builds the bash script run on the instance over SSM to
// swap spored in place, state-preservingly. It mirrors the install logic in
// pkg/launcher/bootstrap.go (regional bucket with us-east-1 fallback, SHA256
// verify, atomic rename over the busy binary) but pulls the VERSIONED artifact
// (spawn/versions/<v>/spored-linux-<arch>) so the upgrade is deterministic.
//
// State preservation: before stopping the daemon it asks the running spored to
// flush its accumulated compute-seconds to the tag (`spored config` has no such
// hook, so we rely on the graceful-stop flush added in agent.Cleanup, #234 — a
// `systemctl stop` triggers SIGTERM → Cleanup → flushComputeSecondsTag). After
// the swap it restarts spored and health-checks that the daemon is active and
// reports the target version, rolling back to the prior binary if not.
//
// Pulled out as a pure function so the (correctness-sensitive) script is unit
// tested without an SSM round-trip. version must be a bare semver (no leading v).
func buildSporedUpgradeScript(version string) string {
	// version comes from a release tag / --version and is validated by the
	// caller's semver comparison; it only ever contains [0-9.]+ plus an optional
	// pre-release suffix, none of which can break out of the shell context below.
	return fmt.Sprintf(`set -euo pipefail
TARGET_VERSION=%[1]q

ARCH=$(uname -m)
case "$ARCH" in
  x86_64)  BIN="spored-linux-amd64" ;;
  aarch64) BIN="spored-linux-arm64" ;;
  *) echo "Unsupported architecture: $ARCH" >&2; exit 1 ;;
esac

# Region from IMDSv2 (fall back to us-east-1).
TOKEN=$(curl -sX PUT "http://169.254.169.254/latest/api/token" -H "X-aws-ec2-metadata-token-ttl-seconds: 60" 2>/dev/null || true)
if [ -n "$TOKEN" ]; then
  REGION=$(curl -s -H "X-aws-ec2-metadata-token: $TOKEN" http://169.254.169.254/latest/meta-data/placement/region 2>/dev/null || echo us-east-1)
else
  REGION=$(curl -s http://169.254.169.254/latest/meta-data/placement/region 2>/dev/null || echo us-east-1)
fi

%[2]s
# Back up the current binary BEFORE the install, so the health-check rollback
# below has something to go back to. The installer replaces the binary itself.
BACKUP=$(mktemp /tmp/spored-backup.XXXXXX)
cp -f /usr/local/bin/spored "$BACKUP" 2>/dev/null || true

# Download, verify and install via the one shared implementation. This block
# previously verified the sha256 only, and FAIL-OPEN: a missing .sha256 printed
# "WARNING: no checksum published; skipping verification" to stderr and installed
# anyway. A missing checksum is not a reason to skip verification — an attacker
# who can rewrite the bucket can equally delete a checksum object, so
# "verify if one happens to be published" is a check the attacker opts you out
# of. There was no signature verification at all, though the boot path had had it
# since spore-host#440 (GHSA-3x4q-99gh-fxhx, #819).
#
# Stopping spored gracefully matters here: SIGTERM lets spored's Cleanup flush
# compute-seconds to the tag (#234) so the compute clock survives the swap. The
# installer does that stop itself, then installs by atomic rename.
# Only spawn/versions/${TARGET_VERSION}/, regional then us-east-1 (#1412).
if ! spore_install_spored_from_buckets "$BIN" "https://spawn-binaries-${REGION}.s3.${REGION}.amazonaws.com" "https://spawn-binaries-us-east-1.s3.us-east-1.amazonaws.com"; then
  echo "Failed to install a verified spored v${TARGET_VERSION} (${BIN})" >&2
  rm -f "$BACKUP"
  exit 1
fi
systemctl daemon-reload 2>/dev/null || true
systemctl start spored

# Health check: daemon active AND reporting the target version. Roll back if not.
sleep 3
NEW_VERSION=$(/usr/local/bin/spored version 2>/dev/null | awk '{print $NF}')
if ! systemctl is-active --quiet spored || [ "$NEW_VERSION" != "$TARGET_VERSION" ]; then
  echo "Health check failed (active=$(systemctl is-active spored 2>/dev/null), version=${NEW_VERSION:-none}); rolling back" >&2
  systemctl stop spored 2>/dev/null || true
  # The backup lives under /tmp; mv keeps its label, so relabel or an enforcing
  # host cannot exec the rolled-back binary either (#1052).
  if [ -s "$BACKUP" ]; then mv -f "$BACKUP" /usr/local/bin/spored && spore_selinux_relabel /usr/local/bin/spored; fi
  systemctl start spored 2>/dev/null || true
  exit 1
fi
rm -f "$BACKUP"
echo "spored upgraded to v${NEW_VERSION}"
`, version, shellmin.Strip(sporedinstall.KeySetupScript())+sporedinstall.PinScript(version)+
		shellmin.Strip(sporedinstall.VerifiedInstallFunc()))
}

// compareVersions returns -1 if a < b, 0 if equal, 1 if a > b, comparing bare
// semver (major.minor.patch; pre-release suffixes are ignored on each segment).
// Used to refuse downgrades. Mirrors libs/update's semver comparison so the CLI
// doesn't need to export it.
func compareVersions(a, b string) int {
	pa, pb := parseSemverTriple(a), parseSemverTriple(b)
	for i := 0; i < 3; i++ {
		if pa[i] > pb[i] {
			return 1
		}
		if pa[i] < pb[i] {
			return -1
		}
	}
	return 0
}

func parseSemverTriple(v string) [3]int {
	v = strings.TrimPrefix(v, "v")
	parts := strings.SplitN(v, ".", 3)
	var out [3]int
	for i := 0; i < len(parts) && i < 3; i++ {
		num := strings.SplitN(parts[i], "-", 2)[0]
		n := 0
		for _, r := range num {
			if r < '0' || r > '9' {
				break
			}
			n = n*10 + int(r-'0')
		}
		out[i] = n
	}
	return out
}
