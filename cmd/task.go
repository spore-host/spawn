package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/launcher"
	"github.com/spore-host/spawn/pkg/taskproto"
	truffleaws "github.com/spore-host/truffle/pkg/aws"
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
	Short: "Inspect and diagnose individual tasks (instances)",
	Long: `Work with a single launched instance as a "task".

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

		age := time.Since(inst.LaunchTime)
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
			EstCostUSD:   estimateInstanceCost(inst, age),
			LikelyCause:  likelyCause(inst),
		}

		if taskDiagnoseJSON || getOutputFormat() == "json" {
			return json.NewEncoder(os.Stdout).Encode(d)
		}

		fmt.Printf("Task:        %s (%s)\n", orDash(d.Name), d.InstanceID)
		fmt.Printf("Type:        %s%s\n", d.InstanceType, spotSuffix(d.Spot))
		fmt.Printf("State:       %s\n", d.State)
		fmt.Printf("Region/AZ:   %s / %s\n", d.Region, orDash(d.AZ))
		fmt.Printf("Age:         %s\n", age.Round(time.Minute))
		if d.TTL != "" {
			fmt.Printf("TTL:         %s\n", d.TTL)
		}
		if d.EstCostUSD > 0 {
			fmt.Printf("Est. cost:   ~$%.2f (%s × $%.4f/hr, estimate)\n", d.EstCostUSD, age.Round(time.Minute), pricePerHour(inst))
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
// tag spawn stamps at launch × the instance's age. Standalone instances have no
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
	taskRunSpecPath     string
	taskRunDryRun       bool
	taskRunRegion       string
	taskRunWait         bool
	taskRunPollInterval time.Duration
	taskStatusRegion    string
	taskStatusCheckDone bool
)

var taskRunCmd = &cobra.Command{
	Use:   "run --spec <file>",
	Short: "Launch a task from a TaskSpec (stage → run → durable completion record)",
	Long: `Run a task described by a TaskSpec JSON file (the shared workflow-adapter
contract, spawn#386).

Sizes the cheapest instance type that fits the resource request (via truffle),
then launches an ephemeral instance that stages inputs from S3, runs the command,
stages outputs back, and writes a durable completion record to
s3://spawn-results-<account>-<region>/tasks/<task_id>/completion.json — the
signal workflow adapters poll. The instance self-terminates on completion (TTL +
on_complete).

--dry-run sizes and prints the plan without launching. Container execution
(spec.container) is a follow-up increment — omit it to run on the host.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if taskRunSpecPath == "" {
			return fmt.Errorf("--spec is required")
		}
		spec, err := taskproto.ParseSpecFile(taskRunSpecPath)
		if err != nil {
			return err
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
		finder := truffleFinder{tc: truffleaws.NewClientFromConfig(awsClient.Config()), region: region}
		if taskRunDryRun {
			return renderTaskDryRun(cmd.Context(), os.Stdout, spec, finder, region)
		}
		return runTaskReal(cmd.Context(), os.Stdout, awsClient, spec, finder, region)
	},
}

// truffleFinder adapts truffle's SearchInstanceTypes to taskproto.InstanceFinder.
// It searches one region (region-spread is a later increment) and projects each
// result down to a taskproto.Candidate. Read-only: offerings + pricing only.
type truffleFinder struct {
	tc     *truffleaws.Client
	region string
}

func (f truffleFinder) FindCandidates(ctx context.Context, req taskproto.ResourceRequest) ([]taskproto.Candidate, error) {
	// Match all types; the resource minimums do the filtering. Family allow-list
	// is applied by the sizer (truffle's FilterOptions has only a single family).
	matcher := regexp.MustCompile(`.*`)
	opts := truffleaws.FilterOptions{
		Architecture: req.Architecture,
		MinVCPUs:     req.CPU,
		MinMemory:    taskproto.EffectiveMemoryGiB(req),
	}
	results, err := f.tc.SearchInstanceTypes(ctx, []string{f.region}, matcher, opts)
	if err != nil {
		return nil, err
	}
	cands := make([]taskproto.Candidate, 0, len(results))
	for _, r := range results {
		// SearchInstanceTypes does NOT populate on-demand price (that's a separate
		// pricing call), so r.OnDemandPrice is 0 here. The sizer ranks on price —
		// without it, "cheapest" degenerates to a name tie-break and can pick the
		// LARGEST type. So look the price up explicitly (API with a static
		// libs/pricing fallback). A lookup miss leaves price 0; the sizer sorts
		// those last, so a priced option always wins and we never silently pick a
		// huge box because pricing was unavailable.
		price := r.OnDemandPrice
		if price <= 0 {
			if p, perr := f.tc.OnDemandPrice(ctx, r.InstanceType, f.region); perr == nil {
				price = p
			}
		}
		cands = append(cands, taskproto.Candidate{
			InstanceType:  r.InstanceType,
			Family:        r.InstanceFamily,
			VCPUs:         int(r.VCPUs),
			MemoryGiB:     float64(r.MemoryMiB) / 1024,
			GPUs:          int(r.GPUs),
			Architecture:  r.Architecture,
			OnDemandPrice: price,
		})
	}
	return cands, nil
}

// renderTaskDryRun sizes the task via the given finder and prints the plan. Split
// from the RunE (which builds the AWS-backed finder) so it's unit-testable with a
// fake finder — no AWS, no launch.
func renderTaskDryRun(ctx context.Context, out io.Writer, spec *taskproto.TaskSpec, finder taskproto.InstanceFinder, region string) error {
	sized, err := taskproto.Size(ctx, finder, spec.Resources)
	if err != nil {
		return err
	}

	fmt.Fprintln(out, "DRY RUN — nothing will be launched.")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Task:         %s\n", spec.TaskID)
	fmt.Fprintf(out, "Command:      %s\n", strings.Join(spec.Command, " "))
	if spec.Container != "" {
		fmt.Fprintf(out, "Container:    %s\n", spec.Container)
	}
	fmt.Fprintf(out, "Region:       %s\n", region)
	fmt.Fprintf(out, "Instance:     %s  (%d vCPU, %.0f GiB", sized.InstanceType, sized.VCPUs, sized.MemoryGiB)
	if sized.OnDemandPrice > 0 {
		fmt.Fprintf(out, ", $%.4f/hr on-demand", sized.OnDemandPrice)
	}
	fmt.Fprintf(out, ")\n")
	fmt.Fprintf(out, "              chosen as cheapest of %d matching type(s)\n", sized.Considered)
	purchase := spec.Resources.Purchase
	if purchase == "" {
		purchase = taskproto.PurchaseOnDemand
	}
	fmt.Fprintf(out, "Purchase:     %s\n", purchase)
	fmt.Fprintf(out, "TTL:          %s   on-complete: %s\n", spec.Lifecycle.TTL, spec.EffectiveOnComplete())

	if d, err := time.ParseDuration(spec.Lifecycle.TTL); err == nil && sized.OnDemandPrice > 0 {
		fmt.Fprintf(out, "Max cost:     ~$%.2f (on-demand rate × TTL; a completed task usually costs far less)\n", sized.OnDemandPrice*d.Hours())
	}

	if len(spec.Inputs) > 0 {
		fmt.Fprintf(out, "Inputs:\n")
		for _, m := range spec.Inputs {
			fmt.Fprintf(out, "  %s → %s\n", m.Source, m.Destination)
		}
	}
	if len(spec.Outputs) > 0 {
		fmt.Fprintf(out, "Outputs:\n")
		for _, m := range spec.Outputs {
			fmt.Fprintf(out, "  %s → %s\n", m.Source, m.Destination)
		}
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Re-run without --dry-run to launch this task.")
	return nil
}

// runTaskReal launches a task for real: it sizes the instance (same as dry-run),
// ensures the per-account results bucket exists, generates the on-instance
// wrapper (stage-in → command → stage-out → durable completion record), creates
// a scoped instance profile granting the S3 access the wrapper needs, and
// launches via launcher.Provision (keyless/SSM, auto AMI+IAM+user-data). It
// returns as soon as the instance is launched — the caller polls the S3
// completion record (a --wait poller is a follow-up increment). Container
// execution is deferred: a spec with a container ref is rejected here.
func runTaskReal(ctx context.Context, out io.Writer, client *aws.Client, spec *taskproto.TaskSpec, finder taskproto.InstanceFinder, region string) error {
	if spec.Container != "" {
		return fmt.Errorf("container execution is a follow-up increment (spawn#386); omit 'container' to run the command on the host")
	}

	sized, err := taskproto.Size(ctx, finder, spec.Resources)
	if err != nil {
		return err
	}

	account, err := client.GetAccountID(ctx)
	if err != nil {
		return fmt.Errorf("resolve account id: %w", err)
	}
	resultsBucket := fmt.Sprintf("spawn-results-%s-%s", account, region)
	// Create the results bucket before launch so the wrapper's write can't hit
	// NoSuchBucket on a first-ever task in this account/region.
	if err := client.CreateS3BucketIfNotExists(ctx, resultsBucket, region); err != nil {
		return fmt.Errorf("ensure results bucket %s: %w", resultsBucket, err)
	}

	wrapper := taskproto.GenerateWrapper(spec, resultsBucket)

	// Scoped instance profile: the default spored role has no S3 write, so grant
	// exactly the buckets this task reads (inputs) and writes (outputs + results).
	profile, err := client.CreateOrGetInstanceProfile(ctx, aws.IAMRoleConfig{
		TrustServices:    []string{"ec2"}, // an instance profile is assumed by EC2
		InlinePolicyJSON: taskStagingPolicy(s3Buckets(spec.Inputs), s3Buckets(spec.Outputs), resultsBucket),
	})
	if err != nil {
		return fmt.Errorf("create task instance profile: %w", err)
	}

	cfg := taskLaunchConfig(spec, sized, region, profile, wrapper)

	result, err := launcher.Provision(ctx, client, cfg, launcher.Options{})
	// Single-region spot→on-demand fallback (a minimal echo of lagotto's
	// capacity fallthrough; AZ-spread is out of scope for this increment).
	if err != nil && cfg.Spot && spec.Resources.Fallback == taskproto.PurchaseOnDemand && isCapacityErr(err) {
		fmt.Fprintf(out, "⚠ spot capacity unavailable (%v); retrying on-demand per fallback...\n", classifyLaunchErr(err))
		cfg.Spot = false
		result, err = launcher.Provision(ctx, client, cfg, launcher.Options{})
	}
	if err != nil {
		return fmt.Errorf("launch task: %w", err)
	}

	lr := taskproto.LaunchResult{
		TaskID:       spec.TaskID,
		InstanceID:   result.InstanceID,
		Region:       result.Region,
		AZ:           result.AvailabilityZone,
		InstanceType: sized.InstanceType,
		Spot:         cfg.Spot,
	}
	if getOutputFormat() == "json" {
		return json.NewEncoder(out).Encode(lr)
	}

	fmt.Fprintf(out, "✅ Task launched: %s\n", spec.TaskID)
	fmt.Fprintf(out, "Instance:     %s  (%s%s) in %s / %s\n", lr.InstanceID, lr.InstanceType, spotSuffix(lr.Spot), lr.Region, orDash(lr.AZ))
	fmt.Fprintf(out, "TTL:          %s   on-complete: %s\n", spec.Lifecycle.TTL, spec.EffectiveOnComplete())
	fmt.Fprintf(out, "Completion:   s3://%s/tasks/%s/completion.json\n", resultsBucket, spec.TaskID)
	if !taskRunWait {
		fmt.Fprintf(out, "\nPoll for completion:\n")
		fmt.Fprintf(out, "  spawn task status %s --region %s\n", spec.TaskID, region)
		fmt.Fprintf(out, "  aws s3 cp s3://%s/tasks/%s/completion.json -\n", resultsBucket, spec.TaskID)
		return nil
	}

	// --wait: poll the completion record until it appears or the TTL elapses,
	// then print it and exit with the task's own exit code.
	fmt.Fprintf(out, "\nWaiting for completion (polling every %s)...\n", taskRunPollInterval)
	deadline := waitDeadline(spec.Lifecycle.TTL)
	rec, err := pollCompletion(ctx, client, region, resultsBucket, spec.TaskID, taskRunPollInterval, deadline)
	if err != nil {
		return err
	}
	printCompletion(out, rec)
	if rec.ExitCode != 0 {
		os.Exit(rec.ExitCode)
	}
	return nil
}

// waitDeadline returns now+TTL as the wait cutoff, defaulting to a generous cap
// if the TTL can't be parsed (validation should have caught that).
func waitDeadline(ttl string) time.Time {
	d, err := time.ParseDuration(ttl)
	if err != nil || d <= 0 {
		d = 24 * time.Hour
	}
	// A little slack past the TTL so a task that runs right up to its deadline
	// still has its record observed before we give up.
	return time.Now().Add(d + 2*time.Minute)
}

// pollCompletion fetches the completion record, retrying until it appears or the
// deadline passes.
func pollCompletion(ctx context.Context, client *aws.Client, region, resultsBucket, taskID string, every time.Duration, deadline time.Time) (*taskproto.CompletionRecord, error) {
	if every <= 0 {
		every = 15 * time.Second
	}
	for {
		rec, present, err := fetchCompletion(ctx, client, region, resultsBucket, taskID)
		if err != nil {
			return nil, err
		}
		if present {
			return rec, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for task %q completion record (past TTL); poll later with 'spawn task status %s'", taskID, taskID)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(every):
		}
	}
}

// fetchCompletion reads and parses tasks/<taskID>/completion.json from the
// results bucket. present=false (nil error) means the record isn't there yet —
// the task is still running.
func fetchCompletion(ctx context.Context, client *aws.Client, region, resultsBucket, taskID string) (rec *taskproto.CompletionRecord, present bool, err error) {
	key := fmt.Sprintf("tasks/%s/completion.json", taskID)
	data, err := client.GetS3Object(ctx, region, resultsBucket, key)
	if err != nil {
		if errors.Is(err, aws.ErrS3NoSuchKey) {
			return nil, false, nil
		}
		return nil, false, err
	}
	rec, err = taskproto.ParseCompletionRecord(data)
	if err != nil {
		return nil, false, err
	}
	return rec, true, nil
}

// printCompletion renders a CompletionRecord as human text.
func printCompletion(out io.Writer, rec *taskproto.CompletionRecord) {
	fmt.Fprintf(out, "Task:       %s\n", rec.TaskID)
	fmt.Fprintf(out, "State:      %s\n", rec.State)
	fmt.Fprintf(out, "Exit code:  %d\n", rec.ExitCode)
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

If the record isn't there yet the task is still running. With --check-complete,
exit codes mirror 'spawn status': 0=completed, 1=failed, 2=running, 3=error.`,
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
		account, err := client.GetAccountID(ctx)
		if err != nil {
			if taskStatusCheckDone {
				os.Exit(3)
			}
			return fmt.Errorf("resolve account id: %w", err)
		}
		resultsBucket := fmt.Sprintf("spawn-results-%s-%s", account, region)

		rec, present, err := fetchCompletion(ctx, client, region, resultsBucket, taskID)
		if err != nil {
			if taskStatusCheckDone {
				fmt.Fprintf(os.Stderr, "task status: %v\n", err)
				os.Exit(3)
			}
			return err
		}
		if !present {
			if taskStatusCheckDone {
				os.Exit(2) // running
			}
			fmt.Printf("Task %s: running (no completion record yet)\n", taskID)
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
		return nil
	},
}

// buildLaunchConfig maps a sized TaskSpec to the minimal aws.LaunchConfig for one
// task instance. AMI, UserData, and KeyName are deliberately left empty for
// launcher.Provision to fill (auto AMI, keyless/SSM, wrapper-as-user-data); the
// scoped instance profile is set so Provision skips its default-role step.
func taskLaunchConfig(spec *taskproto.TaskSpec, sized *taskproto.SizeResult, region, iamProfile, wrapper string) aws.LaunchConfig {
	return aws.LaunchConfig{
		InstanceType:       sized.InstanceType,
		Region:             region,
		JobArrayCommand:    wrapper, // Provision embeds this via /etc/spawn/command
		Spot:               spec.Resources.Purchase == taskproto.PurchaseSpot,
		TTL:                spec.Lifecycle.TTL,
		OnComplete:         spec.EffectiveOnComplete(),
		Name:               spec.TaskID,
		IamInstanceProfile: iamProfile,
		Tags:               map[string]string{"spawn:task-id": spec.TaskID},
	}
}

// s3Buckets extracts the distinct S3 bucket names from a manifest list's s3://
// endpoints (source or destination, whichever is the s3 side). Non-s3 entries
// (local paths) are skipped.
func s3Buckets(manifests []taskproto.Manifest) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range manifests {
		for _, ep := range []string{m.Source, m.Destination} {
			if b := s3Bucket(ep); b != "" && !seen[b] {
				seen[b] = true
				out = append(out, b)
			}
		}
	}
	return out
}

// s3Bucket returns the bucket name from an s3://bucket/key URI, or "" if uri is
// not an s3:// URI (e.g. a local path).
func s3Bucket(uri string) string {
	const p = "s3://"
	if !strings.HasPrefix(uri, p) {
		return ""
	}
	rest := uri[len(p):]
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[:i]
	}
	return rest
}

// taskStagingPolicy builds a scoped S3 policy granting read on the input buckets
// and write on the output + results buckets — the exact access the wrapper's
// aws s3 cp needs, and no more (preferred over the wildcard s3:FullAccess
// template). Modeled on GenerateScopedS3Policy.
func taskStagingPolicy(inputBuckets, outputBuckets []string, resultsBucket string) string {
	readB := dedupeBuckets(inputBuckets)
	writeB := dedupeBuckets(append(append([]string{}, outputBuckets...), resultsBucket))

	var stmts []string
	if len(readB) > 0 {
		stmts = append(stmts, fmt.Sprintf(`{"Effect":"Allow","Action":["s3:GetObject","s3:GetObjectVersion"],"Resource":[%s]}`, bucketObjectARNs(readB)))
		stmts = append(stmts, fmt.Sprintf(`{"Effect":"Allow","Action":["s3:ListBucket","s3:GetBucketLocation"],"Resource":[%s]}`, bucketARNs(readB)))
	}
	stmts = append(stmts, fmt.Sprintf(`{"Effect":"Allow","Action":["s3:PutObject"],"Resource":[%s]}`, bucketObjectARNs(writeB)))
	return `{"Version":"2012-10-17","Statement":[` + strings.Join(stmts, ",") + `]}`
}

func dedupeBuckets(buckets []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, b := range buckets {
		if b != "" && !seen[b] {
			seen[b] = true
			out = append(out, b)
		}
	}
	return out
}

func bucketARNs(buckets []string) string {
	arns := make([]string, len(buckets))
	for i, b := range buckets {
		arns[i] = fmt.Sprintf("%q", "arn:aws:s3:::"+b)
	}
	return strings.Join(arns, ",")
}

func bucketObjectARNs(buckets []string) string {
	arns := make([]string, len(buckets))
	for i, b := range buckets {
		arns[i] = fmt.Sprintf("%q", "arn:aws:s3:::"+b+"/*")
	}
	return strings.Join(arns, ",")
}

// isCapacityErr reports whether a launch error is a capacity class (worth a
// fallback retry), keyed on the AWS error code carried by aws.LaunchError.
func isCapacityErr(err error) bool {
	return classifyLaunchErr(err) == taskproto.RetryCapacity
}

// classifyLaunchErr extracts the AWS error code from a launch error and maps it
// to a taskproto RetryClass.
func classifyLaunchErr(err error) taskproto.RetryClass {
	var le *aws.LaunchError
	if errors.As(err, &le) {
		return taskproto.ClassifyLaunchError(le.Code)
	}
	return taskproto.RetryNone
}

func init() {
	rootCmd.AddCommand(taskGroupCmd)
	taskGroupCmd.AddCommand(taskDiagnoseCmd)
	taskDiagnoseCmd.Flags().BoolVar(&taskDiagnoseJSON, "json", false, "Output as JSON")
	_ = taskDiagnoseCmd.Flags().MarkDeprecated("json", "use --output json instead")

	taskGroupCmd.AddCommand(taskRunCmd)
	taskRunCmd.Flags().StringVar(&taskRunSpecPath, "spec", "", "Path to a TaskSpec JSON file (required)")
	taskRunCmd.Flags().BoolVar(&taskRunDryRun, "dry-run", false, "Size and preview the task without launching")
	taskRunCmd.Flags().StringVar(&taskRunRegion, "region", "", "Region to size against (default: the configured AWS region)")
	taskRunCmd.Flags().BoolVar(&taskRunWait, "wait", false, "Block until the task's completion record appears, then exit with its exit code")
	taskRunCmd.Flags().DurationVar(&taskRunPollInterval, "poll-interval", 15*time.Second, "How often to poll for completion when --wait is set")

	taskGroupCmd.AddCommand(taskStatusCmd)
	taskStatusCmd.Flags().StringVar(&taskStatusRegion, "region", "", "Region the task ran in (default: the configured AWS region)")
	taskStatusCmd.Flags().BoolVar(&taskStatusCheckDone, "check-complete", false, "Exit 0=completed, 1=failed, 2=running, 3=error instead of printing")
}
