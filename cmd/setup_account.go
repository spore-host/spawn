package cmd

// `spawn setup-account` — the one-time, admin-run step that provisions what
// spawn needs in an AWS account, and (with --remove) takes it away again.
//
// spawn caps every role it creates (spored-instance-role, the spawn-instance-*
// family, the worker-pool roles and the cross-account sweep role) with a
// customer-managed permissions boundary, so that a caller who can write a
// role's policies — which spawn does by design — cannot use an instance to
// reach IAM administration or another account. The documented least-privilege
// user baseline deliberately cannot create that boundary (creating
// spore-host/* policies other than spored-* is refused), so it is created once
// per account here, with credentials allowed to create IAM policies.
//
// Beside it, setup-account creates spore-host/spawn-user-lifecycle (#1666):
// the lifecycle lease actions spawn's creators, `spawn doctor` and
// `spawn reaper teardown --if-idle-for` need on the spore-host-lifecycle
// table. An administrator attaches it to spawn users' groups or roles.
//
// Re-running is safe and is how both are upgraded: if spawn's document has
// changed, a new default version is written (older versions pruned to stay
// under IAM's five-version cap); if it is already current, nothing changes.
//
// --remove (#1656) is the only path that deletes them: report first, --yes to
// delete, refused while any role still has the boundary, and refused in
// spore.host's infra account. --offboard adds the spore-portal-onboard role.

import (
	"context"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/spf13/cobra"
	libtags "github.com/spore-host/libs/tags"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/footprintspawn"
	"github.com/spore-host/spawn/pkg/lifecyclemark"
	"github.com/spore-host/spawn/pkg/sporetags"
)

var (
	setupAccountRegion   string
	setupAccountRemove   bool
	setupAccountOffboard bool
	setupAccountYes      bool
)

// Seams for the hermetic tests: the caller-identity lookup and the IAM client.
var (
	setupAccountCaller = func(ctx context.Context, cfg aws.Config) (string, string, error) {
		return spawnaws.NewClientFromConfig(cfg).GetCallerIdentityInfo(ctx)
	}
	setupAccountIAMClient = func(cfg aws.Config) spawnaws.AccountSetupAPI { return spawnaws.NewAccountSetupIAM(cfg) }
)

var setupAccountCmd = &cobra.Command{
	Use:   "setup-account",
	Short: "Provision (or, with --remove, remove) spawn's account setup: the permissions boundary and the user lifecycle policy (admin)",
	Long: `Create (or update) what spawn needs once per AWS account, with
credentials allowed to create IAM policies (an administrator):

  - spore-host/spawn-instance-boundary, the customer-managed permissions
    boundary that caps every IAM role spawn creates in this account. The
    documented spawn user baseline cannot create it on purpose. Until it
    exists, launches that must create or update an instance role are refused,
    naming this command.
  - spore-host/spawn-user-lifecycle, a customer-managed policy granting the
    actions spawn users need on the shared-account lifecycle lease table
    (spore-host-lifecycle), so idle teardown can see the account in use.
    Attach it to the IAM groups or roles your spawn users sign in with, e.g.
    aws iam attach-group-policy --group-name <group> --policy-arn <arn>.

Both are tagged spore-host:managed-by=spawn and spore-host:permanent=true: no
idle teardown or cleanup deletes them. Re-running upgrades either when spawn's
document has changed, and is otherwise a no-op.

--remove takes them away again. It is the only command that deletes the
boundary. Without --yes it prints what it would delete and deletes nothing.
It refuses while any role or user still has the boundary as its permissions
boundary (it lists them), and in spore.host's infra account. It detaches the
user lifecycle policy from whatever it is attached to, deletes each policy's
older versions, then the policy, the boundary last.

--offboard (with --remove) also deletes the spore-portal-onboard role that
'spawn onboard' created: the portal's access to this account, and the record
that the account was onboarded, so it is removed only when you ask. A role made
by the portal's CloudFormation quick-create is refused: delete that stack
instead. Offboarding here does not change the portal's own record of the
account (spore-host/spore-host#602).`,
	Example: `  spawn setup-account                       # provision, or upgrade
  spawn setup-account --remove              # print what removal would delete
  spawn setup-account --remove --yes        # delete the boundary and the user policy
  spawn setup-account --remove --offboard --yes   # ...and the spore-portal-onboard role`,
	RunE: runSetupAccount,
}

func runSetupAccount(cmd *cobra.Command, _ []string) error {
	if setupAccountOffboard && !setupAccountRemove {
		return fmt.Errorf("--offboard is part of --remove: run 'spawn setup-account --remove --offboard'")
	}
	if setupAccountYes && !setupAccountRemove {
		return fmt.Errorf("--yes confirms --remove; provisioning needs no confirmation")
	}
	ctx := context.Background()
	cfg, err := loadAWSConfig(ctx, setupAccountRegion)
	if err != nil {
		return fmt.Errorf("load AWS config: %w", err)
	}
	if setupAccountRegion != "" {
		cfg.Region = setupAccountRegion
	}

	account, _, err := setupAccountCaller(ctx, cfg)
	if err != nil {
		return fmt.Errorf("get caller identity: %w", err)
	}
	if setupAccountRemove {
		return runSetupAccountRemove(ctx, cmd, setupAccountIAMClient(cfg), account)
	}

	out := cmd.OutOrStdout()
	warn := func(s string) { fmt.Fprintf(cmd.ErrOrStderr(), "  ! %s\n", s) }
	// Both policies are kept until --remove, but this is a create in the
	// account like any other (spawn#1654).
	lifecyclemark.Mark(ctx, cfg, lifecyclemark.ScopeGlobal)
	api := setupAccountIAMClient(cfg)
	res, err := spawnaws.EnsureSpawnInstanceBoundary(ctx, api, account, warn)
	if err != nil {
		return err
	}
	switch {
	case res.Created:
		fmt.Fprintf(out, "Created the spawn permissions boundary %s in account %s.\n", res.ARN, account)
	case res.Updated:
		fmt.Fprintf(out, "Updated the spawn permissions boundary %s to the current version", res.ARN)
		if res.Pruned > 0 {
			fmt.Fprintf(out, " (pruned %d superseded version(s))", res.Pruned)
		}
		fmt.Fprintln(out, ".")
	default:
		fmt.Fprintf(out, "The spawn permissions boundary %s is already current in account %s.\n", res.ARN, account)
	}
	if res.Tagged {
		fmt.Fprintf(out, "Tagged the boundary %s=spawn and %s=%s, the one spawn resource kept on purpose.\n",
			libtags.ManagedBy, sporetags.PermanentKey, sporetags.PermanentValue)
	}
	fmt.Fprintln(out, "spawn now caps every role it creates in this account with this boundary.")

	if account == footprintspawn.InfraAccountID {
		// No lease is marked in the infra account (pkg/lifecyclemark), so
		// its users need no lease permissions.
		fmt.Fprintf(out, "Not creating %s: spore.host's infra account has no lifecycle lease.\n", lifecyclemark.UserPolicy)
		return nil
	}
	up, err := spawnaws.EnsureSpawnUserLifecyclePolicy(ctx, api, account, warn)
	if err != nil {
		return fmt.Errorf("the boundary is in place, but the user lifecycle policy is not: %w", err)
	}
	switch {
	case up.Created:
		fmt.Fprintf(out, "Created the user lifecycle policy %s.\n", up.ARN)
	case up.Updated:
		fmt.Fprintf(out, "Updated the user lifecycle policy %s to the current version.\n", up.ARN)
	default:
		fmt.Fprintf(out, "The user lifecycle policy %s is already current.\n", up.ARN)
	}
	fmt.Fprintf(out, "Attach it to the IAM groups or roles your spawn users sign in with, so idle teardown can see the account in use:\n"+
		"    aws iam attach-group-policy --group-name <group> --policy-arn %s\n"+
		"Without it spawn works as before, but this account is never torn down for idleness ('spawn doctor' checks it).\n", up.ARN)
	return nil
}

// runSetupAccountRemove is --remove: the plan, and with --yes the deletions.
func runSetupAccountRemove(ctx context.Context, cmd *cobra.Command, api spawnaws.AccountSetupAPI, account string) error {
	out := cmd.OutOrStdout()
	plan, err := spawnaws.PlanAccountRemoval(ctx, api, account, setupAccountOffboard)
	if err != nil {
		return err
	}
	writeAccountRemovalPlan(out, plan)
	switch {
	case len(plan.Refusals) > 0:
		return fmt.Errorf("refusing to remove spawn's account setup in %s; nothing was deleted", account)
	case plan.Empty():
		return nil
	case !setupAccountYes:
		fmt.Fprintln(out, "\nReport only: nothing was deleted. Run again with --yes to delete the above.")
		return nil
	}
	fmt.Fprintln(out)
	if err := spawnaws.ExecuteAccountRemoval(ctx, api, plan, func(s string) { fmt.Fprintf(out, "  ✓ %s\n", s) }); err != nil {
		return err
	}
	fmt.Fprintf(out, "Removed spawn's account setup from %s.", account)
	if !plan.Offboard {
		fmt.Fprintf(out, " The %s role, if any, is kept: add --offboard to remove it too.", spawnaws.PortalOnboardRoleName)
	}
	fmt.Fprintln(out)
	return nil
}

// writeAccountRemovalPlan prints what --remove would delete, in order, and why
// it would refuse.
func writeAccountRemovalPlan(w io.Writer, p *spawnaws.AccountRemovalPlan) {
	fmt.Fprintf(w, "Removing spawn's account setup from %s:\n", p.Account)
	if p.Empty() {
		fmt.Fprintln(w, "  nothing to remove: no spawn permissions boundary, user lifecycle policy or (with --offboard) spore-portal-onboard role.")
	}
	step := 0
	policy := func(what string, pr spawnaws.PolicyRemoval) {
		if !pr.Exists {
			fmt.Fprintf(w, "  - %s %s: not present\n", what, pr.ARN)
			return
		}
		step++
		fmt.Fprintf(w, "  %d. %s %s\n", step, what, pr.ARN)
		for _, e := range pr.AttachedTo {
			fmt.Fprintf(w, "       detach from %s\n", e)
		}
		for _, v := range pr.OlderVersions {
			fmt.Fprintf(w, "       delete version %s\n", v)
		}
		fmt.Fprintf(w, "       delete the policy (default version %s)\n", pr.DefaultVersion)
		for _, e := range pr.BoundaryOf {
			fmt.Fprintf(w, "       ! the permissions boundary of %s\n", e)
		}
	}
	policy("the user lifecycle policy", p.UserPolicy)
	if r := p.OnboardRole; r != nil {
		if !r.Exists {
			fmt.Fprintf(w, "  - the role %s: not present\n", r.Name)
		} else {
			step++
			fmt.Fprintf(w, "  %d. the role %s (the portal's access to this account)\n", step, r.ARN)
			for _, ip := range r.InlinePolicies {
				fmt.Fprintf(w, "       delete inline policy %s\n", ip)
			}
			for _, a := range r.Attached {
				fmt.Fprintf(w, "       detach %s\n", a)
			}
			fmt.Fprintln(w, "       delete the role")
		}
	}
	policy("the permissions boundary", p.Boundary)
	for _, r := range p.Refusals {
		fmt.Fprintf(w, "\nRefused: %s\n", r)
	}
}

func init() {
	rootCmd.AddCommand(setupAccountCmd)
	setupAccountCmd.Flags().StringVar(&setupAccountRegion, "region", "", "AWS region (default: profile/SDK region)")
	setupAccountCmd.Flags().BoolVar(&setupAccountRemove, "remove", false,
		"Remove spawn's account setup: the permissions boundary and the user lifecycle policy. Prints the plan; add --yes to delete")
	setupAccountCmd.Flags().BoolVar(&setupAccountOffboard, "offboard", false,
		"With --remove, also delete the spore-portal-onboard role (the portal's access to this account)")
	setupAccountCmd.Flags().BoolVar(&setupAccountYes, "yes", false, "With --remove, delete what the plan lists instead of only printing it")
}
