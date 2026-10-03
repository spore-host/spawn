package cmd

import (
	"fmt"
	"io"
)

// FSx Lustre storage rates (spawn#613).
//
// These are APPROXIMATIONS, kept in exactly one place so no other file grows its
// own magic number. Source: the AWS FSx for Lustre pricing page, PERSISTENT_2
// SSD, us-east-1, as read in September 2026. For PERSISTENT_2 the throughput
// tier is folded into the per-GiB-month storage price, so the rate is keyed by
// MB/s/TiB rather than added on top.
//
// Other regions are higher (typically 0–25%), and this deliberately ignores
// compression savings, backups, and data-repository traffic. It exists so a
// ~$174/month resource is not reported as $0 — not to be an invoice.
var fsxLustreUSDPerGiBMonthByThroughput = map[int32]float64{
	125:  0.145,
	250:  0.290,
	500:  0.580,
	1000: 1.160,
}

// fsxLustreDefaultUSDPerGiBMonth is used when the throughput tier is unknown or
// not one spawn can create (it matches --fsx-throughput's own default of 125).
const fsxLustreDefaultUSDPerGiBMonth = 0.145

// fsxLustreUSDPerGiBMonth returns the approximate per-GiB-month storage rate for
// a PERSISTENT_2 filesystem at the given throughput tier (MB/s/TiB).
func fsxLustreUSDPerGiBMonth(throughputMBpsPerTiB int32) float64 {
	if rate, ok := fsxLustreUSDPerGiBMonthByThroughput[throughputMBpsPerTiB]; ok {
		return rate
	}
	return fsxLustreDefaultUSDPerGiBMonth
}

// fsxLustreMonthlyUSD returns the approximate monthly storage cost of a
// filesystem of the given capacity and throughput tier. Zero capacity yields 0.
func fsxLustreMonthlyUSD(capacityGiB, throughputMBpsPerTiB int32) float64 {
	if capacityGiB <= 0 {
		return 0
	}
	return float64(capacityGiB) * fsxLustreUSDPerGiBMonth(throughputMBpsPerTiB)
}

// fsxCreatePlan is the filesystem a launch WOULD create, as far as the cost
// preview is concerned. It is a plain value with no AWS dependency so the
// renderer below can be tested without a client (same seam idea as taskAMIPlan,
// spawn#605).
type fsxCreatePlan struct {
	CapacityGiB          int32
	ThroughputMBpsPerTiB int32
	Lifecycle            string // "ephemeral" | "durable" | "" (unvalidated)
	TTL                  string // --fsx-ttl, durable only
	MountPoint           string
}

// fsxCreatePlanFromFlags builds the plan from the resolved launch flags. Returns
// false when this launch creates no filesystem (nothing to price).
func fsxCreatePlanFromFlags(create bool, capacityGiB, throughput int32, lifecycle, ttl, mountPoint string) (fsxCreatePlan, bool) {
	if !create {
		return fsxCreatePlan{}, false
	}
	// Mirror the create path's own defaults so the preview quotes what would
	// actually be provisioned (cmd/launch_single.go).
	if throughput == 0 {
		throughput = 125 // PERSISTENT_2 minimum, same fallback the create path applies
	}
	if capacityGiB == 0 {
		capacityGiB = 1200 // FSx Lustre minimum, same as --fsx-storage-capacity's default
	}
	return fsxCreatePlan{
		CapacityGiB:          capacityGiB,
		ThroughputMBpsPerTiB: throughput,
		Lifecycle:            lifecycle,
		TTL:                  ttl,
		MountPoint:           mountPoint,
	}, true
}

// renderFSxCostEstimate writes the FSx section of the --estimate-only preview.
//
// Why this exists (spawn#613): --fsx-create provisions an FSx Lustre filesystem
// whose 1200 GiB minimum costs ~$174/month, and the estimate used to print only
// the instance's hourly/TTL cost — so the single most expensive thing a launch
// could create was invisible. --cost-limit does not cover it either (it tracks
// compute spend only), and that limitation is stated HERE, at the point of use,
// not just in the flag help nobody re-reads.
func renderFSxCostEstimate(w io.Writer, plan fsxCreatePlan) {
	monthly := fsxLustreMonthlyUSD(plan.CapacityGiB, plan.ThroughputMBpsPerTiB)
	rate := fsxLustreUSDPerGiBMonth(plan.ThroughputMBpsPerTiB)

	fmt.Fprintf(w, "\n📁 FSx Lustre filesystem (created by this launch)\n")
	fmt.Fprintf(w, "   Capacity:   %d GiB PERSISTENT_2 @ %d MB/s/TiB\n", plan.CapacityGiB, plan.ThroughputMBpsPerTiB)
	if monthly > 0 {
		fmt.Fprintf(w, "   Storage:    ~$%.2f/month (~$%.4f/hr) at ~$%.3f/GiB-month — approximate, us-east-1 list price\n",
			monthly, monthly/730, rate)
	}
	switch plan.Lifecycle {
	case "ephemeral":
		fmt.Fprintf(w, "   Lifecycle:  ephemeral — reclaimed asynchronously after the instance is gone,\n")
		fmt.Fprintf(w, "               not by `spawn terminate`; it bills until then\n")
	case "durable":
		if plan.TTL != "" {
			fmt.Fprintf(w, "   Lifecycle:  durable — bills until its %s TTL expires with nothing using it\n", plan.TTL)
		} else {
			fmt.Fprintf(w, "   Lifecycle:  durable — bills until its TTL expires with nothing using it\n")
		}
	}
	fmt.Fprintf(w, "   ⚠️  --cost-limit counts this, and refuses the launch if it exceeds the cap on its\n")
	fmt.Fprintf(w, "       own — but it cannot RECLAIM it: the filesystem outlives the instance, so it\n")
	fmt.Fprintf(w, "       keeps billing after the instance is gone. Check with `spawn fsx list`;\n")
	fmt.Fprintf(w, "       delete with `spawn fsx delete <fs-id>`.\n")
}
