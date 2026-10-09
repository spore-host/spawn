package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/aws"
)

var (
	footprintRegion     string
	footprintAllRegions bool
)

var footprintCmd = &cobra.Command{
	Use:   "footprint",
	Short: "Report everything spawn has created in this account, including the control plane",
	Long: `Report everything spawn has created in an account — not just instances.

'spawn orphans' covers the DATA plane: volumes, security groups, placement
groups, Elastic IPs — the things a launch creates and that a launch's end should
reclaim. This covers the CONTROL plane as well: the Lambdas, their log groups and
execution roles, EventBridge schedules, state tables, and the buckets spawn
auto-creates. The TTL reaper is the backstop for instances; nothing is the
backstop for the reaper (#653), so the honest first step is making the footprint
visible rather than deleting anything.

This command NEVER deletes. It is a report.

Resources are found two ways, and each row says which:

  tag    the spawn:managed tag — authoritative, and what 'spawn cleanup' acts on
  name   a spawn/spore/spored/lagotto/truffle name prefix — needed because most
         of the control plane predates being tagged

The name half has a known blind spot, stated rather than hidden: it cannot find a
resource named differently. One live Lambda is called 'scheduler-handler', with no
prefix at all. So the totals below are a floor, not a guarantee — three separate
hand-built footprint lists during #653 were each incomplete.`,
	Example: `  # This region, including the control plane
  spawn footprint

  # Every region spawn knows about
  spawn footprint --all-regions

  # Machine-readable
  spawn footprint -o json`,
	RunE: runFootprint,
}

func init() {
	footprintCmd.Flags().StringVar(&footprintRegion, "region", "", "AWS region (default: configured region)")
	footprintCmd.Flags().BoolVar(&footprintAllRegions, "all-regions", false, "Scan every region spawn supports")
	rootCmd.AddCommand(footprintCmd)
}

// footprintReport is the JSON shape. Named fields rather than a bare list so a
// consumer can tell an empty account from a failed scan — the #708 lesson, where
// "nothing to clean" and "the scope hid everything" printed identically.
type footprintReport struct {
	Regions      []string                   `json:"regions"`
	Tagged       []aws.ManagedResource      `json:"tagged"`
	ControlPlane []aws.ControlPlaneResource `json:"control_plane"`
	Global       []aws.ControlPlaneResource `json:"global"`
	Notes        []string                   `json:"notes,omitempty"`
	Warnings     []string                   `json:"warnings,omitempty"`
}

func runFootprint(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	client, err := aws.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("initialize AWS client: %w", err)
	}

	// Reuses the resolver `spawn orphans`/`cleanup` use, so --all-regions means
	// the same set of regions in all three rather than a second opinion.
	regions, err := resolveCleanupRegions(ctx, client, footprintRegion, footprintAllRegions)
	if err != nil {
		return fmt.Errorf("resolve regions: %w", err)
	}

	rep := footprintReport{}
	for _, r := range regions {
		tagged, _, err := client.DiscoverManagedResources(ctx, aws.DiscoverOptions{Region: r})
		if err != nil {
			rep.Notes = append(rep.Notes, fmt.Sprintf("%s: tagged scan: %v", r, err))
		} else {
			rep.Tagged = append(rep.Tagged, tagged...)
		}

		cp, notes := client.ScanControlPlane(ctx, r)
		rep.ControlPlane = append(rep.ControlPlane, cp...)
		for _, n := range notes {
			rep.Notes = append(rep.Notes, fmt.Sprintf("%s: %s", r, n))
		}
		if len(cp) > 0 {
			rep.Regions = append(rep.Regions, firstNonEmpty(cp[0].Region, r))
		}
	}

	g, gnotes := client.ScanGlobalFootprint(ctx)
	rep.Global = g
	rep.Notes = append(rep.Notes, gnotes...)

	for _, r := range append(append([]aws.ControlPlaneResource{}, rep.ControlPlane...), rep.Global...) {
		if r.Warn != "" {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s %s: %s", r.Kind, r.Name, r.Warn))
		}
	}

	// The root -o/--output, not a local --json: the repo's flag convention
	// (spawn#40) keeps one spelling across every command, and
	// TestFlagConventions enforces it.
	if spawnOutputFormat == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	renderFootprint(os.Stdout, rep)
	return nil
}

// renderFootprint writes the human report. Split out so the wording — which is
// the point of this command — is testable without AWS.
func renderFootprint(w io.Writer, rep footprintReport) {
	fmt.Fprintf(w, "spawn footprint — a report, nothing is deleted\n\n")

	if len(rep.Tagged) == 0 && len(rep.ControlPlane) == 0 && len(rep.Global) == 0 {
		fmt.Fprintf(w, "Nothing found.\n\n")
		// Never let an empty result read as a clean account without saying what
		// was actually looked at (#708).
		fmt.Fprintf(w, "That means: no resource tagged spawn:managed, and nothing matching a\n")
		fmt.Fprintf(w, "spawn/spore/spored/lagotto/truffle name prefix. It does NOT mean the account\n")
		fmt.Fprintf(w, "is empty — a resource named differently would not appear here.\n")
		renderFootprintNotes(w, rep)
		return
	}

	if len(rep.Tagged) > 0 {
		fmt.Fprintf(w, "Tagged spawn:managed (%d) — what `spawn cleanup` acts on\n", len(rep.Tagged))
		tw := newTableWriter(w)
		fmt.Fprintf(tw, "  SERVICE\tTYPE\tID\tREGION\tSTATE\n")
		for _, r := range rep.Tagged {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", r.Service, r.ResourceType, r.ID, r.Region, r.State)
		}
		tw.Flush()
		fmt.Fprintln(w)
	}

	if len(rep.ControlPlane) > 0 {
		fmt.Fprintf(w, "Control plane, by region (%d) — NOT covered by `spawn cleanup` or the reaper\n", len(rep.ControlPlane))
		tw := newTableWriter(w)
		fmt.Fprintf(tw, "  SERVICE\tKIND\tNAME\tREGION\tDETAIL\tWHY\n")
		for _, r := range rep.ControlPlane {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\n", r.Service, r.Kind, r.Name, r.Region, r.Detail, r.Why)
		}
		tw.Flush()
		fmt.Fprintln(w)
	}

	if len(rep.Global) > 0 {
		fmt.Fprintf(w, "Global (%d) — account-wide, not per-region\n", len(rep.Global))
		tw := newTableWriter(w)
		fmt.Fprintf(tw, "  SERVICE\tKIND\tNAME\tAGE\tWHY\n")
		for _, r := range rep.Global {
			age := "—"
			if !r.Created.IsZero() {
				age = fmt.Sprintf("%dd", int(time.Since(r.Created).Hours()/24))
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", r.Service, r.Kind, r.Name, age, r.Why)
		}
		tw.Flush()
		fmt.Fprintln(w)
	}

	if len(rep.Warnings) > 0 {
		fmt.Fprintf(w, "Worth a look (%d)\n", len(rep.Warnings))
		for _, warn := range rep.Warnings {
			fmt.Fprintf(w, "  ⚠  %s\n", warn)
		}
		fmt.Fprintln(w)
	}

	renderFootprintNotes(w, rep)

	fmt.Fprintf(w, "The name-matched rows are a FLOOR, not a complete list: a resource named\n")
	fmt.Fprintf(w, "outside the known prefixes does not appear. One live Lambda is called\n")
	fmt.Fprintf(w, "'scheduler-handler', with no prefix at all.\n")
}

func renderFootprintNotes(w io.Writer, rep footprintReport) {
	if len(rep.Notes) == 0 {
		return
	}
	// Printed, not swallowed: a service the caller cannot read is a HOLE in the
	// inventory, and an inventory with an unreported hole is worse than none.
	fmt.Fprintf(w, "Could not read (%d) — these are gaps in the report, not absences\n", len(rep.Notes))
	for _, n := range rep.Notes {
		fmt.Fprintf(w, "  !  %s\n", n)
	}
	fmt.Fprintln(w)
}
