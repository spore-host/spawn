package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/aws"
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
	taskRunSpecPath string
	taskRunDryRun   bool
	taskRunRegion   string
)

var taskRunCmd = &cobra.Command{
	Use:   "run --spec <file> --dry-run",
	Short: "Size and plan a task from a TaskSpec (dry-run only for now)",
	Long: `Run a task described by a TaskSpec JSON file (the shared workflow-adapter
contract, spawn#386).

This is the first increment: only --dry-run is supported. It parses and validates
the spec, sizes the cheapest instance type that fits its resource request (via
truffle), and prints the plan — WITHOUT launching anything. Real launch and the
durable .exitcode-in-S3 completion record are a follow-up (see #386).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if taskRunSpecPath == "" {
			return fmt.Errorf("--spec is required")
		}
		if !taskRunDryRun {
			return fmt.Errorf("real execution is not implemented yet (spawn#386) — re-run with --dry-run to size and preview the task")
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
		return renderTaskDryRun(cmd.Context(), os.Stdout, spec, finder, region)
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
		cands = append(cands, taskproto.Candidate{
			InstanceType:  r.InstanceType,
			Family:        r.InstanceFamily,
			VCPUs:         int(r.VCPUs),
			MemoryGiB:     float64(r.MemoryMiB) / 1024,
			GPUs:          int(r.GPUs),
			Architecture:  r.Architecture,
			OnDemandPrice: r.OnDemandPrice,
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
	fmt.Fprintln(out, "Real launch + durable completion record are not implemented yet (spawn#386).")
	return nil
}

func init() {
	rootCmd.AddCommand(taskGroupCmd)
	taskGroupCmd.AddCommand(taskDiagnoseCmd)
	taskDiagnoseCmd.Flags().BoolVar(&taskDiagnoseJSON, "json", false, "Output as JSON")
	_ = taskDiagnoseCmd.Flags().MarkDeprecated("json", "use --output json instead")

	taskGroupCmd.AddCommand(taskRunCmd)
	taskRunCmd.Flags().StringVar(&taskRunSpecPath, "spec", "", "Path to a TaskSpec JSON file (required)")
	taskRunCmd.Flags().BoolVar(&taskRunDryRun, "dry-run", false, "Size and preview the task without launching (currently required)")
	taskRunCmd.Flags().StringVar(&taskRunRegion, "region", "", "Region to size against (default: the configured AWS region)")
}
