package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"

	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/aws"
)

// The `spawn array` group gives job arrays first-class reporting. Unlike sweeps
// (which have a DynamoDB record), array members are discovered by listing EC2 and
// grouping on the spawn:job-array-id / spawn:job-array-name tags — the same tags
// launch writes (pkg/aws/tags.go) and `spawn list --job-array-name` already
// filters on. Members are addressed by array NAME (the human-facing identifier);
// the id tag is the stable grouping key.

var (
	arrayRegion     string
	arrayJSON       bool
	arrayCancelYes  bool
	arrayCancelPend bool
	arrayCollectDir string
)

var arrayGroupCmd = &cobra.Command{
	Use:   "array",
	Short: "Manage job arrays (status, collect, cancel)",
	Long: `Manage job arrays launched with --job-array-name / --count.

Members are grouped by their job-array tags, so these work without any
server-side record:

  spawn array status data-proc
  spawn array collect data-proc ./results
  spawn array cancel data-proc --pending`,
}

// arrayMember is one instance in an array, with the fields status/collect need.
type arrayMember struct {
	Index      int
	InstanceID string
	Name       string
	State      string
	Region     string
	Params     map[string]string
}

// findArrayMembers lists instances (optionally in one region) and returns those
// whose job-array-name matches, sorted by index. The requested size comes from
// the spawn:job-array-size tag (the count the array was launched with), which is
// stamped identically on every member.
func findArrayMembers(ctx context.Context, name, region string) (members []arrayMember, requestedSize int, err error) {
	client, err := aws.NewClient(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("init AWS client: %w", err)
	}
	instances, err := client.ListInstances(ctx, region, "")
	if err != nil {
		return nil, 0, fmt.Errorf("list instances: %w", err)
	}
	for _, in := range instances {
		if in.JobArrayName != name {
			continue
		}
		idx, _ := strconv.Atoi(in.JobArrayIndex)
		if in.JobArraySize != "" {
			if sz, e := strconv.Atoi(in.JobArraySize); e == nil && sz > requestedSize {
				requestedSize = sz
			}
		}
		members = append(members, arrayMember{
			Index:      idx,
			InstanceID: in.InstanceID,
			Name:       in.Name,
			State:      in.State,
			Region:     in.Region,
			Params:     in.Parameters,
		})
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Index < members[j].Index })
	return members, requestedSize, nil
}

// missingIndexes returns the requested indexes (0..size-1) that have no live
// member — the sparse-index gap a --min-viable partial launch leaves behind.
func missingIndexes(members []arrayMember, size int) []int {
	present := make(map[int]bool, len(members))
	for _, m := range members {
		present[m.Index] = true
	}
	var missing []int
	for i := 0; i < size; i++ {
		if !present[i] {
			missing = append(missing, i)
		}
	}
	return missing
}

// ── array status ─────────────────────────────────────────────────────────────

var arrayStatusCmd = &cobra.Command{
	Use:   "status <array-name>",
	Short: "Show a job array's members, requested vs launched, and missing indexes",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		members, size, err := findArrayMembers(ctx, args[0], arrayRegion)
		if err != nil {
			return err
		}
		if len(members) == 0 {
			return fmt.Errorf("no instances found for job array %q (try --region, or the array may be fully terminated)", args[0])
		}
		missing := missingIndexes(members, size)

		if arrayJSON || getOutputFormat() == "json" {
			return json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
				"name":            args[0],
				"requested":       size,
				"launched":        len(members),
				"missing_indexes": missing,
				"members":         members,
			})
		}

		fmt.Printf("Job array: %s\n", args[0])
		fmt.Printf("Launched %d of %d requested member(s).\n", len(members), size)
		w := newTableWriter(os.Stdout)
		_, _ = fmt.Fprintln(w, "INDEX\tNAME\tINSTANCE\tSTATE\tREGION")
		for _, m := range members {
			_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", m.Index, m.Name, m.InstanceID, m.State, m.Region)
		}
		_ = w.Flush()
		if len(missing) > 0 {
			fmt.Printf("\n⚠ Missing indexes (never launched or already terminated): %v\n", missing)
			fmt.Printf("  {total} stays %d for every member, so a shard scheme assuming a dense\n", size)
			fmt.Printf("  0..%d range will skip these. See job-arrays docs.\n", size-1)
		}
		return nil
	},
}

// ── array collect ──────────────────────────────────────────────────────────

var arrayCollectCmd = &cobra.Command{
	Use:   "collect <array-name> [dest-dir]",
	Short: "Report where each member's results are (per-index)",
	Args:  cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		members, size, err := findArrayMembers(ctx, args[0], arrayRegion)
		if err != nil {
			return err
		}
		if len(members) == 0 {
			return fmt.Errorf("no instances found for job array %q", args[0])
		}
		dest := arrayCollectDir
		if len(args) == 2 {
			dest = args[1]
		}
		// Array members write to wherever their own --command sends output (there
		// is no server-side results manifest as sweeps have). Report the per-index
		// members so the caller can pull from their known S3 layout; note any gaps.
		fmt.Printf("Job array %s: %d of %d members present.\n", args[0], len(members), size)
		if dest != "" {
			fmt.Printf("Destination: %s\n", dest)
		}
		for _, m := range members {
			fmt.Printf("  index %d → %s (%s)\n", m.Index, m.Name, m.State)
		}
		if missing := missingIndexes(members, size); len(missing) > 0 {
			fmt.Printf("⚠ No results for missing indexes: %v\n", missing)
		}
		return nil
	},
}

// ── array cancel ─────────────────────────────────────────────────────────────

var arrayCancelCmd = &cobra.Command{
	Use:   "cancel <array-name>",
	Short: "Terminate a job array's instances",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		members, _, err := findArrayMembers(ctx, args[0], arrayRegion)
		if err != nil {
			return err
		}
		// --pending keeps running work and only reaps members that are not doing
		// useful compute (stopped/stopping) — mirrors "cancel the queued remainder".
		targets := members
		if arrayCancelPend {
			targets = targets[:0]
			for _, m := range members {
				switch m.State {
				case "running", "pending":
					// leave active members alone
				default:
					targets = append(targets, m)
				}
			}
		}
		if len(targets) == 0 {
			fmt.Println("Nothing to cancel.")
			return nil
		}

		fmt.Printf("Will terminate %d instance(s) in job array %q:\n", len(targets), args[0])
		for _, m := range targets {
			fmt.Printf("  index %d  %s  %s (%s)\n", m.Index, m.Name, m.InstanceID, m.State)
		}
		if !confirmYes(arrayCancelYes, "Terminate these instances?") {
			fmt.Println("Aborted.")
			return nil
		}

		client, err := aws.NewClient(ctx)
		if err != nil {
			return fmt.Errorf("init AWS client: %w", err)
		}
		var failed int
		for _, m := range targets {
			if err := client.Terminate(ctx, m.Region, m.InstanceID); err != nil {
				fmt.Fprintf(os.Stderr, "⚠ terminate %s (index %d) failed: %v\n", m.InstanceID, m.Index, err)
				failed++
			}
		}
		if failed > 0 {
			return fmt.Errorf("%d of %d terminations failed", failed, len(targets))
		}
		fmt.Printf("Terminated %d instance(s).\n", len(targets))
		return nil
	},
}

func init() {
	rootCmd.AddCommand(arrayGroupCmd)
	arrayGroupCmd.AddCommand(arrayStatusCmd, arrayCollectCmd, arrayCancelCmd)

	for _, sub := range []*cobra.Command{arrayStatusCmd, arrayCollectCmd, arrayCancelCmd} {
		sub.Flags().StringVar(&arrayRegion, "region", "", "Region to search (default: all regions)")
	}
	arrayStatusCmd.Flags().BoolVar(&arrayJSON, "json", false, "Output as JSON")
	arrayCollectCmd.Flags().StringVar(&arrayCollectDir, "output-dir", "", "Destination directory hint for results")
	arrayCancelCmd.Flags().BoolVarP(&arrayCancelYes, "yes", "y", false, "Skip the confirmation prompt")
	arrayCancelCmd.Flags().BoolVar(&arrayCancelPend, "pending", false, "Only terminate members that are not actively running")
}
