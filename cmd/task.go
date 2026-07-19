package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/aws"
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

func init() {
	rootCmd.AddCommand(taskGroupCmd)
	taskGroupCmd.AddCommand(taskDiagnoseCmd)
	taskDiagnoseCmd.Flags().BoolVar(&taskDiagnoseJSON, "json", false, "Output as JSON")
	_ = taskDiagnoseCmd.Flags().MarkDeprecated("json", "use --output json instead")
}
