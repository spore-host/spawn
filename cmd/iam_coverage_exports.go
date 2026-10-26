package cmd

import "github.com/spf13/cobra"

// The IAM coverage gate (internal/iamcoverage, #254) reads these. It holds the
// policies spawn generates to the AWS calls the code they are for makes, and
// inventories what each CLI command calls (docs/reference/cli-iam-actions.md).

// PoolWorkerPolicy is the inline policy of a pool's worker role.
func PoolWorkerPolicy(account, region, specBucket, runID string, extraRead, extraWrite []string) string {
	return poolWorkerPolicy(account, region, specBucket, runID, extraRead, extraWrite)
}

// RootCommand is the spawn command tree.
func RootCommand() *cobra.Command {
	return rootCmd
}
