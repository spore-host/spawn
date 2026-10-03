package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/reaperdeploy"
)

var (
	reaperRegion   string
	reaperVersion  string
	reaperArtifact string
	reaperBucket   string
	reaperSchedule string
	reaperRegions  string
	reaperYes      bool
)

var reaperCmd = &cobra.Command{
	Use:   "reaper",
	Short: "Manage the out-of-band TTL reaper in your own AWS account",
	Long: `Run spawn's TTL reaper inside your own AWS account.

The reaper is the backstop for what spored cannot do. spored enforces TTL, idle and
cost from INSIDE each instance — so it cannot act on an instance that is stopped, it
cannot delete a filesystem that outlives its instance, and it enforces nothing at all
if it dies. "Everything dies eventually" holds in two layers, and this is the second.

spawn's own deployment of the reaper lives in the spore.host infra account and reaches
into each launching account by assuming a role there. That requires your account to
trust an external principal, which many organizations forbid outright — so for those
accounts the reaper was simply unavailable, and nothing reclaimed a stopped instance
past its TTL or an orphaned filesystem.

These commands deploy the same reaper in your account, scanning only your account. It
assumes nothing and trusts nothing but Lambda.

It deploys UNARMED (dry-run): the schedule runs and logs what it WOULD reclaim, and
touches nothing, until you run 'spawn reaper arm'. Read a cycle of those logs first —
the reaper terminates instances, and that is not reversible.

  spawn doctor                 # does anything cover this account today?
  spawn reaper deploy          # install it, unarmed
  spawn reaper status          # deployed? armed? on what schedule?
  spawn reaper arm             # start actually reclaiming
  spawn reaper teardown        # remove it`,
	SilenceUsage: true,
}

var reaperDeployCmd = &cobra.Command{
	Use:   "deploy",
	Short: "Deploy the TTL reaper into this AWS account (unarmed)",
	Long: `Create the reaper's execution role, upload its Lambda artifact to a bucket in
this account, create the function, and put it on a schedule.

Deploys with dry-run ON. Nothing is reclaimed until 'spawn reaper arm'.

The artifact comes from the spawn GitHub Release matching --version (default: this
binary's version). --artifact overrides that with a local path or an explicit URL, for
mirrored or air-gapped environments.`,
	SilenceUsage: true,
	RunE:         runReaperDeploy,
}

var reaperArmCmd = &cobra.Command{
	Use:          "arm",
	Short:        "Let the deployed reaper actually terminate expired instances",
	SilenceUsage: true,
	RunE:         runReaperArm,
}

var reaperStatusCmd = &cobra.Command{
	Use:          "status",
	Short:        "Report whether the reaper is deployed here, and whether it is armed",
	SilenceUsage: true,
	RunE:         runReaperStatus,
}

var reaperTeardownCmd = &cobra.Command{
	Use:          "teardown",
	Short:        "Remove the reaper from this account",
	SilenceUsage: true,
	RunE:         runReaperTeardown,
}

func init() {
	rootCmd.AddCommand(reaperCmd)
	reaperCmd.AddCommand(reaperDeployCmd, reaperArmCmd, reaperStatusCmd, reaperTeardownCmd)

	reaperCmd.PersistentFlags().StringVar(&reaperRegion, "region", "", "AWS region to operate in (default: resolved region)")

	reaperDeployCmd.Flags().StringVar(&reaperVersion, "version", "", "spawn release to take the reaper artifact from (default: this binary's version)")
	reaperDeployCmd.Flags().StringVar(&reaperArtifact, "artifact", "", "Use this Lambda zip instead of downloading a release asset (local path or URL)")
	reaperDeployCmd.Flags().StringVar(&reaperBucket, "bucket", "", "S3 bucket in THIS account to hold the artifact (default: spawn-reaper-artifacts-<account>-<region>)")
	reaperDeployCmd.Flags().StringVar(&reaperSchedule, "schedule", reaperdeploy.DefaultSchedule, "EventBridge schedule expression")
	reaperDeployCmd.Flags().StringVar(&reaperRegions, "regions", "", "Comma-separated regions for the reaper to scan (default: the deploy region)")

	reaperArmCmd.Flags().BoolVar(&reaperYes, "yes", false, "Skip the confirmation prompt")
}

// newReaperDeployer builds a Deployer and resolves account/region. Split out so each
// command shares one construction path.
func newReaperDeployer(ctx context.Context) (*reaperdeploy.Deployer, string, string, error) {
	client, err := spawnaws.NewClientWithRegion(ctx, reaperRegion)
	if err != nil {
		return nil, "", "", fmt.Errorf("initialize AWS client: %w", err)
	}
	region := client.Config().Region
	if region == "" {
		return nil, "", "", fmt.Errorf("no region resolved; pass --region or set AWS_REGION")
	}
	accountID, _, err := client.GetCallerIdentityInfo(ctx)
	if err != nil {
		return nil, "", "", fmt.Errorf("resolve account: %w", err)
	}
	return reaperdeploy.New(client.Config()), accountID, region, nil
}

func runReaperDeploy(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	d, account, region, err := newReaperDeployer(ctx)
	if err != nil {
		return err
	}

	ver := reaperVersion
	if ver == "" {
		ver = version()
	}
	regions := reaperRegions
	if regions == "" {
		regions = region
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Deploying the TTL reaper into account %s (%s)\n", account, region)

	res, err := d.Deploy(ctx, reaperdeploy.Options{
		AccountID: account,
		Region:    region,
		Version:   ver,
		Artifact:  reaperArtifact,
		Bucket:    reaperBucket,
		Schedule:  reaperSchedule,
		Regions:   regions,
	})
	if err != nil {
		return err
	}
	for _, a := range res.Actions {
		fmt.Fprintf(out, "  + %s\n", a)
	}

	fmt.Fprintf(out, "\n⚠️  DRY RUN: it will log what it WOULD reclaim and touch nothing.\n")
	fmt.Fprintf(out, "   Read a cycle, then arm it:\n")
	fmt.Fprintf(out, "     aws logs tail /aws/lambda/%s --follow --region %s\n", reaperdeploy.FunctionName, region)
	fmt.Fprintf(out, "     spawn reaper arm\n")
	fmt.Fprintf(out, "\n   'spawn doctor' should now report this account as covered.\n")
	return nil
}

func runReaperArm(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	d, account, region, err := newReaperDeployer(ctx)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()

	if !reaperYes {
		fmt.Fprintf(out, "%s\n\n", reaperdeploy.ArmWarning(account, region))
		if !confirmReaperArm(os.Stdin, out) {
			fmt.Fprintln(out, "Aborted; the reaper stays in dry-run.")
			return nil
		}
	}
	if err := d.Arm(ctx); err != nil {
		return err
	}
	fmt.Fprintf(out, "Armed. The reaper will reclaim expired spawn-managed resources in %s from its next run.\n", account)
	return nil
}

func runReaperStatus(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	d, account, region, err := newReaperDeployer(ctx)
	if err != nil {
		return err
	}
	info, err := d.Inspect(ctx)
	if err != nil {
		return err
	}
	fmt.Fprint(cmd.OutOrStdout(), renderReaperStatus(info, account, region))
	return nil
}

// renderReaperStatus is pure, so the output contract is testable.
func renderReaperStatus(info reaperdeploy.Info, account, region string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "TTL reaper in account %s (%s)\n", account, region)
	if !info.Deployed {
		fmt.Fprintf(&b, "  NOT DEPLOYED — nothing out-of-band reclaims expired instances or orphaned\n")
		fmt.Fprintf(&b, "  filesystems here. spored still enforces TTL from inside each instance, but it\n")
		fmt.Fprintf(&b, "  cannot act on a stopped instance and does nothing if it dies.\n")
		fmt.Fprintf(&b, "    spawn reaper deploy\n")
		return b.String()
	}
	fmt.Fprintf(&b, "  Function:  %s\n", reaperdeploy.FunctionName)
	if info.Version != "" {
		fmt.Fprintf(&b, "  Version:   %s\n", info.Version)
	}
	if info.Armed {
		fmt.Fprintf(&b, "  State:     ARMED — it reclaims expired spawn-managed resources\n")
	} else {
		fmt.Fprintf(&b, "  State:     DRY RUN — it logs what it would reclaim and touches nothing\n")
		fmt.Fprintf(&b, "               arm it with: spawn reaper arm\n")
	}
	if info.Schedule != "" {
		state := "enabled"
		if !info.RuleEnabled {
			// A disabled rule means the reaper never runs, armed or not — worth
			// saying, because "armed" alone would read as protected.
			state = "DISABLED — it will not run"
		}
		fmt.Fprintf(&b, "  Schedule:  %s (%s)\n", info.Schedule, state)
	} else {
		fmt.Fprintf(&b, "  Schedule:  none found — the function exists but nothing invokes it\n")
	}
	if info.Regions != "" {
		fmt.Fprintf(&b, "  Scanning:  %s\n", info.Regions)
	}
	return b.String()
}

func runReaperTeardown(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	d, account, region, err := newReaperDeployer(ctx)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()

	removed, err := d.Teardown(ctx)
	for _, r := range removed {
		fmt.Fprintf(out, "  - removed %s\n", r)
	}
	if err != nil {
		return err
	}
	if len(removed) == 0 {
		fmt.Fprintf(out, "Nothing to remove in account %s (%s).\n", account, region)
		return nil
	}
	fmt.Fprintf(out, "\nThe artifact bucket was left in place (it may hold other objects); remove it by hand if you want it gone.\n")
	fmt.Fprintf(out, "Nothing out-of-band reclaims resources in this account now — 'spawn doctor' will say so.\n")
	return nil
}

// confirmReaperArm requires an explicit "yes". Arming makes a scheduled process
// start terminating instances; a bare Enter must not do that.
func confirmReaperArm(in io.Reader, out io.Writer) bool {
	fmt.Fprint(out, "Type 'yes' to arm: ")
	var answer string
	if _, err := fmt.Fscanln(in, &answer); err != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(answer), "yes")
}
