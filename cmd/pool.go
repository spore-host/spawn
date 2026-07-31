package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spore-host/cohort"
	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/launcher"
	"github.com/spore-host/spawn/pkg/taskcohort"
	"github.com/spore-host/spawn/pkg/taskpool"
	"github.com/spore-host/spawn/pkg/taskproto"
)

// ── spawn pool ─────────────────────────────────────────────────────────────
//
// Pooled execution for wide fan-out (#70): provision a set of fungible worker
// instances (a cohort) that drain a shared SQS task queue, reusing across jobs so
// short tasks don't each pay a full instance boot. This is the CLI seam over the
// tested pkg/taskpool (queue + worker) and pkg/taskcohort (cohort provider) — the
// worker loop itself is `spored pool-worker`, launched via user-data.
//
// Commands:
//   spawn pool create --run-id R --workers N --instance-type T   provision + queue
//   spawn pool submit --run-id R --spec f.json                   stage + enqueue one task
//   spawn pool status --run-id R                                 queue depth
//   spawn pool drain  --run-id R                                 delete the queue

var (
	poolRunID        string
	poolWorkers      int
	poolMinViable    int
	poolInstanceType string
	poolSpot         bool
	poolTTL          string
	poolIdleTimeout  time.Duration
	poolSpecPath     string
	poolVisTimeout   int
)

var poolGroupCmd = &cobra.Command{
	Use:   "pool",
	Short: "Pooled execution: fungible workers drain a shared task queue (#70)",
	Long: `Run a wide fan-out as a POOL of reusable worker instances instead of one
ephemeral instance per task.

At high fan-out, one-instance-per-task is dispatch-bound and pays a full instance
boot per task, so short tasks self-terminate faster than new ones launch and
concurrency can never reach N. A worker pool inverts this: N fungible workers pull
tasks from a run-scoped SQS queue and reuse across jobs (per-job cost is stage+run
only), degrading gracefully — ask for N workers, get M, and tasks drain through
whatever pool came up. Workers self-terminate on idle-timeout (scale to zero).`,
}

// specBucketFor returns the per-account bucket staged task specs live in. Reuses
// the results-bucket naming so a pool run needs no new bucket wiring.
func poolSpecBucket(account, region string) string {
	return fmt.Sprintf("spawn-results-%s-%s", account, region)
}

// poolWorkerPolicy builds the scoped inline IAM policy a pool worker's instance
// profile needs, and NOTHING more:
//   - SQS on THIS run's queue only (arn scoped to spawn-pool-<runID>): the worker
//     resolves the queue, long-polls, and acks — GetQueueUrl/ReceiveMessage/
//     DeleteMessage/GetQueueAttributes. (SendMessage/CreateQueue/DeleteQueue are
//     the operator's, not the worker's.)
//   - S3 read on the spawn-binaries buckets (the bootstrap downloads spored) and
//     read+write on the spec/results bucket (fetch staged specs; the taskproto job
//     script writes completion.json/.exitcode there).
//
// Known limitation: a task's OWN input/output buckets (beyond the results bucket)
// aren't known at pool-create time, so they are not granted here — a pooled task
// that stages from/to a third bucket needs that access added (tracked for a
// follow-up; the common nf-spawn case keeps work in the results/work bucket).
func poolWorkerPolicy(account, region, specBucket, runID string) string {
	queueName := "spawn-pool-" + runID
	queueARN := fmt.Sprintf("arn:aws:sqs:%s:%s:%s", region, account, queueName)
	binPrefix := "arn:aws:s3:::spawn-binaries-*"
	specObj := fmt.Sprintf("arn:aws:s3:::%s/*", specBucket)
	specBkt := fmt.Sprintf("arn:aws:s3:::%s", specBucket)
	return fmt.Sprintf(`{
  "Version": "2012-10-17",
  "Statement": [
    {"Effect":"Allow","Action":["sqs:GetQueueUrl","sqs:ReceiveMessage","sqs:DeleteMessage","sqs:GetQueueAttributes"],"Resource":[%q]},
    {"Effect":"Allow","Action":["s3:GetObject"],"Resource":[%q,%q]},
    {"Effect":"Allow","Action":["s3:PutObject"],"Resource":[%q]},
    {"Effect":"Allow","Action":["s3:ListBucket","s3:GetBucketLocation"],"Resource":[%q,%q]}
  ]
}`, queueARN, binPrefix+"/*", specObj, specObj, binPrefix, specBkt)
}

var poolCreateCmd = &cobra.Command{
	Use:   "create --run-id R --workers N --instance-type T",
	Short: "Provision the worker pool + task queue for a run",
	RunE: func(cmd *cobra.Command, args []string) error {
		if poolRunID == "" || poolInstanceType == "" {
			return fmt.Errorf("--run-id and --instance-type are required")
		}
		if poolWorkers < 1 {
			return fmt.Errorf("--workers must be >= 1")
		}
		ctx := cmd.Context()
		awsClient, err := aws.NewClient(ctx)
		if err != nil {
			return fmt.Errorf("init AWS client: %w", err)
		}
		region := awsClient.Config().Region
		if region == "" {
			return fmt.Errorf("no region configured")
		}
		account, err := awsClient.GetAccountID(ctx)
		if err != nil {
			return fmt.Errorf("resolve account id: %w", err)
		}
		specBucket := poolSpecBucket(account, region)
		// Ensure the spec/results bucket exists before workers try to read from it.
		if err := awsClient.CreateS3BucketIfNotExists(ctx, specBucket, region); err != nil {
			return fmt.Errorf("ensure spec bucket %s: %w", specBucket, err)
		}

		// Create the run-scoped queue. Visibility timeout must exceed the longest
		// single-task runtime; default sized from the TTL, overridable.
		vis := poolVisTimeout
		if vis <= 0 {
			vis = 900 // 15m default claim window
		}
		pool, err := taskpool.CreateForRunWithConfig(ctx, awsClient.Config(), poolRunID, specBucket, "staging", vis)
		if err != nil {
			return fmt.Errorf("create pool queue: %w", err)
		}

		// Provision the workers as a partial cohort: ask for N, accept >= minViable
		// (best-effort/eventual). Each worker boots with user-data that runs
		// `spored pool-worker`, pointed at this run's queue + spec bucket.
		mv := poolMinViable
		if mv < 1 {
			mv = 1
		}
		if mv > poolWorkers {
			mv = poolWorkers
		}
		if err := provisionPoolWorkers(ctx, awsClient, region, specBucket); err != nil {
			// The queue was created above; if provisioning failed there are no workers
			// to drain it, so delete it now rather than leak an orphaned SQS queue
			// (a failed `pool create` must leave nothing behind). Best-effort.
			if derr := pool.Drain(ctx); derr != nil {
				fmt.Fprintf(os.Stderr, "⚠️  could not clean up the queue after a failed provision: %v\n", derr)
			}
			return err
		}

		fmt.Fprintf(os.Stderr, "✅ Pool %q ready: requested %d workers (min viable %d), queue created.\n", poolRunID, poolWorkers, mv)
		fmt.Fprintf(os.Stderr, "   Submit tasks:  spawn pool submit --run-id %s --spec <spec.json>\n", poolRunID)
		fmt.Fprintf(os.Stderr, "   Watch depth:   spawn pool status --run-id %s\n", poolRunID)
		fmt.Fprintf(os.Stderr, "   Tear down:     spawn pool drain --run-id %s\n", poolRunID)
		return nil
	},
}

// provisionPoolWorkers launches the worker cohort via taskcohort + cohort. Workers
// are fungible, so one base config (with the pool-worker user-data) fits all; the
// partial cohort gives best-effort/eventual semantics. Returns nil once the cohort
// reconciles (Ready with >= minViable) or an error if it can't reach minViable.
func provisionPoolWorkers(ctx context.Context, awsClient *aws.Client, region, specBucket string) error {
	// The worker bootstrap: export the pool env the `spored pool-worker` loop reads,
	// then run it. Runs after spored is installed (CustomUserData is appended to the
	// bootstrap), so `spored` is on PATH.
	workerCmd := fmt.Sprintf(`export SPAWN_POOL_RUN_ID=%q
export SPAWN_POOL_SPEC_BUCKET=%q
export SPAWN_POOL_SPEC_PREFIX=%q
spored pool-worker --idle-timeout %s`,
		poolRunID, specBucket, "staging", poolIdleTimeout.String())

	userData, err := launcher.BuildLinuxBootstrap(launcher.BootstrapConfig{
		Username:       "ec2-user",
		CustomUserData: workerCmd,
	})
	if err != nil {
		return fmt.Errorf("build worker bootstrap: %w", err)
	}

	// Resolve the AMI and IAM instance profile UP FRONT, once, for the whole pool.
	// The taskcohort Actuator calls aws.Client.Launch directly (RunInstances), which
	// — unlike the launcher.Provision path used by `spawn task run` — does NOT
	// auto-fill these. RunInstances requires an ImageId, so an empty AMI fails every
	// worker with MissingParameter. Workers are homogeneous, so one AMI + one shared
	// spored profile fits all of them (resolve once, reuse across the cohort).
	ami, err := awsClient.GetRecommendedAMI(ctx, region, poolInstanceType)
	if err != nil {
		return fmt.Errorf("auto-detect worker AMI for %s in %s: %w", poolInstanceType, region, err)
	}
	// A pool worker needs MORE than the bare spored role: it pulls from the run's
	// SQS queue (GetQueueUrl/ReceiveMessage/DeleteMessage/GetQueueAttributes) and
	// its job script reads staged specs + writes completion records in S3. The
	// bare SetupSporedIAMRole grants NEITHER — a worker with it fails GetQueueUrl
	// with NonExistentQueue (SQS masks access-denied as not-found) and exits. So
	// build a SCOPED instance profile (like `spawn task run` does) granting exactly
	// the pool queue + the spec/results bucket, plus the spored binary-download the
	// bootstrap needs. CreateOrGetInstanceProfile also guarantees SSM.
	account, err := awsClient.GetAccountID(ctx)
	if err != nil {
		return fmt.Errorf("resolve account id for worker policy: %w", err)
	}
	iamProfile, err := awsClient.CreateOrGetInstanceProfile(ctx, aws.IAMRoleConfig{
		RoleName:         "spawn-pool-worker",
		TrustServices:    []string{"ec2"},
		InlinePolicyJSON: poolWorkerPolicy(account, region, specBucket, poolRunID),
	})
	if err != nil {
		return fmt.Errorf("set up worker IAM instance profile: %w", err)
	}

	base := aws.LaunchConfig{
		InstanceType:       poolInstanceType,
		Region:             region,
		AMI:                ami,
		IamInstanceProfile: iamProfile,
		Spot:               poolSpot,
		TTL:                poolTTL,
		IdleTimeout:        poolIdleTimeout.String(),
		OnComplete:         "terminate",
		UserData:           launcher.EncodeLinuxUserData(userData),
		Tags: map[string]string{
			"spawn:pool-run-id": poolRunID,
			"spawn:role":        "pool-worker",
		},
	}

	act := &taskcohort.Actuator{Client: awsClient, Region: region, BaseConfig: base}
	obs := &taskcohort.Observer{Client: awsClient, Region: region}

	capacity := cohort.CapacityOnDemand
	if poolSpot {
		capacity = cohort.CapacitySpot
	}
	rung := cohort.Rung{InstanceType: poolInstanceType, CapacityModel: capacity}

	members := make([]cohort.EntityIntent, 0, poolWorkers)
	for i := 0; i < poolWorkers; i++ {
		id := cohort.EntityID(fmt.Sprintf("%s-worker-%d", poolRunID, i))
		intent, err := cohort.NewEntityIntent("pool", id, "g1", cohort.CohortID(poolRunID),
			cohort.RungPlacement{Rung: rung, Chain: []cohort.Rung{rung}}, "")
		if err != nil {
			return fmt.Errorf("build worker %d intent: %w", i, err)
		}
		members = append(members, intent)
	}
	mv := poolMinViable
	if mv < 1 {
		mv = 1
	}
	if mv > poolWorkers {
		mv = poolWorkers
	}
	c, err := cohort.NewPartialCohort(cohort.CohortID(poolRunID), members, cohort.DefaultBudget(), mv, nil)
	if err != nil {
		return fmt.Errorf("build worker cohort: %w", err)
	}

	r := cohort.NewReconciler(act, obs, taskcohort.Classifier{}, nil, nil, nil)
	fmt.Fprintf(os.Stderr, "🚀 Provisioning up to %d pool workers (%s%s)...\n", poolWorkers, poolInstanceType, spotSuffix(poolSpot))
	outcome, err := r.Reconcile(ctx, c)
	if err != nil {
		return fmt.Errorf("provision workers: %w", err)
	}
	if !outcome.Ready {
		// Surface WHY: render each member's terminal fault (the AWS error code +
		// message the Classifier preserved), so a provisioning failure is
		// diagnosable instead of an opaque "failed to reach min viable". Mirrors
		// how launchCohort renders job-array member failures.
		var details []string
		for _, m := range members {
			rec := outcome.Records[m.ID]
			if !rec.Succeeded() {
				details = append(details, fmt.Sprintf("  %s: %s", m.ID, rec.Summary()))
			}
		}
		return fmt.Errorf("pool failed to reach min viable (%d of %d) workers:\n%s",
			mv, poolWorkers, strings.Join(details, "\n"))
	}
	return nil
}

var poolSubmitCmd = &cobra.Command{
	Use:   "submit --run-id R --spec spec.json",
	Short: "Stage a task spec to S3 and enqueue it for the pool",
	RunE: func(cmd *cobra.Command, args []string) error {
		if poolRunID == "" || poolSpecPath == "" {
			return fmt.Errorf("--run-id and --spec are required")
		}
		ctx := cmd.Context()
		specJSON, err := os.ReadFile(poolSpecPath)
		if err != nil {
			return fmt.Errorf("read spec: %w", err)
		}
		spec, err := taskproto.ParseSpec(specJSON)
		if err != nil {
			return err
		}
		awsClient, err := aws.NewClient(ctx)
		if err != nil {
			return fmt.Errorf("init AWS client: %w", err)
		}
		region := awsClient.Config().Region
		account, err := awsClient.GetAccountID(ctx)
		if err != nil {
			return fmt.Errorf("resolve account id: %w", err)
		}
		pool, err := openPool(ctx, awsClient, region, account)
		if err != nil {
			return err
		}
		if err := pool.Enqueue(ctx, spec, specJSON); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "✅ Enqueued task %s to pool %s\n", spec.TaskID, poolRunID)
		return nil
	},
}

var poolStatusCmd = &cobra.Command{
	Use:   "status --run-id R",
	Short: "Show the pool queue depth (visible + in-flight tasks)",
	RunE: func(cmd *cobra.Command, args []string) error {
		if poolRunID == "" {
			return fmt.Errorf("--run-id is required")
		}
		ctx := cmd.Context()
		awsClient, err := aws.NewClient(ctx)
		if err != nil {
			return fmt.Errorf("init AWS client: %w", err)
		}
		region := awsClient.Config().Region
		account, err := awsClient.GetAccountID(ctx)
		if err != nil {
			return fmt.Errorf("resolve account id: %w", err)
		}
		pool, err := openPool(ctx, awsClient, region, account)
		if err != nil {
			return err
		}
		visible, inFlight, err := pool.Depth(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("pool %s: %d waiting, %d in-flight\n", poolRunID, visible, inFlight)
		return nil
	},
}

var poolDrainCmd = &cobra.Command{
	Use:   "drain --run-id R",
	Short: "Delete the pool's task queue (workers self-terminate on idle)",
	RunE: func(cmd *cobra.Command, args []string) error {
		if poolRunID == "" {
			return fmt.Errorf("--run-id is required")
		}
		ctx := cmd.Context()
		awsClient, err := aws.NewClient(ctx)
		if err != nil {
			return fmt.Errorf("init AWS client: %w", err)
		}
		region := awsClient.Config().Region
		account, err := awsClient.GetAccountID(ctx)
		if err != nil {
			return fmt.Errorf("resolve account id: %w", err)
		}
		pool, err := openPool(ctx, awsClient, region, account)
		if err != nil {
			return err
		}
		if err := pool.Drain(ctx); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "✅ Drained pool %s (queue deleted; idle workers self-terminate, reaper backstops)\n", poolRunID)
		return nil
	},
}

// openPool resolves an existing run-scoped pool (queue + spec store) for the
// submit/status/drain commands. Client construction lives in pkg/taskpool so cmd/
// doesn't import the SQS/S3 SDK directly (#326/#327 boundary).
func openPool(ctx context.Context, awsClient *aws.Client, region, account string) (*taskpool.Pool, error) {
	pool, err := taskpool.OpenForRunWithConfig(ctx, awsClient.Config(), poolRunID, poolSpecBucket(account, region), "staging")
	if err != nil {
		return nil, err
	}
	pool.Log = os.Stderr
	return pool, nil
}

func init() {
	rootCmd.AddCommand(poolGroupCmd)
	poolGroupCmd.AddCommand(poolCreateCmd, poolSubmitCmd, poolStatusCmd, poolDrainCmd)

	poolGroupCmd.PersistentFlags().StringVar(&poolRunID, "run-id", "", "Run id identifying the pool (queue) — shared by all commands")

	poolCreateCmd.Flags().IntVar(&poolWorkers, "workers", 0, "Number of worker instances to provision")
	poolCreateCmd.Flags().IntVar(&poolMinViable, "min-viable", 1, "Minimum workers that must come up (best-effort: ask N, accept >= this)")
	poolCreateCmd.Flags().StringVar(&poolInstanceType, "instance-type", "", "Worker instance type (e.g. c7i.large)")
	poolCreateCmd.Flags().BoolVar(&poolSpot, "spot", false, "Launch workers as Spot instances")
	poolCreateCmd.Flags().StringVar(&poolTTL, "ttl", "4h", "Per-worker TTL backstop")
	poolCreateCmd.Flags().DurationVar(&poolIdleTimeout, "idle-timeout", 5*time.Minute, "Workers drain after this long with an empty queue")
	poolCreateCmd.Flags().IntVar(&poolVisTimeout, "visibility-timeout", 900, "SQS claim window in seconds (must exceed the longest task runtime)")

	poolSubmitCmd.Flags().StringVar(&poolSpecPath, "spec", "", "Path to a TaskSpec JSON file to enqueue")
}
