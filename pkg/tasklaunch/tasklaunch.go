// Package tasklaunch is how spawn plans and launches one TaskSpec: the code
// behind `spawn task run` and `spawn task run --dry-run`, as a library, so
// that every front end launches a task through the same code.
//
// It exists because spore-host-mcp re-implemented this path and drifted from
// the CLI. It called launcher.Provision with an empty Options{}, so its
// launches had no storage mounts and no terminal-flush hook (the spawn#1144 bug
// class), and it never fell back to On-Demand on a Spot capacity error
// (spore-host-mcp#61). It also sized with spec.Resources rather than
// spec.SizingRequest(), which skipped the container memory floor
// (spore-host-mcp#60, spawn#1180).
//
// The API is three calls. Plan sizes the task and checks that it can launch,
// without creating anything. Render prints a plan as the CLI's dry run does.
// Launch plans and then launches. Every AWS dependency is an interface in Deps,
// so tests, and any embedding program, can substitute fakes. NewDeps builds
// the real ones from an *aws.Client.
package tasklaunch

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/google/uuid"
	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/launcher"
	"github.com/spore-host/spawn/pkg/taskiam"
	"github.com/spore-host/spawn/pkg/taskproto"
)

// AWS is the account-side work a task launch does besides RunInstances:
// resolve the account, ensure the results bucket, clear a previous attempt's
// records, create the scoped instance profile, look up an FSx filesystem for
// the mount script, and describe an instance type's memory. *aws.Client
// satisfies it.
type AWS interface {
	GetAccountID(ctx context.Context) (string, error)
	CreateS3BucketIfNotExists(ctx context.Context, bucketName, region string) error
	DeleteS3Object(ctx context.Context, region, bucket, key string) error
	CreateOrGetInstanceProfile(ctx context.Context, config aws.IAMRoleConfig) (string, error)
	GetFSxFilesystem(ctx context.Context, filesystemID, region string) (*aws.FSxInfo, error)
	InstanceMemoryMiB(ctx context.Context, region, instanceType string) (int64, bool)
}

// Provisioner launches one instance. LauncherProvisioner is the real one;
// Launch calls it once, or twice when a Spot attempt fails for capacity and
// the spec asks for an On-Demand fallback, with the same Options both times.
type Provisioner interface {
	Provision(ctx context.Context, cfg aws.LaunchConfig, opts launcher.Options) (*aws.LaunchResult, error)
}

// ProvisionerFunc adapts a function to Provisioner.
type ProvisionerFunc func(ctx context.Context, cfg aws.LaunchConfig, opts launcher.Options) (*aws.LaunchResult, error)

// Provision calls f.
func (f ProvisionerFunc) Provision(ctx context.Context, cfg aws.LaunchConfig, opts launcher.Options) (*aws.LaunchResult, error) {
	return f(ctx, cfg, opts)
}

// LauncherProvisioner provisions through launcher.Provision with client,
// passing on the Options it is given.
func LauncherProvisioner(client *aws.Client) Provisioner {
	return ProvisionerFunc(func(ctx context.Context, cfg aws.LaunchConfig, opts launcher.Options) (*aws.LaunchResult, error) {
		return launcher.Provision(ctx, client, cfg, opts)
	})
}

// Deps are what Plan and Launch need from outside. Plan uses Finder, and AWS
// only as a fallback when a pinned type's sizing lookup failed. Launch uses
// all three.
type Deps struct {
	Finder      taskproto.InstanceFinder
	AWS         AWS
	Provisioner Provisioner

	// Out receives launch progress, namely the Spot-to-On-Demand fallback
	// notice. Warn receives warnings: the plan's Warnings, and a failure to
	// clear a previous attempt's completion record. Verbose receives detail
	// the CLI prints under -v. A nil writer discards.
	Out, Warn, Verbose io.Writer
}

// NewDeps returns the real dependencies for region: spawn's AWS-backed
// finder (NewFinder), client for the account-side calls, and
// launcher.Provision with client. Progress and warnings, including the label
// on any static price the finder ranked on, go to warn, which may be nil.
func NewDeps(client *aws.Client, region string, warn io.Writer) Deps {
	return Deps{
		Finder:      NewFinder(client.Config(), region, warn, nil),
		AWS:         client,
		Provisioner: LauncherProvisioner(client),
		Out:         warn,
		Warn:        warn,
	}
}

// Result is a launched task.
type Result struct {
	// Plan is the plan the task was launched from.
	Plan *TaskPlan

	TaskID           string
	InstanceID       string
	Region           string
	AvailabilityZone string
	InstanceType     string
	// Spot is the purchase option that actually launched: false after an
	// On-Demand fallback, even when the spec asked for Spot.
	Spot bool
	// RunID identifies this attempt. The wrapper stamps it into the
	// completion record, so a record from a previous attempt of the same
	// task_id can be told apart (spawn#608).
	RunID string
	// ResultsPrefix is the s3://bucket/prefix the task's records are written
	// under. The completion record is ResultsPrefix + "/completion.json", that
	// is, CompletionKey(ResultsKeyPrefix) in ResultsBucket.
	ResultsPrefix    string
	ResultsBucket    string
	ResultsKeyPrefix string
}

// Launch plans spec (see Plan) and launches it.
//
// In order, it: refuses a spec whose user data is over EC2's limit, before
// any AWS call (#1063); resolves the results prefix and creates spawn's own
// results bucket if needed; mints a run id and clears a previous attempt's
// records (#608) and staged files (#1274); creates an instance profile scoped
// to the spec's buckets (plus ecr:ReadOnly for a private-ECR image); builds
// the boot-time storage mount script, the wrapper and the terminal-flush hook
// (#632); re-checks the exact user data; from a release build, uploads the
// wrapper and flush hook to the results prefix, so the user data carries only
// a verified reference to them (#1274); and provisions. A Spot launch that fails for capacity
// with resources.fallback "on_demand" is retried once On-Demand with the same
// config and Options (#1144).
func Launch(ctx context.Context, deps Deps, spec *taskproto.TaskSpec, region string) (*Result, error) {
	// Plan measures the user data before any AWS call that creates something
	// (or any at all, beyond sizing): a spec whose user data is over EC2's
	// limit cannot launch (#1063).
	plan, err := Plan(ctx, deps, spec, region)
	if err != nil {
		return nil, err
	}
	if deps.AWS == nil || deps.Provisioner == nil {
		return nil, errors.New("tasklaunch: Launch needs Deps.AWS and Deps.Provisioner")
	}
	client := deps.AWS
	warn := orDiscard(deps.Warn)
	for _, w := range plan.Warnings {
		fmt.Fprint(warn, w)
	}
	sized := plan.Sized

	account, err := client.GetAccountID(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve account id: %w", err)
	}
	// One resolution of where this task's records go, shared by the wrapper, the
	// flush hook, the instance policy and the --wait poll (spawn#646). A spec's
	// results_prefix overrides it; empty keeps
	// s3://spawn-results-<account>-<region>/tasks/<task_id>.
	resultsPrefix := taskproto.EffectiveResultsPrefix(spec, account, region)
	resultsBucket, resultsKeyPrefix, ok := taskproto.SplitS3URI(resultsPrefix)
	if !ok {
		return nil, fmt.Errorf("results_prefix %q is not an s3://bucket/prefix URI", spec.ResultsPrefix)
	}

	// Create the bucket only when it is the one spawn manages. A caller who names
	// their own bucket gets an error if it does not exist, rather than having a
	// stray bucket created from a typo and the run reported as a success.
	if taskproto.IsDefaultResultsBucket(resultsBucket, account, region) {
		if err := client.CreateS3BucketIfNotExists(ctx, resultsBucket, region); err != nil {
			return nil, fmt.Errorf("ensure results bucket %s: %w", resultsBucket, err)
		}
	}

	// Run identity for THIS attempt (spawn#608). Re-running a task_id overwrites
	// the same tasks/<task_id>/completion.json key, so without an attempt id a
	// record from a previous attempt is indistinguishable from this one's — and
	// --wait returned it as the answer while the new instance was still running,
	// reporting a fixed task as still failing. The id is stamped into the record by
	// the wrapper and checked by the CLI's pollCompletion.
	runID := uuid.NewString()

	// Belt (the structural half of the fix): clear the previous attempt's
	// completion artifacts BEFORE the instance can exist, so there is no stale
	// record to mis-read in the first place. Best-effort by design — a delete
	// failure must not abort a launch, because the run-id check is the
	// suspenders that catches whatever survives. Deleting an absent key is a
	// success in S3 (the overwhelmingly common first-run case), so this is silent
	// unless something actually went wrong.
	if err := clearStaleCompletion(ctx, client, region, resultsBucket, resultsKeyPrefix); err != nil {
		fmt.Fprintf(warn, "warning: could not clear a previous attempt's completion record for task %s: %v\n", spec.TaskID, err)
		fmt.Fprintf(warn, "         (harmless: --wait verifies run_id, so a leftover record is ignored rather than reported)\n")
	} else if deps.Verbose != nil {
		fmt.Fprintf(deps.Verbose, "cleared any previous completion record for task %s (run_id %s)\n", spec.TaskID, runID)
	}
	// And a previous attempt's staged wrapper and flush hook (#1274): each
	// attempt stages its own, under a name that is its sha256, so nothing
	// overwrites the last one.
	stage := stagesFiles(client)
	if stage {
		if err := clearStaleStagedFiles(ctx, client.(stagedFilesClearer), region, resultsBucket, resultsKeyPrefix); err != nil {
			fmt.Fprintf(warn, "warning: could not clear a previous attempt's staged task files for task %s: %v\n", spec.TaskID, err)
		}
	}

	// Scoped instance profile: the default spored role has no S3 write, so grant
	// exactly the buckets this task reads (inputs) and writes (outputs + results).
	// A private-ECR container image additionally needs ecr:ReadOnly to pull.
	profile, err := client.CreateOrGetInstanceProfile(ctx, aws.IAMRoleConfig{
		TrustServices:    []string{"ec2"}, // an instance profile is assumed by EC2
		InlinePolicyJSON: taskiam.TaskRunPolicyFor(taskiam.S3Buckets(spec.Inputs), taskiam.S3Buckets(spec.Outputs), resultsBucket, taskiam.S3BucketsFromURIs(spec.Resources.S3ReadWrite)),
		Policies:         taskiam.ExtraPolicies(spec),
	})
	if err != nil {
		return nil, fmt.Errorf("create task instance profile: %w", err)
	}

	// Build the storage mount script for any FSx/EFS/attached volumes the task
	// placement requests (mounted before the workload; #166). Empty when none.
	storageScript, err := taskStorageScript(ctx, client, spec, region)
	if err != nil {
		return nil, fmt.Errorf("build task storage: %w", err)
	}

	cfg, opts, err := buildTaskLaunch(spec, sized, region, profile, resultsPrefix, runID, storageScript)
	if err != nil {
		return nil, err
	}
	var staged *stagedObject
	if stage {
		staged = stageTaskFiles(cfg.JobArrayCommand, &opts, region, resultsBucket, resultsKeyPrefix)
	}
	// The exact user data, now that the account, run id and storage are real.
	// The preflight in Plan measured it with same-length placeholders; this is
	// the authoritative check, still before RunInstances (#1063).
	if _, err := taskUserDataSize(cfg, opts); err != nil {
		return nil, err
	}
	if staged != nil {
		if opts, err = uploadStagedFiles(ctx, client.(stagedFilesStore), warn, region, staged, cfg, opts); err != nil {
			return nil, err
		}
	}

	result, err := provisionTask(orDiscard(deps.Out), func(c aws.LaunchConfig, o launcher.Options) (*aws.LaunchResult, error) {
		return deps.Provisioner.Provision(ctx, c, o)
	}, &cfg, opts, spec.Resources.Fallback)
	if err != nil {
		return nil, fmt.Errorf("launch task: %w", err)
	}

	return &Result{
		Plan:             plan,
		TaskID:           spec.TaskID,
		InstanceID:       result.InstanceID,
		Region:           result.Region,
		AvailabilityZone: result.AvailabilityZone,
		InstanceType:     sized.InstanceType,
		Spot:             cfg.Spot,
		RunID:            runID,
		ResultsPrefix:    resultsPrefix,
		ResultsBucket:    resultsBucket,
		ResultsKeyPrefix: resultsKeyPrefix,
	}, nil
}

func orDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}
