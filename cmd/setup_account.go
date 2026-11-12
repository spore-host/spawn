package cmd

// `spawn setup-account` — the one-time, admin-run step that provisions the
// spawn-instance permissions boundary in an AWS account.
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
// Re-running is safe and is how the boundary is upgraded: if spawn's document
// has changed, a new default version is written (older versions pruned to stay
// under IAM's five-version cap); if it is already current, nothing changes.

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/spf13/cobra"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
)

var setupAccountRegion string

// Seams for the hermetic tests: the caller-identity lookup and the IAM client.
var (
	setupAccountCaller = func(ctx context.Context, cfg aws.Config) (string, string, error) {
		return spawnaws.NewClientFromConfig(cfg).GetCallerIdentityInfo(ctx)
	}
	setupAccountIAMClient = func(cfg aws.Config) spawnaws.BoundaryAdminAPI { return spawnaws.NewBoundaryAdminIAM(cfg) }
)

var setupAccountCmd = &cobra.Command{
	Use:   "setup-account",
	Short: "Provision the spawn permissions boundary for this AWS account (admin, once)",
	Long: `Create (or update) the customer-managed permissions boundary that caps every
IAM role spawn creates in this account.

Run once per account with credentials allowed to create IAM policies (an
administrator). The documented spawn user baseline cannot create it on purpose.
Until it exists, launches that must create or update an instance role are
refused, naming this command.

Re-running upgrades the boundary when spawn's document has changed, and is
otherwise a no-op.`,
	RunE: runSetupAccount,
}

func runSetupAccount(cmd *cobra.Command, _ []string) error {
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

	out := cmd.OutOrStdout()
	warn := func(s string) { fmt.Fprintf(cmd.ErrOrStderr(), "  ! %s\n", s) }
	res, err := spawnaws.EnsureSpawnInstanceBoundary(ctx, setupAccountIAMClient(cfg), account, warn)
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
	fmt.Fprintln(out, "spawn now caps every role it creates in this account with this boundary.")
	return nil
}

func init() {
	rootCmd.AddCommand(setupAccountCmd)
	setupAccountCmd.Flags().StringVar(&setupAccountRegion, "region", "", "AWS region (default: profile/SDK region)")
}
