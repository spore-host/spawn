package cmd

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spore-host/spawn/pkg/aws"
)

// billableResource is one thing this launch is paying for.
type billableResource struct {
	Kind string `json:"kind"`           // "instance" | "ebs" | "fsx" | "efs"
	ID   string `json:"id,omitempty"`   // instance id, volume/filesystem id
	Note string `json:"note,omitempty"` // type/size/lifecycle, human-readable
	USD  string `json:"cost,omitempty"` // rendered rate, e.g. "~$0.0104/hr"
	// Capped reports whether --cost-limit governs this resource. Only compute is.
	Capped bool `json:"capped_by_cost_limit"`
	// OutlivesInstance marks a resource that keeps billing after the instance is
	// gone — the shape that produced a 1.2 TiB filesystem nobody was watching.
	OutlivesInstance bool `json:"outlives_instance,omitempty"`
}

// billableResources enumerates what a launch is paying for, from the instance's
// own tags (spawn#615).
//
// Why from tags: the tags are what spawn wrote at launch and what spored keeps
// current, so this view cannot drift from the thing --cost-limit is actually
// enforced against (spawn:price-per-hour, #533) the way an independently re-derived
// price would. It also means no extra AWS calls on the common path.
//
// This exists because `spawn status` reported the instance and nothing else, while
// a launch can provision an FSx filesystem (the FSx Lustre minimum is 1200 GiB,
// ~$174/month), an EFS mount, and EBS volumes that keep billing while the instance
// is merely stopped. In #613 the filesystem was the most expensive thing in the
// launch and the least visible thing in the tooling: --cost-limit is compute-only,
// --estimate-only didn't mention it, terminate didn't mention it, and status didn't
// either. Every surface a user would check was silent about the largest line item.
func billableResources(instance *aws.InstanceInfo) []billableResource {
	if instance == nil || len(instance.Tags) == 0 {
		return nil
	}
	tags := instance.Tags
	var out []billableResource

	// Compute — the only thing --cost-limit governs.
	compute := billableResource{
		Kind:   "instance",
		ID:     instance.InstanceID,
		Note:   instance.InstanceType,
		Capped: true,
	}
	if instance.SpotInstance {
		compute.Note += " (spot)"
	}
	if rate := parseTagFloat(tags["spawn:price-per-hour"]); rate > 0 {
		compute.USD = fmt.Sprintf("~$%.4f/hr", rate)
	} else {
		// Honest blank rather than $0.00: an unpriced instance is how #533's cap
		// silently stopped being enforceable.
		compute.USD = "rate unknown"
	}
	if instance.State != "running" && instance.State != "pending" {
		compute.Note += " — " + instance.State + ", compute not billing"
	}
	out = append(out, compute)

	// EBS. The rate was resolved once at first boot and cached on the instance
	// (spawn:ebs-hourly-cost), so this needs no DescribeVolumes.
	if rate := parseTagFloat(tags["spawn:ebs-hourly-cost"]); rate > 0 {
		ebs := billableResource{
			Kind: "ebs",
			Note: "root/data volumes",
			USD:  fmt.Sprintf("~$%.4f/hr", rate),
		}
		if instance.State == "stopped" || instance.State == "stopping" {
			// The quiet surprise: "stopped to save money" still bills storage, and
			// spored cannot act once stopped.
			ebs.Note = "root/data volumes — STILL BILLING while the instance is stopped"
			ebs.OutlivesInstance = true
		}
		out = append(out, ebs)
	}

	// FSx Lustre, via the lease tags the reaper's refcount uses.
	if lease, ok := fsxLeaseFromTags(tags); ok {
		fsx := billableResource{
			Kind:             "fsx",
			ID:               lease.FilesystemID,
			OutlivesInstance: true,
		}
		var parts []string
		if lease.Pending {
			parts = append(parts, "still provisioning")
		}
		if lease.CapacityGiB > 0 {
			parts = append(parts, fmt.Sprintf("%d GiB", lease.CapacityGiB))
			fsx.USD = fmt.Sprintf("~$%.2f/month", fsxLustreMonthlyUSD(lease.CapacityGiB, 0))
		} else {
			// 1200 GiB is the FSx Lustre minimum, so the floor is knowable even
			// without a describe — far more useful than printing nothing.
			fsx.USD = fmt.Sprintf("~$%.2f/month or more", fsxLustreMonthlyUSD(1200, 0))
			parts = append(parts, "size unknown; 1200 GiB minimum assumed")
		}
		if lc := tags["spawn:fsx-lifecycle"]; lc != "" {
			parts = append(parts, lc)
		}
		fsx.Note = strings.Join(parts, ", ")
		out = append(out, fsx)
	}

	// EFS — billed per GiB stored, which spawn cannot know from a tag, so say that
	// rather than imply it is free.
	if id := tags["spawn:efs-id"]; id != "" {
		out = append(out, billableResource{
			Kind:             "efs",
			ID:               id,
			Note:             "billed per GiB stored (spawn does not know the stored size)",
			OutlivesInstance: true,
		})
	}

	return out
}

// renderBillableResources formats the billable-resource view, or "" when there is
// nothing to say (an unmanaged instance, or no tags).
func renderBillableResources(instance *aws.InstanceInfo) string {
	res := billableResources(instance)
	// One compute line alone is what `spawn status` already conveys; the point of
	// this block is the resources BESIDES the instance.
	if len(res) < 2 {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "\nBillable resources:\n")
	uncapped := false
	outlives := false
	for _, r := range res {
		id := r.ID
		if id == "" {
			id = "—"
		}
		fmt.Fprintf(&b, "  %-9s %-22s %-24s %s\n", r.Kind, id, r.USD, r.Note)
		if !r.Capped {
			uncapped = true
		}
		if r.OutlivesInstance {
			outlives = true
		}
	}
	if uncapped {
		fmt.Fprintf(&b, "  ⚠️  --cost-limit counts compute + EBS, and refuses a launch whose storage exceeds it\n")
		fmt.Fprintf(&b, "      up front — but it cannot RECLAIM storage that outlives the instance.\n")
	}
	if outlives {
		fmt.Fprintf(&b, "      Rows marked as outliving the instance keep billing after it is gone:\n")
		fmt.Fprintf(&b, "      'spawn fsx list' / 'spawn orphans' to find them, and delete explicitly.\n")
	}
	return b.String()
}

// parseTagFloat reads a numeric tag value, returning 0 when absent or unparseable.
func parseTagFloat(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || v < 0 {
		return 0
	}
	return v
}
