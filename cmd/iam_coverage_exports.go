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

// ShowExperimentalChat un-hides the experimental chat commands (chat_gate.go)
// until the returned func is called. The inventory, like docs-gen, keeps them:
// a user who opts in with SPAWN_EXPERIMENTAL_CHAT=1 needs their permissions.
func ShowExperimentalChat() (restore func()) {
	applyChatVisibility(true)
	return func() { applyChatVisibility(chatEnabled()) }
}

// BatchQueuePolicy is the inline policy of a batch queue's role (#1344).
func BatchQueuePolicy(scheduleBucket, resultsBucket, resultsPrefix string) (string, error) {
	return batchQueuePolicy(scheduleBucket, resultsBucket, resultsPrefix)
}
