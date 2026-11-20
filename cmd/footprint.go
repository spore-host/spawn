package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/spore-host/libs/footprint"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/instancerole"
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
execution roles, EventBridge event buses and the rules on every bus (the default
event bus and spawn's own), EventBridge Scheduler schedules (what 'spawn
schedule' creates, in every schedule group), state tables, and the buckets spawn
auto-creates. The TTL reaper is the backstop for instances; nothing is the
backstop for the reaper (#653), so the honest first step is making the footprint
visible rather than deleting anything.

This command NEVER deletes. It is a report. 'spawn teardown --plan' reads its
JSON and, with --mode reap, deletes what the report marks deletable.

Resources are found two ways, and each row says which:

  tag          the spawn:managed tag — authoritative, and what 'spawn cleanup' acts on
  name         a spawn/spore/spored/lagotto/truffle/calque name prefix — needed because
               most of the control plane predates being tagged
  target       a schedule whose target is a spore.host Lambda
  description  a schedule named like spawn's (sched-YYYYMMDD-HHMMSS) whose
               description is spawn's

Reading schedules needs scheduler:ListSchedules (and scheduler:GetSchedule for
the description check). Without them the report says so under "Could not read"
and lists everything else.

The name half has a known blind spot, stated rather than hidden: it cannot find a
resource named differently. A few known spore.host names with no prefix (such as
the 'scheduler-handler' Lambda) are matched by exact name, but any other
differently-named resource is missed. So the totals below are a floor, not a
guarantee — three separate hand-built footprint lists during #653 were each
incomplete.

-o json is the shared spore.host footprint schema (libs/footprint, schema 1):
every resource with its ARN, type, region, name, tags, ownership, class,
deletable verdict and reason, and depends_on. It also lists what the tables
above do not: the managed policies under /spore-host/, CloudFormation stacks
named like spore.host's, spawn-pipeline-* SNS topics, anything tagged
spore-host:managed-by by any tool, and calque's queues, NFS security group and
AMIs (calque's, so never spawn's to delete).

  ownership  tagged (spore-host:managed-by=spawn), legacy-name (only the name or
             spawn:managed matched; never deleted), foreign (another tool's)
  class      permanent (the two pieces of account setup: the boundary and the
             user lifecycle policy; spore-host:permanent, role=service, the
             infra account), user-data (results, AMIs, snapshots, a user's key
             or Elastic IP), active (in use now), residue (spawn's machinery),
             litter (the rest)

Only tagged resources of a type spawn's teardown can delete, whose class is
not permanent or user-data, are deletable.`,
	Example: `  # This region, including the control plane
  spawn footprint

  # Every region enabled in this account
  spawn footprint --all-regions

  # Machine-readable, and a teardown plan
  spawn footprint -o json > plan.json
  spawn teardown --plan plan.json`,
	RunE: runFootprint,
}

func init() {
	footprintCmd.Flags().StringVar(&footprintRegion, "region", "", "AWS region (default: configured region)")
	footprintCmd.Flags().BoolVar(&footprintAllRegions, "all-regions", false, "Scan every region enabled in this account (DescribeRegions), not just the configured one")
	rootCmd.AddCommand(footprintCmd)
}

// footprintReport is what the human tables print. -o json is the
// libs/footprint Report built from it (spawn#1653); this shape is no longer
// written as JSON.
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

	// The same scans the reaper's account-idle sweep plans from (spawn#1654).
	in := client.GatherFootprint(ctx, regions)
	rep := footprintReport{Tagged: in.Tagged, ControlPlane: in.ControlPlane, Global: in.Global, Notes: in.Notes}
	seenRegion := map[string]bool{}
	for _, cp := range in.ControlPlane {
		if cp.Region != "" && !seenRegion[cp.Region] {
			seenRegion[cp.Region] = true
			rep.Regions = append(rep.Regions, cp.Region)
		}
	}

	for _, r := range append(append([]aws.ControlPlaneResource{}, rep.ControlPlane...), rep.Global...) {
		if r.Warn != "" {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s %s: %s", r.Kind, r.Name, r.Warn))
		}
	}

	account, err := client.GetAccountID(ctx)
	if err != nil {
		rep.Notes = append(rep.Notes, fmt.Sprintf("account: %v (ARNs built from it are left without one)", err))
	}
	full := client.FootprintReport(ctx, aws.FootprintInput{
		Account: account, Regions: regions, Tagged: rep.Tagged, ControlPlane: rep.ControlPlane,
		Global: rep.Global, Notes: rep.Notes, Now: time.Now(),
	})

	// The root -o/--output, not a local --json: the repo's flag convention
	// (spawn#40) keeps one spelling across every command, and
	// TestFlagConventions enforces it.
	if spawnOutputFormat == "json" {
		return writeFootprintJSON(os.Stdout, full)
	}
	renderFootprint(os.Stdout, rep)
	renderFootprintVerdicts(os.Stdout, full)
	return nil
}

// writeFootprintJSON writes the libs/footprint report, the shape
// `spawn teardown --plan` reads back with footprint.Decode.
func writeFootprintJSON(w io.Writer, rep *footprint.Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}

// renderFootprintVerdicts is the teardown view of the report: every resource
// with its ownership and class, and whether 'spawn teardown' would delete it.
func renderFootprintVerdicts(w io.Writer, rep *footprint.Report) {
	if len(rep.Resources) == 0 {
		return
	}
	deletable := 0
	byClass := map[footprint.Class]int{}
	for _, r := range rep.Resources {
		byClass[r.Class]++
		if r.Deletable {
			deletable++
		}
	}
	fmt.Fprintf(w, "Teardown view (%d) — %d deletable by 'spawn teardown'; litter %d, residue %d, active %d, user-data %d, permanent %d\n",
		len(rep.Resources), deletable, byClass[footprint.ClassLitter], byClass[footprint.ClassResidue],
		byClass[footprint.ClassActive], byClass[footprint.ClassUserData], byClass[footprint.ClassPermanent])
	tw := newTableWriter(w)
	fmt.Fprintf(tw, "  TYPE\tNAME\tREGION\tOWNERSHIP\tCLASS\tDELETABLE\tREASON\n")
	for _, r := range rep.Resources {
		yes := "no"
		if r.Deletable {
			yes = "yes"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Type, r.Name, r.Region, r.Ownership, r.Class, yes, r.Reason)
	}
	tw.Flush()
	fmt.Fprintln(w)
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
		fmt.Fprintf(w, "spawn/spore/spored/lagotto/truffle/calque name prefix. It does NOT mean the account\n")
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
		// Name the deleter of the per-task roles (#1300), so the rows above are
		// not read as clutter nothing will ever remove.
		perTask := 0
		for _, r := range rep.Global {
			if r.Service == "iam" && r.Kind == "role" && instancerole.IsCandidate(r.Name) {
				perTask++
			}
		}
		if perTask > 0 {
			fmt.Fprintf(w, "  %d per-task %s* role(s): each is deleted with its instance profile once no\n", perTask, instancerole.NamePrefix)
			fmt.Fprintf(w, "  instance uses it and it has been unused for 24h, by the TTL reaper\n")
			fmt.Fprintf(w, "  (REAPER_INSTANCE_ROLES=reap) or 'spawn cleanup --instance-roles'.\n")
		}
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
	fmt.Fprintf(w, "outside the known prefixes and exact names does not appear.\n")
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
