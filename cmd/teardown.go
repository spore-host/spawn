package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/spore-host/libs/footprint"
	"github.com/spore-host/libs/lifecycle"
	"github.com/spore-host/libs/lifecycle/lifecycletest"

	"github.com/spore-host/spawn/pkg/aws"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
	fs "github.com/spore-host/spawn/pkg/footprintspawn"
	"github.com/spore-host/spawn/pkg/instancerole"
	"github.com/spore-host/spawn/pkg/scheduler"
	"github.com/spore-host/spawn/pkg/teardown"
)

var (
	teardownPlan    string
	teardownMode    string
	teardownYes     bool
	teardownNoLease bool
)

// noLeaseFlag is the hidden override that lets reap mode run without the
// shared-account teardown lease, which does not exist yet (spawn#1654).
const noLeaseFlag = "i-understand-no-lease"

var teardownCmd = &cobra.Command{
	Use:   "teardown",
	Short: "Re-check and delete what a 'spawn footprint -o json' report marks deletable",
	Long: `Carry out a teardown plan: a report from 'spawn footprint -o json'.

Every resource is re-read from the account before anything happens to it,
and the delete guard is applied again to what the account says NOW. A resource
is deleted only if it is tagged spore-host:managed-by=spawn, its name is one
spawn's teardown registers, it is not spore.host's own service
(spore-host:role=service), not tagged spore-host:permanent=true, not one of
the two permanent account-setup policies (spawn-instance-boundary and
spawn-user-lifecycle, which only 'spawn setup-account --remove' removes), and
not user data. A resource that only spawn's
name or the old spawn:managed tag matched (ownership legacy-name), and anything
another tool owns (calque, lagotto), is never deleted. Anything in use (a live
instance, an instance profile on one, a security group on a network interface,
a pool with workers, an attached policy) is kept, and so is everything it
depends on. Dependents go first: an instance before its security group and
profile, a profile before its role, a role before its policies.

--mode report (the default) re-checks everything and deletes nothing: it says
what reap would do. --mode reap deletes, and needs --yes.

This is the human-run path; the automatic idle teardown is spawn#1654. Reap
mode needs the shared-account teardown lease that keeps one user's teardown
from racing another user's setup, and that lease does not exist yet, so reap
mode refuses to run. It never runs in spore.host's infra account.`,
	Example: `  spawn footprint --all-regions -o json > plan.json
  # What would go, re-checked against the account
  spawn teardown --plan plan.json
  # From stdin
  spawn footprint -o json | spawn teardown --plan -`,
	RunE: runTeardown,
}

func init() {
	teardownCmd.Flags().StringVar(&teardownPlan, "plan", "", "The 'spawn footprint -o json' report to carry out, or - for stdin (required)")
	teardownCmd.Flags().StringVar(&teardownMode, "mode", string(footprint.ModeReport), "report (re-check, delete nothing) or reap (delete; needs --yes)")
	teardownCmd.Flags().BoolVarP(&teardownYes, "yes", "y", false, "Confirm --mode reap")
	teardownCmd.Flags().BoolVar(&teardownNoLease, noLeaseFlag, false, "Run --mode reap without the shared-account teardown lease")
	_ = teardownCmd.Flags().MarkHidden(noLeaseFlag)
	_ = teardownCmd.MarkFlagRequired("plan")
	rootCmd.AddCommand(teardownCmd)
}

func runTeardown(cmd *cobra.Command, _ []string) error {
	ctx := context.Background()
	mode, err := teardownModeOf(teardownMode, teardownYes, teardownNoLease)
	if err != nil {
		return err
	}
	rep, err := readTeardownPlan(cmd.InOrStdin(), teardownPlan)
	if err != nil {
		return err
	}
	client, err := aws.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("initialize AWS client: %w", err)
	}
	account, err := client.GetAccountID(ctx)
	if err != nil {
		return fmt.Errorf("resolve this account (a plan is only carried out in the account it was made for): %w", err)
	}
	refs, refNote := teardownRoleReferences(ctx)
	if refNote != "" {
		fmt.Fprintf(cmd.ErrOrStderr(), "note: %s\n", refNote)
	}
	deleter := teardown.New(client.Config(), teardown.Options{Account: account, RoleReferences: refs})
	res, err := executeTeardown(ctx, rep, account, mode, deleter, localLease)
	if res == nil {
		return err
	}
	if spawnOutputFormat == "json" {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		if jerr := enc.Encode(res); jerr != nil {
			return jerr
		}
	} else {
		renderTeardown(cmd.OutOrStdout(), res)
	}
	return teardownExitError(res, err)
}

// teardownModeOf checks the mode flags: report is the default, reap needs
// --yes and, until the lease exists, the hidden no-lease override.
func teardownModeOf(mode string, yes, noLease bool) (footprint.Mode, error) {
	switch footprint.Mode(mode) {
	case footprint.ModeReport:
		return footprint.ModeReport, nil
	case footprint.ModeReap:
		if !yes {
			return "", errors.New("--mode reap deletes resources; add --yes to confirm (or run --mode report to see what it would delete)")
		}
		if !noLease {
			return "", errors.New("--mode reap needs the shared-account teardown lease, which does not exist yet (spawn#1654), " +
				"so one user's teardown could race another user's setup; nothing was deleted. --mode report shows what reap would delete")
		}
		return footprint.ModeReap, nil
	}
	return "", fmt.Errorf("--mode %q: want report or reap", mode)
}

// readTeardownPlan reads and checks a footprint report from path, or from in
// for "-".
func readTeardownPlan(in io.Reader, path string) (*footprint.Report, error) {
	r := in
	if path != "-" {
		f, err := os.Open(path) //nolint:gosec // the user's own plan file
		if err != nil {
			return nil, fmt.Errorf("read the plan: %w", err)
		}
		defer f.Close()
		r = f
	}
	rep, err := footprint.Decode(r)
	if err != nil {
		return nil, fmt.Errorf("read the plan: %w", err)
	}
	return rep, nil
}

// teardownRoleReferences collects the schedules that name per-task roles, as
// 'spawn cleanup --instance-roles' does. When they cannot be read, the
// references stay uncollected, and no per-task role or profile is deleted.
func teardownRoleReferences(ctx context.Context) (instancerole.References, string) {
	infraCfg, err := spawnconfig.LoadInfraAWSConfig(ctx, scheduler.TableRegion)
	if err == nil {
		var refs instancerole.References
		if refs, err = instancerole.CollectReferences(ctx, instancerole.NewScheduleSource(infraCfg)); err == nil {
			return refs, ""
		}
	}
	return instancerole.References{}, fmt.Sprintf("the schedules that may name spawn-instance-* roles could not be read, so none of those roles "+
		"or profiles is deleted (they are in spawn's infra account; set SPAWN_INFRA_PROFILE to its profile, or to \"\" if it is this account): %v", err)
}

// leaseFunc returns the teardown lease reap mode runs under.
type leaseFunc func(ctx context.Context) (*lifecycle.Lease, error)

// executeTeardown checks rep against the account it is carried out in, and
// runs it: footprint.NewPlan with spawn's patterns, then footprint.Execute.
func executeTeardown(ctx context.Context, rep *footprint.Report, account string, mode footprint.Mode, deleter footprint.Deleter, lease leaseFunc) (*footprint.Result, error) {
	switch {
	case rep.Tool != fs.Tool:
		return nil, fmt.Errorf("the plan is %s's footprint, not spawn's; run '%s teardown' instead", rep.Tool, rep.Tool)
	case rep.Account != account:
		return nil, fmt.Errorf("the plan was made for account %q and this is %q; run 'spawn footprint -o json' here", rep.Account, account)
	case mode == footprint.ModeReap && (account == "" || account == fs.InfraAccountID):
		return nil, fmt.Errorf("refusing to reap in account %q: spore.host's infra account, or one that could not be identified, "+
			"holds spore.host's own service, which shares spawn's tags", account)
	}
	plan, err := footprint.NewPlan(fs.Tool, fs.Patterns, rep.Resources)
	if err != nil {
		return nil, fmt.Errorf("order the plan: %w", err)
	}
	var l *lifecycle.Lease
	if mode == footprint.ModeReap {
		if l, err = lease(ctx); err != nil {
			return nil, fmt.Errorf("take the teardown lease: %w", err)
		}
	}
	return footprint.Execute(ctx, plan, mode, deleter, l)
}

// noLeaseScope is the scope of the stand-in lease.
const noLeaseScope = "no-lease"

// localLease is the stand-in for the shared-account teardown lease, which
// does not exist yet (spawn#1654): a lifecycle table held in this process's
// memory, so footprint.Execute's heartbeat runs and coordinates with no one.
// It is only reached with the hidden --i-understand-no-lease, which says so.
func localLease(ctx context.Context) (*lifecycle.Lease, error) {
	f := lifecycletest.New()
	c, err := lifecycle.New(lifecycle.Options{
		DynamoDB: func(string) (lifecycle.API, error) { return f, nil },
		Owner:    "spawn-teardown (no lease)",
		Sleep:    func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		return nil, err
	}
	if _, _, err := c.Observe(ctx, fs.Tool, noLeaseScope, false); err != nil {
		return nil, err
	}
	return c.Claim(ctx, fs.Tool, noLeaseScope, 0)
}

// renderTeardown prints what Execute did, one line per resource.
func renderTeardown(w io.Writer, res *footprint.Result) {
	verb := "would delete"
	if res.Mode == footprint.ModeReap {
		verb = "deleted"
	}
	n := res.Count(footprint.OutcomeWouldDelete) + res.Count(footprint.OutcomeDeleted)
	fmt.Fprintf(w, "spawn teardown --mode %s: %s %d of %d; kept %d, refused %d, skipped %d, already gone %d, failed %d\n\n",
		res.Mode, verb, n, len(res.Actions), res.Count(footprint.OutcomeKept), res.Count(footprint.OutcomeRefused),
		res.Count(footprint.OutcomeSkipped), res.Count(footprint.OutcomeGone), res.Count(footprint.OutcomeFailed))
	tw := newTableWriter(w)
	fmt.Fprintf(tw, "  OUTCOME\tTYPE\tNAME\tWHY\n")
	for _, a := range res.Actions {
		why := a.Reason
		if a.Err != nil {
			why = a.Err.Error()
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", a.Outcome, a.Type, a.Name, why)
	}
	tw.Flush()
	if res.Mode == footprint.ModeReport {
		fmt.Fprintf(w, "\nNothing was deleted. --mode reap --yes deletes the would-delete rows, re-checking each first.\n")
	}
}

// teardownExitError is the command's error: Execute's, without the steps
// kept because they are in use, which are the teardown working as meant.
func teardownExitError(res *footprint.Result, err error) error {
	if err == nil {
		return nil
	}
	var real []error
	for _, a := range res.Actions {
		if a.Err != nil && !errors.Is(a.Err, teardown.ErrInUse) {
			real = append(real, a.Err)
		}
	}
	if len(real) == 0 && !errors.Is(err, lifecycle.ErrLeaseLost) {
		return nil
	}
	return fmt.Errorf("teardown: %w", errors.Join(append(real, leaseErr(err))...))
}

func leaseErr(err error) error {
	if errors.Is(err, lifecycle.ErrLeaseLost) {
		return lifecycle.ErrLeaseLost
	}
	return nil
}
