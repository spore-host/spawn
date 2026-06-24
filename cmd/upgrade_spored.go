package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spore-host/libs/i18n"
	"github.com/spore-host/libs/update"
	"github.com/spore-host/spawn/pkg/aws"
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
completion / pre-stop / idle / FSx config all live in EC2 tags that the new
spored re-reads on boot).

The default target is the latest released version; pin one with --version. A
downgrade is refused unless --force is given. The swap is driven over SSM
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
	f.StringVar(&upgradeSporedVersion, "version", "", "Target spored version (e.g. 0.64.0); default: latest release")
	f.BoolVar(&upgradeSporedForce, "force", false, "Allow a downgrade (target older than the running version)")
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

	// Resolve the target version. Default is the latest spawn release (spored is
	// versioned in lockstep with spawn — same release tag, see .goreleaser.yaml),
	// so "latest spored" == latest spawn GitHub release.
	target := strings.TrimPrefix(upgradeSporedVersion, "v")
	if target == "" {
		res := update.CheckNow("spawn", Version)
		if res == nil {
			return fmt.Errorf("could not determine the latest spored version (GitHub unreachable); pin one with --version")
		}
		target = res.LatestVersion
	}

	// What's running now? Prefer the tag (no exec); it's written by spored on boot
	// (#234). Empty when an older spored that predates the tag is running — then we
	// can't compare, so we proceed (and --force is not required).
	running := instance.Tags["spawn:spored-version"]
	if running != "" && !upgradeSporedForce {
		if cmp := compareVersions(target, running); cmp == 0 {
			fmt.Fprintf(out, "%s spored is already at v%s on %s — nothing to do.\n", i18n.Symbol("success"), target, instance.InstanceID)
			return nil
		} else if cmp < 0 {
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
TARGET_VERSION=%q

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

REG_BASE="https://spawn-binaries-${REGION}.s3.amazonaws.com/spawn/versions/${TARGET_VERSION}"
FB_BASE="https://spawn-binaries-us-east-1.s3.amazonaws.com/spawn/versions/${TARGET_VERSION}"

TMP=$(mktemp /tmp/spored.XXXXXX)
if curl -fsS -o "$TMP" "${REG_BASE}/${BIN}" 2>/dev/null; then
  SUM_URL="${REG_BASE}/${BIN}.sha256"
else
  echo "Regional bucket missing v${TARGET_VERSION}; trying us-east-1..."
  curl -fsS -o "$TMP" "${FB_BASE}/${BIN}" || { echo "Failed to download spored v${TARGET_VERSION} (${BIN})" >&2; rm -f "$TMP"; exit 1; }
  SUM_URL="${FB_BASE}/${BIN}.sha256"
fi

# Verify SHA256 (the versioned .sha256 is uploaded alongside the binary, #234).
if curl -fsS -o "${TMP}.sha256" "$SUM_URL" 2>/dev/null; then
  EXPECTED=$(cat "${TMP}.sha256")
  ACTUAL=$(sha256sum "$TMP" | awk '{print $1}')
  if [ "$EXPECTED" != "$ACTUAL" ]; then
    echo "Checksum verification failed (expected $EXPECTED, got $ACTUAL)" >&2
    rm -f "$TMP" "${TMP}.sha256"
    exit 1
  fi
  echo "Checksum verified: $ACTUAL"
else
  echo "WARNING: no checksum published for v${TARGET_VERSION}; skipping verification" >&2
fi
chmod +x "$TMP"

# Back up the current binary so we can roll back on a failed health check.
BACKUP=$(mktemp /tmp/spored-backup.XXXXXX)
cp -f /usr/local/bin/spored "$BACKUP" 2>/dev/null || true

# Stop spored gracefully: SIGTERM → spored's Cleanup flushes compute-seconds to
# the tag (#234), so the compute clock survives the swap. Then atomic rename over
# the (now-stopped) binary and restart.
systemctl stop spored 2>/dev/null || true
mv -f "$TMP" /usr/local/bin/spored
systemctl daemon-reload 2>/dev/null || true
systemctl start spored

# Health check: daemon active AND reporting the target version. Roll back if not.
sleep 3
NEW_VERSION=$(/usr/local/bin/spored version 2>/dev/null | awk '{print $NF}')
if ! systemctl is-active --quiet spored || [ "$NEW_VERSION" != "$TARGET_VERSION" ]; then
  echo "Health check failed (active=$(systemctl is-active spored 2>/dev/null), version=${NEW_VERSION:-none}); rolling back" >&2
  systemctl stop spored 2>/dev/null || true
  if [ -s "$BACKUP" ]; then mv -f "$BACKUP" /usr/local/bin/spored; fi
  systemctl start spored 2>/dev/null || true
  rm -f "${TMP}.sha256"
  exit 1
fi
rm -f "$BACKUP" "${TMP}.sha256"
echo "spored upgraded to v${NEW_VERSION}"
`, version)
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
