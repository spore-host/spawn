package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/tasklaunch"
	"github.com/spore-host/spawn/pkg/taskproto"
	ttlpkg "github.com/spore-host/spawn/pkg/ttl"
)

// The `spawn task` group aggregates the scattered signals about a single
// launched instance — identity, lifecycle state, cost estimate, and where its
// logs live — into one "why did this go wrong / what happened" view. It composes
// data spawn already has (InstanceInfo + spawn:* tags); it does NOT open an SSH
// session in the base path, so it works even when the instance is unreachable
// (the log pointers tell you where to look once you can connect).

var taskDiagnoseJSON bool

var taskGroupCmd = &cobra.Command{
	Use:   "task",
	Short: "Run tasks from a TaskSpec, read their completion records, and diagnose them",
	Long: `Work with a single launched instance as a "task".

  spawn task run --spec <file>             launch a task from a TaskSpec (stage, run, completion record)
  spawn task status <task-id>              read a task's durable completion record from S3
  spawn task diagnose <name|instance-id>   one-screen summary + likely cause`,
}

// logPaths are the on-instance locations diagnose points at (see
// cmd/spored/paths_other.go and pkg/launcher/bootstrap.go).
const (
	sporedLogRemotePath  = "/var/log/spored.log"
	commandLogRemotePath = "/var/log/spawn-command.log"
)

// taskDiagnosis is the structured form (for --output json).
type taskDiagnosis struct {
	Name         string  `json:"name"`
	InstanceID   string  `json:"instance_id"`
	InstanceType string  `json:"instance_type"`
	State        string  `json:"state"`
	Region       string  `json:"region"`
	AZ           string  `json:"availability_zone,omitempty"`
	Spot         bool    `json:"spot"`
	AgeHours     float64 `json:"age_hours"`
	TTL          string  `json:"ttl,omitempty"`
	EstCostUSD   float64 `json:"estimated_cost_usd,omitempty"`
	IAMRole      string  `json:"iam_instance_profile,omitempty"`
	LikelyCause  string  `json:"likely_cause,omitempty"`
}

var taskDiagnoseCmd = &cobra.Command{
	Use:   "diagnose <name|instance-id>",
	Short: "Summarize an instance's state, cost, and likely failure cause",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		client, err := aws.NewClient(ctx)
		if err != nil {
			return fmt.Errorf("init AWS client: %w", err)
		}
		inst, err := resolveInstance(ctx, client, args[0])
		if err != nil {
			return err
		}

		// Age is since the ORIGINAL launch and the cost is rate × compute time
		// across every boot, not time since EC2's LaunchTime (the latest start,
		// #1029).
		rt := runtimeSoFar(inst, time.Now())
		age := rt.Since
		d := taskDiagnosis{
			Name:         inst.Name,
			InstanceID:   inst.InstanceID,
			InstanceType: inst.InstanceType,
			State:        inst.State,
			Region:       inst.Region,
			AZ:           inst.AvailabilityZone,
			Spot:         inst.SpotInstance,
			AgeHours:     age.Hours(),
			TTL:          inst.TTL,
			EstCostUSD:   estimateInstanceCost(inst, rt.Compute),
			IAMRole:      inst.IAMRole,
			LikelyCause:  likelyCause(inst),
		}

		if taskDiagnoseJSON || getOutputFormat() == "json" {
			return json.NewEncoder(os.Stdout).Encode(d)
		}

		fmt.Printf("Task:        %s (%s)\n", orDash(d.Name), d.InstanceID)
		fmt.Printf("Type:        %s%s\n", d.InstanceType, spotSuffix(d.Spot))
		fmt.Printf("State:       %s\n", d.State)
		fmt.Printf("Region/AZ:   %s / %s\n", d.Region, orDash(d.AZ))
		fmt.Printf("IAM profile: %s\n", orDash(d.IAMRole))
		fmt.Printf("Age:         %s\n", age.Round(time.Minute))
		if d.TTL != "" {
			fmt.Printf("TTL:         %s\n", d.TTL)
		}
		if d.EstCostUSD > 0 {
			fmt.Printf("Est. cost:   ~$%.2f (%s compute × $%.4f/hr, estimate)\n", d.EstCostUSD, rt.Compute.Round(time.Minute), pricePerHour(inst))
		}
		if jaName := inst.JobArrayName; jaName != "" {
			fmt.Printf("Job array:   %s (index %s of %s)\n", jaName, orDash(inst.JobArrayIndex), orDash(inst.JobArraySize))
		}
		if inst.SweepName != "" {
			fmt.Printf("Sweep:       %s\n", inst.SweepName)
		}
		if d.LikelyCause != "" {
			fmt.Printf("\nLikely: %s\n", d.LikelyCause)
		}

		fmt.Printf("\nLogs (on the instance):\n")
		fmt.Printf("  spored:   %s\n", sporedLogRemotePath)
		fmt.Printf("  command:  %s\n", commandLogRemotePath)
		fmt.Printf("  fetch:    spawn connect %s -- tail -n 200 %s\n", cliName(inst), commandLogRemotePath)
		if inst.SweepName != "" {
			fmt.Printf("  cost:     spawn cost %s\n", inst.SweepName)
		}
		return nil
	},
}

// estimateInstanceCost gives a rough compute-only cost from the price-per-hour
// tag spawn stamps at launch × a compute duration (runtimeSoFar). Standalone instances have no
// DynamoDB cost record (that's keyed by sweep-id), so this is an on-the-fly
// estimate, clearly labeled as such — not a billing figure. Returns 0 if the
// rate is unknown.
func estimateInstanceCost(inst *aws.InstanceInfo, age time.Duration) float64 {
	rate := pricePerHour(inst)
	if rate <= 0 || age <= 0 {
		return 0
	}
	return rate * age.Hours()
}

func pricePerHour(inst *aws.InstanceInfo) float64 {
	if v := inst.Tags["spawn:price-per-hour"]; v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return 0
}

// likelyCause offers a small, clearly-hedged hint from state + tags. It never
// claims certainty — the logs are authoritative.
func likelyCause(inst *aws.InstanceInfo) string {
	switch inst.State {
	case "terminated":
		if inst.SpotInstance {
			return "instance is terminated — if this was unexpected, a Spot interruption is a common cause (check the spored log for a spot_interrupt event)."
		}
		return "instance is terminated — likely its TTL elapsed or the workload completed with --on-complete terminate."
	case "stopped":
		return "instance is stopped — likely an idle-timeout stop, or a manual/queued stop. It has not been billed for compute while stopped."
	case "running":
		return "" // nothing wrong to explain
	}
	return ""
}

func spotSuffix(spot bool) string {
	if spot {
		return " (spot)"
	}
	return ""
}

// cliName returns the identifier to use in a follow-up `spawn connect` — prefer
// the human name, fall back to the instance ID.
func cliName(inst *aws.InstanceInfo) string {
	if inst.Name != "" {
		return inst.Name
	}
	return inst.InstanceID
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ── task run ─────────────────────────────────────────────────────────────────

var (
	taskRunSpecPath         string
	taskRunDryRun           bool
	taskRunRegion           string
	taskRunWait             bool
	taskRunPollInterval     time.Duration
	taskRunAMI              string
	taskStatusRegion        string
	taskStatusResultsPrefix string
	taskStatusCheckDone     bool
)

var taskRunCmd = &cobra.Command{
	Use:   "run --spec <file>",
	Short: "Launch a task from a TaskSpec (stage → run → durable completion record)",
	Long: `Run a task described by a TaskSpec JSON file (the shared workflow-adapter
contract, spawn#386).

Sizing picks the cheapest type that fits, and on a price TIE the smallest that
fits — which matters on a family like hpc7g where every size costs the same
because you rent the socket rather than the cores, so a 16-vCPU request returns
hpc7g.4xlarge and not the 64-vCPU hpc7g.16xlarge (spawn#610).

To choose the type yourself, set resources.instance_type in the spec: that pins
it exactly and skips candidate search and price ranking altogether (cpu,
memory_gib, architecture and families are then ignored). That is the way to sweep
several sizes of one family deliberately, since cpu/memory can't distinguish
equal-spec sizes. resources.families narrows sizing to an allow-list without
pinning a single type.

Once the type is chosen, spawn launches an ephemeral instance that stages inputs
from S3, runs the command, stages outputs back, and writes a durable completion
record to
s3://spawn-results-<account>-<region>/tasks/<task_id>/completion.json — the
signal workflow adapters poll. The instance self-terminates on completion (TTL +
on_complete).

If spec.container is set, the command runs inside that image (Docker is installed
on demand; a private-ECR image is pulled with an ecr:ReadOnly grant, GPUs passed
with --gpus all). Otherwise it runs on the host. A container task is sized to at
least 2 GiB of memory even when resources.memory_gib is lower or unset, because
the Docker install is OOM-killed on smaller boxes; a pinned
resources.instance_type is honoured as given, with a warning if it is below that.
If Docker cannot be installed or its daemon does not start, the record reports
retry_class instance_health (retryable), not app_error.

A container gets Docker's default 64 MB /dev/shm, which many-rank MPI (Open MPI's
shared-memory transport), PyTorch DataLoader workers and Ray outgrow: an Open MPI
run with 64 ranks on one box dies with SIGBUS in its first collective. Set
resources.shm_gib to raise it (passed as docker run --shm-size=<n>g). It must be
a positive number of GiB, needs a container, and may not exceed the instance's
memory: memory_gib with headroom (or the 2 GiB container floor) for a sized spec,
the type's memory for a pinned instance_type. /dev/shm is RAM-backed, so size
memory_gib to cover it as well as the workload.

What gets bind-mounted, and as whom: spawn pre-creates the parent directory of
every inputs[].destination and outputs[].source — plus any EFS/FSx/volume mount
point from placement — as the instance user, and bind-mounts exactly those paths
into the container. The container then runs as that same uid:gid rather than the
image's declared USER (spawn#555), so the staged directories are writable
whatever the image happens to declare; every staged input is also chown'd to that
uid so it can be deleted as well as read (spawn#565). Practical upshot: any
absolute path works, the image's USER is irrelevant, and you do not need to keep
staged paths flat in /tmp. A manifest path ending in "/" is a directory and is
staged recursively in both directions (spawn#564). A directory input that copies
no objects (its prefix is empty or missing, often because the task that writes
it failed) fails stage-in as staging_error; set allow_empty: true on that input
if an empty directory is expected (spawn#1207).

A container's /tmp is its own tmpfs (docker run --tmpfs), RAM-backed and capped
at half of instance memory like the host's AL2023 /tmp, so scratch written there
uses memory, not the root disk; staged directories under /tmp are mounted on top
of it. The exception is a manifest path directly in /tmp (such as /tmp/run.sh):
then the host's /tmp is bind-mounted instead, as before (spawn#1270). command.log
says which.

A workflow engine that starts its own task containers (Cromwell's local
backend, miniwdl, Nextflow with docker.enabled) can run as a container task with
container_docker_socket: true. It bind-mounts the host's /var/run/docker.sock
into the container and adds the socket's group; --user is unchanged. This is
ROOT-EQUIVALENT ON THE INSTANCE: anything that can reach dockerd can start a
privileged container that mounts the host's /, so set it only for an image you
trust. It needs container, and is refused for a pooled task, where one task
could control the others on the shared worker. The containers the engine starts
are siblings on the host, so paths it hands them must be host paths: use the
staged or placement-mounted directories, not the engine container's own /tmp.
The provenance record notes the socket, and such a run is never proven
regenerable, because the engine's images are not recorded (spawn#1231).

Staging space is NOT resources.disk_gib unless you stage onto the root
filesystem. disk_gib sizes the root EBS volume, which is where an ordinary path
like /work lives — so that is the knob for a large staged input. /tmp is
different: on AL2023 it is a tmpfs, RAM-backed and capped near half of instance
memory by default, so staging a 40 GiB index into /tmp fails on a 32 GiB box no
matter how large disk_gib is. Stage big inputs to a non-/tmp path and size
disk_gib, or keep /tmp and size memory_gib for the staging footprint — the
latter tends to pick the instance FAMILY rather than just its size, so the two
are not interchangeable on cost.

The launch AMI is auto-selected from the sized instance type: a GPU family (g5,
g6, p4/p5, …) gets the AL2023 NVIDIA DLAMI so --gpus all lands on a host with a
driver (spawn#601), and everything else gets the standard AL2023 for the type's
architecture. Pin a specific AMI with --ami (or placement.ami in the spec); an
explicit AMI always wins over auto-selection.

A task launches into the region's default VPC unless the spec names a subnet
with placement.subnet_id, as --subnet-id does for spawn launch. In a region with
no default VPC, usual for Control Tower and Landing Zone Accelerator accounts, a
spec without one fails before launching and lists subnets to use (spawn#1267).

Re-running the same task_id is safe with --wait: the results of the previous
attempt are cleared at launch, and every run stamps a run_id into its completion
record, so --wait ignores any record that isn't from the run it just launched
instead of reporting an earlier attempt's verdict (#608).

--dry-run sizes and prints the plan without launching.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if taskRunSpecPath == "" {
			return fmt.Errorf("--spec is required")
		}
		spec, err := taskproto.ParseSpecFile(taskRunSpecPath)
		if err != nil {
			return err
		}
		// An explicit --ami wins over auto-selection AND over any placement.ami in
		// the spec: the operator typed it now, so honor it verbatim (parity with
		// `spawn launch --ami`). Left unset, the sized-instance auto-selection below
		// applies (GPU DLAMI for a GPU family, standard AL2023 otherwise, spawn#601).
		if taskRunAMI != "" {
			spec.Placement.AMI = taskRunAMI
		}
		awsClient, err := aws.NewClient(cmd.Context())
		if err != nil {
			return fmt.Errorf("init AWS client: %w", err)
		}
		region := taskRunRegion
		if region == "" {
			region = awsClient.Config().Region
		}
		if region == "" {
			return fmt.Errorf("no region: pass --region or configure a default AWS region")
		}
		finder := tasklaunch.NewFinder(awsClient.Config(), region, os.Stderr, verboseWriter())
		// Planning and launching are pkg/tasklaunch, the code spore-host-mcp
		// launches through too, so the two cannot drift (spore-host-mcp#60/#61).
		if taskRunDryRun {
			return renderTaskDryRunWith(cmd.Context(), os.Stdout, taskDeps(awsClient, finder, os.Stdout), spec, region)
		}
		return runTaskReal(cmd.Context(), os.Stdout, awsClient, spec, finder, region)
	},
}

// verboseWriter is where -v detail goes: stderr under -v, nowhere otherwise.
func verboseWriter() io.Writer {
	if spawnVerbose {
		return os.Stderr
	}
	return nil
}

// taskDeps is the tasklaunch dependencies `spawn task run` uses: finder for
// sizing, client for everything else, launch progress to out and warnings to
// stderr.
func taskDeps(client *aws.Client, finder taskproto.InstanceFinder, out io.Writer) tasklaunch.Deps {
	deps := tasklaunch.Deps{
		Finder:  finder,
		Out:     out,
		Warn:    os.Stderr,
		Verbose: verboseWriter(),
	}
	// A nil client stays a nil interface rather than a typed nil.
	if client != nil {
		deps.AWS = client
		deps.Provisioner = tasklaunch.LauncherProvisioner(client)
	}
	return deps
}

// renderTaskDryRun plans the task with finder and prints the plan, the dry
// run. Split from the RunE (which builds the AWS-backed finder) so it's
// unit-testable with a fake finder — no AWS, no launch.
func renderTaskDryRun(ctx context.Context, out io.Writer, spec *taskproto.TaskSpec, finder taskproto.InstanceFinder, region string) error {
	return renderTaskDryRunWith(ctx, out, tasklaunch.Deps{Finder: finder}, spec, region)
}

// renderTaskDryRunWith is renderTaskDryRun with every dependency supplied:
// the plan's warnings go to deps.Warn, the plan to out.
func renderTaskDryRunWith(ctx context.Context, out io.Writer, deps tasklaunch.Deps, spec *taskproto.TaskSpec, region string) error {
	plan, err := tasklaunch.Plan(ctx, deps, spec, region)
	if err != nil {
		return err
	}
	if deps.Warn != nil {
		for _, w := range plan.Warnings {
			fmt.Fprint(deps.Warn, w)
		}
	}
	tasklaunch.Render(out, plan)
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Re-run without --dry-run to launch this task.")
	return nil
}

// runTaskReal launches a task for real through tasklaunch.Launch, which sizes
// the instance (same as dry-run), ensures the per-account results bucket exists,
// generates the on-instance wrapper (stage-in → command → stage-out → durable
// completion record), creates a scoped instance profile granting the S3 access
// the wrapper needs, and launches via launcher.Provision (keyless/SSM, auto
// AMI+IAM+user-data). It returns as soon as the instance is launched — the
// caller polls the S3 completion record (or passes --wait). When spec.Container
// is set the command runs inside that image (Docker is installed on demand; a
// private-ECR image gets an ecr:ReadOnly grant on the scoped profile).
func runTaskReal(ctx context.Context, out io.Writer, client *aws.Client, spec *taskproto.TaskSpec, finder taskproto.InstanceFinder, region string) error {
	launched, err := tasklaunch.Launch(ctx, taskDeps(client, finder, out), spec, region)
	if err != nil {
		return err
	}
	runID := launched.RunID
	resultsPrefix := launched.ResultsPrefix
	resultsBucket, resultsKeyPrefix := launched.ResultsBucket, launched.ResultsKeyPrefix

	lr := taskproto.LaunchResult{
		TaskID:       launched.TaskID,
		InstanceID:   launched.InstanceID,
		Region:       launched.Region,
		AZ:           launched.AvailabilityZone,
		InstanceType: launched.InstanceType,
		Spot:         launched.Spot,
	}
	jsonOut := getOutputFormat() == "json"

	// Without --wait we return at launch: JSON emits the LaunchResult; human prints
	// the launch summary + how to poll. (With --wait we fall through to the poll
	// below and, in JSON mode, emit the CompletionRecord instead — the one-shot
	// contract adapters want, spawn#386.)
	if !taskRunWait {
		if jsonOut {
			return json.NewEncoder(out).Encode(lr)
		}
		fmt.Fprintf(out, "✅ Task launched: %s\n", spec.TaskID)
		fmt.Fprintf(out, "Instance:     %s  (%s%s) in %s / %s\n", lr.InstanceID, lr.InstanceType, spotSuffix(lr.Spot), lr.Region, orDash(lr.AZ))
		fmt.Fprintf(out, "Run ID:       %s\n", runID)
		fmt.Fprintf(out, "TTL:          %s   on-complete: %s\n", spec.Lifecycle.TTL, spec.EffectiveOnComplete())
		fmt.Fprintf(out, "Completion:   %s/completion.json\n", resultsPrefix)
		fmt.Fprintf(out, "\nPoll for completion:\n")
		fmt.Fprintf(out, "  spawn task status %s --region %s\n", spec.TaskID, region)
		fmt.Fprintf(out, "  aws s3 cp %s/completion.json -\n", resultsPrefix)
		return nil
	}

	// --wait: poll the completion record until it appears or the TTL elapses. In
	// JSON mode emit the CompletionRecord verbatim (so an adapter gets the full
	// record from one `task run --wait -o json` call); in human mode print the
	// launch summary then the record. Either way exit with waitExitCode: the
	// task's exit code, or 1 if it did not complete (#1143).
	if !jsonOut {
		fmt.Fprintf(out, "✅ Task launched: %s\n", spec.TaskID)
		fmt.Fprintf(out, "Instance:     %s  (%s%s) in %s / %s\n", lr.InstanceID, lr.InstanceType, spotSuffix(lr.Spot), lr.Region, orDash(lr.AZ))
		fmt.Fprintf(out, "Run ID:       %s\n", runID)
		fmt.Fprintf(out, "TTL:          %s   on-complete: %s\n", spec.Lifecycle.TTL, spec.EffectiveOnComplete())
		fmt.Fprintf(out, "Completion:   %s/completion.json\n", resultsPrefix)
		fmt.Fprintf(out, "\nWaiting for completion (polling every %s)...\n", taskRunPollInterval)
	}
	deadline := waitDeadline(spec.Lifecycle.TTL)
	// runID is passed so a record from a previous attempt of this task_id is
	// ignored rather than reported (spawn#608). Notices go to stderr, never to out:
	// in -o json mode out carries only the CompletionRecord an adapter parses.
	rec, err := pollCompletion(ctx, client, region, resultsBucket, resultsKeyPrefix, spec.TaskID, runID, taskRunPollInterval, deadline, os.Stderr)
	if err != nil {
		return err
	}
	if jsonOut {
		if err := json.NewEncoder(out).Encode(rec); err != nil {
			return err
		}
	} else {
		printCompletion(out, rec)
	}
	if code := waitExitCode(rec); code != 0 {
		os.Exit(code)
	}
	return nil
}

// waitExitCode is the process exit code for `task run --wait` once the
// completion record has arrived: the task's own exit code when it is non-zero,
// otherwise 1 for any state other than completed.
//
// The exit code alone is not enough (#1143). When the command succeeds but a
// declared output fails to upload, the wrapper writes state=failed,
// retry_class=output_delivery_error and the COMMAND's exit_code, which is 0.
// Exiting with that 0 let a script that trusts the exit status treat a task with
// missing outputs as a success. 1 matches `task status --check-complete`, which
// already exits 1 for a failed record.
func waitExitCode(rec *taskproto.CompletionRecord) int {
	if rec.ExitCode != 0 {
		return rec.ExitCode
	}
	if rec.State != taskproto.StateCompleted {
		return 1
	}
	return 0
}

// waitDeadline returns now+TTL as the wait cutoff, defaulting to a generous cap
// if the TTL can't be parsed (validation should have caught that).
func waitDeadline(ttl string) time.Time {
	d, err := ttlpkg.ParseDuration(ttl)
	if err != nil || d <= 0 {
		d = 24 * time.Hour
	}
	// A little slack past the TTL so a task that runs right up to its deadline
	// still has its record observed before we give up.
	return time.Now().Add(d + 2*time.Minute)
}

// taskResultStore is the narrow S3 surface the task-run/task-status paths need:
// read a task's completion record and clear a previous attempt's before launch.
// *aws.Client satisfies it; a fake satisfies it in tests, which is the whole
// point — the stale-record logic (spawn#608) is the part worth testing and it
// must be testable with zero AWS.
type taskResultStore interface {
	GetS3Object(ctx context.Context, region, bucket, key string) ([]byte, error)
	DeleteS3Object(ctx context.Context, region, bucket, key string) error
}

// completionKey is the object key of a task's completion record, relative to
// the results bucket, given the key portion of its results prefix. It is
// tasklaunch.CompletionKey, which the launch's stale-record clear uses too, so
// the reader and the clear cannot disagree about the key (#715).
func completionKey(taskPrefix string) string {
	return tasklaunch.CompletionKey(taskPrefix)
}

// completionRunVerdict is pollCompletion's decision about a completion record it
// just fetched: does this record describe the run we launched?
type completionRunVerdict int

const (
	// completionForThisRun — the record's run_id is ours. Report it.
	completionForThisRun completionRunVerdict = iota
	// completionPreviousRun — the record carries a DIFFERENT run_id, so it is a
	// previous attempt's record sitting at the shared key. Keep polling; reporting
	// it is exactly the false negative spawn#608 describes.
	completionPreviousRun
	// completionUnattributed — the record has NO run_id, so it cannot be attributed
	// to any attempt. See classifyCompletionRun for why we accept it anyway.
	completionUnattributed
)

// classifyCompletionRun compares a fetched record's run identity to the run we
// launched (spawn#608).
//
// The empty-run_id case is the judgement call. An empty run_id means the record
// was written by a pre-#608 wrapper — there is no such thing as a current wrapper
// that omits it (the wrapper emits the field unconditionally, and the pooled path
// mints its own id). We ACCEPT it, loudly, rather than treating it as a mismatch:
//
//   - We cannot verify it, but we already deleted both result keys immediately
//     before launch, so a record present now is overwhelmingly likely to be this
//     run's — written by an older spored/AMI-cached wrapper, not left over.
//   - Treating unverifiable as "keep polling" would hang every legitimate old-
//     wrapper run until its TTL and then fail it, converting a rare misattribution
//     risk into a guaranteed outage for anyone on a cached wrapper. A wrong answer
//     that is announced beats a timeout that is not.
//
// So the warning is the contract here: the caller must tell the user the record
// predates run-id stamping and cannot be attributed to this attempt.
//
// runID == "" means the CALLER has no run identity (nothing in-tree does today;
// --wait always mints one). Such a caller gets the pre-#608 behavior — accept
// whatever is at the key — because it has nothing to compare against.
func classifyCompletionRun(rec *taskproto.CompletionRecord, runID string) completionRunVerdict {
	if rec == nil {
		return completionPreviousRun // nothing to report; keep waiting
	}
	if runID == "" {
		return completionForThisRun
	}
	if rec.RunID == "" {
		return completionUnattributed
	}
	if rec.RunID == runID {
		return completionForThisRun
	}
	return completionPreviousRun
}

// pollCompletion fetches the completion record, retrying until a record for THIS
// run (runID) appears or the deadline passes. A record left by a previous attempt
// of the same task_id is skipped, not returned (spawn#608) — that record is a real
// record of a real earlier run, which is exactly why returning it reads as "your
// fix didn't work". Notices (skipped stale record, unattributable record) are
// written to warn, which must not be the machine-readable output stream.
func pollCompletion(ctx context.Context, store taskResultStore, region, resultsBucket, keyPrefix, taskID, runID string, every time.Duration, deadline time.Time, warn io.Writer) (*taskproto.CompletionRecord, error) {
	if every <= 0 {
		every = 15 * time.Second
	}
	if warn == nil {
		warn = io.Discard
	}
	var notedStale bool
	for {
		rec, _, err := tasklaunch.ReadCompletionAt(ctx, store, region, resultsBucket, keyPrefix)
		present := err == nil
		if err != nil && !errors.Is(err, tasklaunch.ErrNoCompletionRecord) {
			return nil, err
		}
		if present {
			switch classifyCompletionRun(rec, runID) {
			case completionForThisRun:
				return rec, nil
			case completionUnattributed:
				fmt.Fprintf(warn, "⚠ task %s: the completion record carries no run_id, so it cannot be attributed to this run (%s).\n", taskID, runID)
				fmt.Fprintf(warn, "  It was written by a wrapper that predates run-id stamping; accepting it. If it looks like an older attempt, re-check s3://%s/%s.\n", resultsBucket, completionKey(keyPrefix))
				return rec, nil
			case completionPreviousRun:
				if !notedStale {
					notedStale = true
					fmt.Fprintf(warn, "ℹ task %s: ignoring a completion record from a previous run (run_id %s; this run is %s) and continuing to wait.\n", taskID, rec.RunID, runID)
				}
			}
		}
		if time.Now().After(deadline) {
			if notedStale {
				return nil, fmt.Errorf("timed out waiting for task %q completion record for this run (run_id %s) (past TTL); the record at s3://%s/%s is from an earlier attempt", taskID, runID, resultsBucket, completionKey(keyPrefix))
			}
			return nil, fmt.Errorf("timed out waiting for task %q completion record (past TTL); poll later with 'spawn task status %s'", taskID, taskID)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(every):
		}
	}
}

// printCompletion renders a CompletionRecord as human text.
func printCompletion(out io.Writer, rec *taskproto.CompletionRecord) {
	fmt.Fprintf(out, "Task:       %s\n", rec.TaskID)
	// run_id makes "which attempt is this?" answerable from the printed record
	// itself (spawn#608) — omitted for a pre-#608 record, which has none.
	if rec.RunID != "" {
		fmt.Fprintf(out, "Run ID:     %s\n", rec.RunID)
	}
	fmt.Fprintf(out, "State:      %s\n", rec.State)
	fmt.Fprintf(out, "Exit code:  %d\n", rec.ExitCode)
	if rec.InstanceType != "" || rec.Purchase != "" {
		fmt.Fprintf(out, "Instance:   %s %s\n", orDash(rec.InstanceType), orDash(rec.Purchase))
	}
	if rec.StartedAt != "" {
		fmt.Fprintf(out, "Started:    %s\n", rec.StartedAt)
	}
	if rec.EndedAt != "" {
		fmt.Fprintf(out, "Ended:      %s\n", rec.EndedAt)
	}
	if rec.RetryClass != taskproto.RetryNone {
		fmt.Fprintf(out, "Retry:      %s (retryable=%v)\n", rec.RetryClass, rec.RetryClass.Retryable())
	}
	for _, l := range rec.Logs {
		fmt.Fprintf(out, "Log:        %s\n", l)
	}
}

// ── task status ──────────────────────────────────────────────────────────────

var taskStatusCmd = &cobra.Command{
	Use:   "status <task-id>",
	Short: "Show a task's durable completion record (from S3)",
	Long: `Read the completion record a 'spawn task run' task wrote to
s3://spawn-results-<account>-<region>/tasks/<task-id>/completion.json.

If the record isn't there, status asks EC2 about the task's instance: a live
instance means the task is still running; a terminated one with no record is
reported as terminated, never as running, with the outcome of spored's
terminal flush (the spawn:flush-status tag) while EC2 still lists the
instance, about an hour after it ends. A record is followed by a Flush: line.
With --check-complete, exit codes mirror 'spawn status': 0=completed,
1=failed (including terminated without a record), 2=running, 3=error
(including no record and no instance found).`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		taskID := args[0]
		client, err := aws.NewClient(ctx)
		if err != nil {
			return fmt.Errorf("init AWS client: %w", err)
		}
		region := taskStatusRegion
		if region == "" {
			region = client.Config().Region
		}
		if region == "" {
			return fmt.Errorf("no region: pass --region or configure a default AWS region")
		}
		// `task status` is given a task id, not a spec, so it cannot infer a
		// results_prefix the spec set (spawn#646). --results-prefix is how a caller
		// that moved its records tells status where to look; without it this stays
		// on spawn's default, which is where every existing task's records are.
		// tasklaunch.ReadCompletion is the one reader of completion records, which
		// the MCP server calls too (spawn#1260).
		rec, _, err := tasklaunch.ReadCompletion(ctx, client, taskID, taskStatusResultsPrefix, region)
		present := err == nil
		if err != nil && !errors.Is(err, tasklaunch.ErrNoCompletionRecord) {
			if taskStatusCheckDone {
				fmt.Fprintf(os.Stderr, "task status: %v\n", err)
				os.Exit(3)
			}
			return err
		}
		if !present {
			// No record is NOT proof the task is running: an instance killed
			// without spored's involvement never writes one (spawn#802). Ask
			// EC2 what became of the instance.
			code := reportTaskWithoutRecord(ctx, client.ListInstances, region, taskID, os.Stdout, taskStatusCheckDone)
			if taskStatusCheckDone {
				os.Exit(code)
			}
			if code == taskStatusExitError {
				return fmt.Errorf("task %s: no completion record, and its instance state could not be determined", taskID)
			}
			return nil
		}

		if taskStatusCheckDone {
			if rec.State == taskproto.StateFailed || rec.ExitCode != 0 {
				os.Exit(1)
			}
			os.Exit(0)
		}
		if getOutputFormat() == "json" {
			return json.NewEncoder(os.Stdout).Encode(rec)
		}
		printCompletion(os.Stdout, rec)
		printRecordFlushStatus(ctx, os.Stdout, client.ListInstances, region, taskID)
		return nil
	},
}

// taskCompletionFile is the on-instance file the task wrapper writes and spored
// watches to fire on_complete (tasklaunch.CompletionFile, spawn#406).
const taskCompletionFile = tasklaunch.CompletionFile

func init() {
	rootCmd.AddCommand(taskGroupCmd)
	taskGroupCmd.AddCommand(taskDiagnoseCmd)
	taskDiagnoseCmd.Flags().BoolVar(&taskDiagnoseJSON, "json", false, "Output as JSON")
	_ = taskDiagnoseCmd.Flags().MarkDeprecated("json", "use --output json instead")

	taskGroupCmd.AddCommand(taskRunCmd)
	taskRunCmd.Flags().StringVar(&taskRunSpecPath, "spec", "", "Path to a TaskSpec JSON file (required)")
	taskRunCmd.Flags().BoolVar(&taskRunDryRun, "dry-run", false, "Size and preview the task without launching")
	taskRunCmd.Flags().StringVar(&taskRunRegion, "region", "", "Region to size against (default: the configured AWS region)")
	taskRunCmd.Flags().StringVar(&taskRunAMI, "ami", "", "Pin a specific AMI (overrides auto-selection and placement.ami); empty = auto-select from the sized instance type (GPU DLAMI for GPU families)")
	taskRunCmd.Flags().BoolVar(&taskRunWait, "wait", false, "Block until the task's completion record appears, then exit with its exit code; a task whose state is not completed exits 1 even if its command exited 0 (e.g. a declared output failed to upload)")
	taskRunCmd.Flags().DurationVar(&taskRunPollInterval, "poll-interval", 15*time.Second, "How often to poll for completion when --wait is set")

	taskGroupCmd.AddCommand(taskStatusCmd)
	taskStatusCmd.Flags().StringVar(&taskStatusRegion, "region", "", "Region the task ran in (default: the configured AWS region)")
	taskStatusCmd.Flags().StringVar(&taskStatusResultsPrefix, "results-prefix", "", "s3://bucket/prefix the task's records were written under; required if the spec set results_prefix (default: spawn's own spawn-results-<account>-<region>/tasks)")
	taskStatusCmd.Flags().BoolVar(&taskStatusCheckDone, "check-complete", false, "Exit 0=completed, 1=failed, 2=running, 3=error instead of printing")
}
