package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	sdkaws "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/spf13/cobra"

	"github.com/spore-host/spawn/pkg/aws"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
	"github.com/spore-host/spawn/pkg/orgaccount"
	"github.com/spore-host/spawn/pkg/sweep"
)

// SpawnSweepCrossAccountRole, provisioned by spawn (#1537).
//
// The sweep orchestrator launches, tags and cancels every member of a
// detached sweep through this role in the account the members launch in (the
// compute account, #1538). It used to be made only by
// scripts/setup-sweep-cross-account-role.sh, by hand, per account; a detached
// sweep without it was refused (#1535). Now a detached sweep creates it on
// first use and converges its trust and policy on every use after
// (aws.EnsureSweepCrossAccountRole), `spawn sweep setup` does the same on its
// own, and `spawn sweep teardown` deletes it.
//
// Before anything is created, aws.CheckSweepRoleAccounts refuses the role in,
// or trusting, 942542972736 or an Organizations management account.

// sweepCrossAccountRoleName is the role the sweep orchestrator assumes in the
// account the members launch in.
const sweepCrossAccountRoleName = sweep.CrossAccountRoleName

// sweepRoleIAM builds the IAM client the sweep role is ensured, inspected and
// torn down with, with the credentials of the account the members launch in.
// A var so the hermetic tests can see what reaches it.
var sweepRoleIAM = aws.NewSweepRoleIAM

// sweepRoleManagementAccount is the management account of the compute
// account's Organization ("" when it is in none), with the compute
// credentials. A var so tests need no Organizations endpoint.
var sweepRoleManagementAccount = func(ctx context.Context, cfg sdkaws.Config) (string, error) {
	return orgaccount.ManagementAccount(ctx, orgaccount.New(cfg))
}

// sweepOrchestratorAccount is the account whose sweep orchestrator the CLI
// invokes: the infra credentials' account (spawnconfig.LoadInfraAWSConfig,
// as sweep.InvokeSweepOrchestrator uses them). Its SpawnSweepOrchestratorRole
// is the principal the role trusts.
var sweepOrchestratorAccount = func(ctx context.Context) (string, error) {
	cfg, err := spawnconfig.LoadInfraAWSConfig(ctx, "us-east-1")
	if err != nil {
		return "", err
	}
	return aws.NewClientFromConfig(cfg).GetAccountID(ctx)
}

// activeSweepsInAccount lists the sweeps the orchestrator may still act for
// in account, from the orchestration table, with the infra credentials.
var activeSweepsInAccount = func(ctx context.Context, account string) ([]string, error) {
	cfg, err := spawnconfig.LoadInfraAWSConfig(ctx, "us-east-1")
	if err != nil {
		return nil, err
	}
	recs, err := sweep.ActiveSweepsInAccount(ctx, cfg, account)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, fmt.Sprintf("%s (%s)", r.SweepID, r.Status))
	}
	return out, nil
}

// loadSweepRoleAccount is the compute config, with a region IAM can sign for,
// and its account.
func loadSweepRoleAccount(ctx context.Context) (sdkaws.Config, string, error) {
	cfg, err := spawnconfig.LoadComputeAWSConfig(ctx, "")
	if err != nil {
		return sdkaws.Config{}, "", fmt.Errorf("failed to load AWS config: %w", err)
	}
	accountID, err := aws.NewClientFromConfig(cfg).GetAccountID(ctx)
	if err != nil {
		return sdkaws.Config{}, "", fmt.Errorf("failed to get AWS account ID: %w", err)
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1" // IAM is global; the SDK still wants a region to sign for
	}
	return cfg, accountID, nil
}

// detachedSweepRefusal is the error a detached sweep stops with when the role
// cannot be had, before anything is created.
func detachedSweepRefusal(accountID, why string) error {
	return fmt.Errorf("a detached sweep needs %s in account %s, the role the sweep orchestrator launches every "+
		"member through: %s. Nothing was created. Run the sweep with --no-detach to launch the members from "+
		"this CLI instead", sweepCrossAccountRoleName, accountID, why)
}

// ensureSweepRole checks the account boundary and then ensures the role in
// accountID (cfg's account), trusting the orchestrator spawn invokes. It is
// the one path that creates the role, for a detached sweep and for `spawn
// sweep setup` alike.
//
// strict is `spawn sweep setup`: every failure is an error. A detached sweep
// is not strict where the role is there already, or IAM will not say: a
// role it may not converge, or may not look at, is the role the sweep used
// before #1537, and the sweep goes on with a warning, as it did then.
func ensureSweepRole(ctx context.Context, cfg sdkaws.Config, accountID string, strict bool, out io.Writer) error {
	warn := func(msg string) { fmt.Fprintf(os.Stderr, "⚠️  %s\n", msg) }
	refuse := func(why string) error {
		if strict {
			return fmt.Errorf("%s in account %s: %s", sweepCrossAccountRoleName, accountID, why)
		}
		return detachedSweepRefusal(accountID, why)
	}

	orchestrator, err := sweepOrchestratorAccount(ctx)
	if err != nil {
		return refuse(fmt.Sprintf("spawn could not resolve the account of the sweep orchestrator the role must trust (%v)", err))
	}
	mgmt := func(ctx context.Context) (string, error) { return sweepRoleManagementAccount(ctx, cfg) }
	if err := aws.CheckSweepRoleAccounts(ctx, accountID, orchestrator, mgmt, warn); err != nil {
		return refuse("spawn will not create it: " + err.Error())
	}

	res, err := aws.EnsureSweepCrossAccountRole(ctx, sweepRoleIAM(cfg), accountID, orchestrator, warn)
	switch {
	case err == nil:
	case strict:
		return fmt.Errorf("%s in account %s: %w", sweepCrossAccountRoleName, accountID, err)
	case errors.Is(err, aws.ErrSweepRoleUnknown):
		warn(fmt.Sprintf("could not confirm %s exists in account %s (%v); the sweep orchestrator needs it to launch the members",
			sweepCrossAccountRoleName, accountID, err))
		return nil
	case res.Found:
		warn(fmt.Sprintf("%s exists in account %s, but spawn could not bring it up to date (%v). The sweep goes on "+
			"with the role as it is; members needing a grant it lacks fail at launch. `spawn sweep setup` with "+
			"credentials that may manage the role updates it", sweepCrossAccountRoleName, accountID, err))
		return nil
	default:
		return refuse(fmt.Sprintf("the account has none, and spawn could not create it (%v)", err))
	}

	switch {
	case res.Created:
		fmt.Fprintf(out, "🔐 Created %s in account %s, trusted by %s: the sweep orchestrator launches the members "+
			"through it. `spawn sweep teardown` removes it.\n", sweepCrossAccountRoleName, accountID, aws.SweepOrchestratorRoleARN(orchestrator))
	case res.TrustUpdated:
		fmt.Fprintf(out, "🔐 %s in account %s now trusts only %s (it trusted: %s)\n", sweepCrossAccountRoleName, accountID,
			aws.SweepOrchestratorRoleARN(orchestrator), oneLineJSON(res.PreviousTrust))
	}
	return nil
}

// oneLineJSON is doc with its whitespace collapsed, for a message.
func oneLineJSON(doc string) string { return strings.Join(strings.Fields(doc), " ") }

// requireDetachedSweepRole ensures SpawnSweepCrossAccountRole in the account a
// detached sweep's members launch in, before the sweep creates anything else:
// the compute account, as launchSweepDetached resolves it
// (spawnconfig.ResolveComputeProfile: the caller's own unless an override
// names another, #1538). recordAccount is the account a resumed sweep's
// record names ("" at launch); when the credentials are for another account,
// spawn can neither check nor create the role there, so it does neither.
//
// It refuses (#1535) only when the sweep could launch nothing: the account is
// one the role may never be in, or there is no role and spawn could not
// create it.
func requireDetachedSweepRole(ctx context.Context, recordAccount string) error {
	cfg, accountID, err := loadSweepRoleAccount(ctx)
	if err != nil {
		return err
	}
	// Before anything is created, say where the members launch (#1538).
	fmt.Fprintln(os.Stderr, memberAccountLine(accountID, recordAccount, spawnconfig.ResolveComputeProfile()))
	if recordAccount != "" && recordAccount != accountID {
		fmt.Fprintf(os.Stderr, "⚠️  not checking %s: the sweep's members launch in account %s, and these credentials are for %s\n",
			sweepCrossAccountRoleName, recordAccount, accountID)
		return nil
	}
	return ensureSweepRole(ctx, cfg, accountID, false, os.Stderr)
}

// ── sweep setup / teardown ─────────────────────────────────────────────────

var sweepTeardownForce bool

var sweepSetupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Create or update " + sweepCrossAccountRoleName + ", which detached sweeps launch through",
	Long: `Create ` + sweepCrossAccountRoleName + ` in the account detached sweeps launch in, or bring
an existing one up to date: its trust (the sweep orchestrator's role, and nothing
else) and its policy.

You do not need to run this: every detached sweep does the same before it creates
anything else. It is here to set an account up ahead of time, and to update the
role with credentials that may manage IAM when the ones that launch sweeps may not.

The account is the compute account: your own credentials', unless
SPAWN_COMPUTE_PROFILE or 'spawn defaults set compute-profile' names another.

spawn refuses to create the role in, or trusting, an AWS Organizations
management account.

'spawn sweep teardown' removes it.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		cfg, accountID, err := loadSweepRoleAccount(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, memberAccountLine(accountID, "", spawnconfig.ResolveComputeProfile()))
		if err := ensureSweepRole(ctx, cfg, accountID, true, os.Stderr); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "✓ %s in account %s is current (policy %s)\n", sweepCrossAccountRoleName, accountID, aws.SweepRolePolicyName())
		return nil
	},
}

var sweepTeardownCmd = &cobra.Command{
	Use:   "teardown",
	Short: "Delete " + sweepCrossAccountRoleName + " from the account detached sweeps launch in",
	Long: `Delete ` + sweepCrossAccountRoleName + ` and every policy on it from the compute account.

Detached sweeps in this account stop working until the role is back; the next
detached sweep, or 'spawn sweep setup', creates it again.

It refuses while a sweep whose members launch in this account is still running
(any status but COMPLETED, FAILED or CANCELLED in the orchestration table), or
when it cannot read that table: the orchestrator would lose the role in the
middle of launching or cancelling them. --force deletes the role anyway.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		cfg, accountID, err := loadSweepRoleAccount(ctx)
		if err != nil {
			return err
		}
		return runSweepTeardown(ctx, cfg, accountID, sweepTeardownForce, cmd.OutOrStdout())
	},
}

// runSweepTeardown is `spawn sweep teardown` once the account is known.
func runSweepTeardown(ctx context.Context, cfg sdkaws.Config, accountID string, force bool, out io.Writer) error {
	api := sweepRoleIAM(cfg)
	st, err := aws.InspectSweepCrossAccountRole(ctx, api)
	if err != nil {
		return fmt.Errorf("look up %s in account %s: %w", sweepCrossAccountRoleName, accountID, err)
	}
	if !st.Exists {
		fmt.Fprintf(out, "✓ No %s in account %s; nothing to remove\n", sweepCrossAccountRoleName, accountID)
		return nil
	}

	active, aerr := activeSweepsInAccount(ctx, accountID)
	switch {
	case aerr != nil && !force:
		return fmt.Errorf("not deleting %s: could not check for running sweeps in account %s (%v). The orchestrator "+
			"acts for a running sweep through the role; --force deletes it anyway", sweepCrossAccountRoleName, accountID, aerr)
	case len(active) > 0 && !force:
		return fmt.Errorf("not deleting %s: %d sweep(s) with members in account %s are still running: %s. "+
			"The orchestrator launches and cancels their members through the role. Wait for them, cancel them "+
			"('spawn sweep cancel <id>'), or pass --force", sweepCrossAccountRoleName, len(active), accountID, strings.Join(active, ", "))
	case aerr != nil:
		fmt.Fprintf(os.Stderr, "⚠️  --force: deleting %s without knowing whether a sweep still runs in account %s (%v)\n", sweepCrossAccountRoleName, accountID, aerr)
	case len(active) > 0:
		fmt.Fprintf(os.Stderr, "⚠️  --force: deleting %s while %d sweep(s) still run in account %s (%s); the orchestrator "+
			"can no longer launch or cancel their members, which stop at their TTL\n", sweepCrossAccountRoleName, len(active), accountID, strings.Join(active, ", "))
	}

	if _, err := aws.TeardownSweepCrossAccountRole(ctx, api); err != nil {
		return err
	}
	fmt.Fprintf(out, "✓ Deleted %s from account %s\n", sweepCrossAccountRoleName, accountID)
	return nil
}

func init() {
	sweepGroupCmd.AddCommand(sweepSetupCmd, sweepTeardownCmd)
	sweepTeardownCmd.Flags().BoolVar(&sweepTeardownForce, "force", false,
		"Delete the role even while a sweep in this account is running, or when that cannot be checked")
}
