package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/provider"
	"github.com/spore-host/spawn/pkg/taskpool"
)

// newPoolWorkerCmd is the on-instance side of pooled execution (#70): a fungible
// worker that pulls tasks from the run-scoped SQS queue, runs each in a clean
// workspace via the taskproto job protocol, and self-drains on idle-timeout — the
// scale-to-zero property (spored's TTL/idle backstop still applies, and the
// off-node reaper is the final backstop).
//
// It reuses pkg/taskpool.Worker with production seams: real SQS/S3 clients built
// from the instance's IMDS identity, taskpool.SpecStore as the SpecFetcher, and
// taskpool.ScriptExecer + DirWorkspace as the executer/workspace. The run id,
// results bucket, spec bucket, and idle timeout come from user-data env (written
// by the pool launcher), mirroring how the batch-queue runner reads
// SPAWN_QUEUE_S3_URL.
func newPoolWorkerCmd() *cobra.Command {
	var (
		runID       string
		specBucket  string
		specPrefix  string
		idleTimeout time.Duration
		workRoot    string
	)
	cmd := &cobra.Command{
		Use:   "pool-worker",
		Short: "Run as a fungible pooled worker: drain the run's task queue",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Env fallbacks (user-data writes these; flags override for manual runs).
			runID = orEnv(runID, "SPAWN_POOL_RUN_ID")
			specBucket = orEnv(specBucket, "SPAWN_POOL_SPEC_BUCKET")
			specPrefix = orEnv(specPrefix, "SPAWN_POOL_SPEC_PREFIX")
			if runID == "" {
				return fmt.Errorf("pool-worker: run id is required (--run-id or SPAWN_POOL_RUN_ID)")
			}
			if specBucket == "" {
				return fmt.Errorf("pool-worker: spec bucket is required (--spec-bucket or SPAWN_POOL_SPEC_BUCKET)")
			}
			return runPoolWorker(cmd.Context(), poolWorkerOpts{
				runID: runID, specBucket: specBucket, specPrefix: specPrefix,
				idleTimeout: idleTimeout, workRoot: workRoot,
			})
		},
	}
	cmd.Flags().StringVar(&runID, "run-id", "", "Run id of the pool queue to drain (or SPAWN_POOL_RUN_ID)")
	cmd.Flags().StringVar(&specBucket, "spec-bucket", "", "S3 bucket holding staged task specs (or SPAWN_POOL_SPEC_BUCKET)")
	cmd.Flags().StringVar(&specPrefix, "spec-prefix", "", "S3 key prefix for staged specs (or SPAWN_POOL_SPEC_PREFIX)")
	cmd.Flags().DurationVar(&idleTimeout, "idle-timeout", 5*time.Minute, "Drain the worker after this long with an empty queue")
	cmd.Flags().StringVar(&workRoot, "work-root", "/var/lib/nf-work", "Parent dir for per-task workspaces")
	return cmd
}

type poolWorkerOpts struct {
	runID       string
	specBucket  string
	specPrefix  string
	idleTimeout time.Duration
	workRoot    string
}

func runPoolWorker(ctx context.Context, o poolWorkerOpts) error {
	// Resolve region + account from the instance identity (IMDS), the same source
	// the daemon uses. The results bucket the job script writes completion records
	// into is spawn-results-<account>-<region>, matching `spawn task run`.
	prov, err := provider.NewProvider(ctx)
	if err != nil {
		return fmt.Errorf("detect provider: %w", err)
	}
	id, err := prov.GetIdentity(ctx)
	if err != nil {
		return fmt.Errorf("get identity: %w", err)
	}
	region := id.Region

	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return fmt.Errorf("load AWS config: %w", err)
	}
	sqsClient := sqs.NewFromConfig(cfg)
	s3Client := s3.NewFromConfig(cfg)

	q, err := taskpool.OpenQueue(ctx, sqsClient, o.runID)
	if err != nil {
		return fmt.Errorf("open pool queue: %w", err)
	}
	specs := &taskpool.SpecStore{Client: s3Client, Bucket: o.specBucket, Prefix: o.specPrefix}
	resultsBucket := fmt.Sprintf("spawn-results-%s-%s", id.AccountID, region)

	worker := &taskpool.Worker{
		Queue:     q,
		Fetcher:   specs,
		Execer:    &taskpool.ScriptExecer{ResultsBucket: resultsBucket, Region: region, Stdout: os.Stdout, Stderr: os.Stderr},
		Workspace: &taskpool.DirWorkspace{Root: o.workRoot},
		Config: taskpool.WorkerConfig{
			PollWaitSeconds: 20, // max SQS long-poll — fewest empty round-trips
			IdleTimeout:     o.idleTimeout,
			Log:             os.Stderr,
		},
	}

	executed, err := worker.Run(ctx)
	fmt.Fprintf(os.Stderr, "pool-worker: drained after %d task(s)\n", executed)
	return err
}

// orEnv returns v if non-empty, else the value of env var key.
func orEnv(v, key string) string {
	if v != "" {
		return v
	}
	return os.Getenv(key)
}
