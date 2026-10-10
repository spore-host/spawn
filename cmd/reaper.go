package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

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

	reaperKeepArtifacts  bool
	reaperForceArtifacts bool
	reaperIfIdleFor      time.Duration
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
  spawn reaper teardown        # remove it

Versioning: the CLI and a deployed reaper are INDEPENDENT. Upgrading spawn does
not touch a reaper already in an account, and 'spawn reaper deploy' installs the
artifact for whichever version it is asked for. So a deployed reaper can be older
than the CLI talking to it — 'spawn reaper status' prints both and says so when
they differ. Nothing breaks when they do; an older reaper still reaps. Bring them
together by re-running 'spawn reaper deploy' (spawn#654).`,
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
	Use:   "teardown",
	Short: "Remove the reaper from this account, including its artifact bucket",
	Long: `Remove the reaper from this account: the schedule, the Lambda, its role, and
the S3 bucket holding its artifact.

The bucket used to be left behind deliberately (#653). Under "leave no trace"
that is wrong — an idle reaper that tidied everything except a bucket has left a
trace while reporting that it has not.

The original concern is answered rather than overruled. The bucket is removed
only when it is tagged spawn:managed=true AND contains nothing outside
ttl-reaper/; a bucket that fails either check is reported and kept, with
--force-artifacts to override. So the liberty is only taken over a bucket that is
provably spawn's and holds only spawn's artifacts.`,
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

	reaperTeardownCmd.Flags().BoolVar(&reaperKeepArtifacts, "keep-artifacts", false,
		"Leave the artifact bucket in place (it is removed by default)")
	reaperTeardownCmd.Flags().DurationVar(&reaperIfIdleFor, "if-idle-for", 0,
		"Only tear down if the reaper has reclaimed nothing for at least this long (e.g. 720h); otherwise report and exit 0")
	reaperTeardownCmd.Flags().BoolVar(&reaperForceArtifacts, "force-artifacts", false,
		"Remove the artifact bucket even if it is not tagged spawn:managed=true (for buckets created before the tag existed)")
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
			return newAborted("aborted: the reaper stays in dry-run")
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
	out := cmd.OutOrStdout()
	fmt.Fprint(out, renderReaperStatus(info, account, region))

	// Idleness last, and only when something is actually deployed: "this reaper
	// has had nothing to do" is meaningless for an account that has none
	// (spawn#772).
	//
	// Best-effort — a log group that cannot be read must not turn a working
	// status report into an error. IdlenessAdvice says "could not tell" rather
	// than implying idleness, which matters because this is the line someone
	// would act on by deleting things.
	if info.Deployed {
		if msg := reaperdeploy.IdlenessAdvice(d.DetectIdleness(ctx, time.Now()), time.Now()); msg != "" {
			fmt.Fprintf(out, "\n%s\n", msg)
		}
	}
	return nil
}

// renderReaperStatus is pure, so the output contract is testable.
func renderReaperStatus(info reaperdeploy.Info, account, region string) string {
	return renderReaperStatusWithCLI(info, account, region, version())
}

// renderReaperStatusWithCLI takes the CLI version explicitly so the skew wording
// is testable without depending on how this binary was built (spawn#654).
func renderReaperStatusWithCLI(info reaperdeploy.Info, account, region, cliVersion string) string {
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
	// Version, plus how it relates to THIS CLI. The two are deployed
	// independently and nothing synchronises them, so skew is the normal state
	// and worth surfacing rather than leaving to be discovered (spawn#654).
	skew := compareReaperVersion(info.Version, cliVersion)
	if info.Version != "" {
		fmt.Fprintf(&b, "  Version:   %s\n", info.Version)
	} else {
		fmt.Fprintf(&b, "  Version:   unknown\n")
	}
	b.WriteString(renderReaperSkew(skew))
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

	// --if-idle-for exists so removal can be SCHEDULED without the Lambda being
	// able to delete things (spawn#772).
	//
	// The obvious reading of "self-remove" is a reaper that deletes itself when
	// idle. That needs lambda:DeleteFunction, iam:DeleteRole, events:DeleteRule
	// and s3:DeleteBucket added to the 11 EC2/FSx/SSM actions it has today — and
	// #613's audit of the in-account reaper is quotable because the policy is
	// minimal, in an account whose organization forbids external trust. Granting a
	// scheduled function iam:DeleteRole to save a human one command is the wrong
	// trade.
	//
	// So the capability lives here, under the caller's own credentials, which
	// already carry those permissions. Put it in a cron if you want it automatic.
	if reaperIfIdleFor > 0 {
		idle := d.DetectIdleness(ctx, time.Now())
		switch {
		case !idle.Determined:
			// Never remove on an inconclusive read. Exit 0: this is a scheduled
			// no-op, not a failure.
			fmt.Fprintf(out, "Not tearing down: could not determine idleness (%s).\n", idle.Why)
			return nil
		case idle.EverWorked:
			fmt.Fprintf(out, "Not tearing down: the reaper reclaimed something at %s.\n",
				idle.LastWorked.UTC().Format(time.RFC3339))
			return nil
		case !idle.Ran:
			// A reaper that has not RUN is a broken schedule, not an unused
			// feature. Removing it here would delete something that never got the
			// chance to work — the opposite of the intent.
			fmt.Fprintf(out, "Not tearing down: the reaper has not run at all, which is a broken "+
				"schedule rather than an idle one. Check `spawn reaper status`.\n")
			return nil
		}
		if got := idle.IdleFor(time.Now()); got < reaperIfIdleFor {
			fmt.Fprintf(out, "Not tearing down: idle for %s, which is less than the %s required.\n",
				got.Round(time.Hour), reaperIfIdleFor)
			return nil
		}
		fmt.Fprintf(out, "Idle for at least %s — tearing down.\n", reaperIfIdleFor)
	}

	removed, err := d.Teardown(ctx, reaperdeploy.TeardownOptions{
		Bucket:         reaperdeploy.DefaultBucketName(account, region),
		KeepArtifacts:  reaperKeepArtifacts,
		ForceArtifacts: reaperForceArtifacts,
	})
	for _, r := range removed {
		fmt.Fprintf(out, "  - %s\n", r)
	}
	if err != nil {
		return err
	}
	if len(removed) == 0 {
		fmt.Fprintf(out, "Nothing to remove in account %s (%s).\n", account, region)
		return nil
	}
	fmt.Fprintf(out, "\nNothing out-of-band reclaims resources in this account now — 'spawn doctor' will say so.\n")
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
