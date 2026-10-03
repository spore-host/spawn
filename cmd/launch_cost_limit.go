package cmd

import (
	"fmt"
	"strings"
)

// storageCommitment is the storage a launch would create, priced as a MONTHLY figure.
//
// Monthly, not TTL-prorated, deliberately: the cost that matters here is the cost
// `--cost-limit` cannot bound. A filesystem outlives its instance by design, so once
// created it bills until something deletes it — and spored, which enforces the cap,
// dies with the instance. For a `durable` filesystem nothing deletes it automatically
// at all; for an `ephemeral` one only the out-of-band reaper does, and only in an
// account that reaper covers (spawn#624). So a month is the floor, not the estimate.
type storageCommitment struct {
	// Items are human-readable lines, one per resource.
	Items []string
	// MonthlyUSD is the floor cost of everything in Items.
	MonthlyUSD float64
}

// Empty reports whether the launch creates no storage that outlives the instance.
func (s storageCommitment) Empty() bool { return s.MonthlyUSD <= 0 }

// launchStorageCommitment prices the storage the CURRENT launch flags would create.
//
// Only counts storage that OUTLIVES the instance. Root/data EBS with
// DeleteOnTermination (spawn's default) dies with the instance and is accounted
// in-flight by spored instead, so including it here would double-count and refuse
// launches that are genuinely fine.
func launchStorageCommitment() storageCommitment {
	var s storageCommitment

	// --fsx-create: the FSx Lustre minimum is 1200 GiB, so the floor is knowable
	// without any AWS call.
	if plan, ok := fsxCreatePlanFromFlags(fsxCreate, fsxStorageCapacity, fsxThroughput, fsxLifecycle, fsxTTL, fsxMountPoint); ok {
		monthly := fsxLustreMonthlyUSD(plan.CapacityGiB, plan.ThroughputMBpsPerTiB)
		if monthly > 0 {
			lifecycle := plan.Lifecycle
			if lifecycle == "" {
				lifecycle = "unset"
			}
			s.Items = append(s.Items, fmt.Sprintf(
				"FSx Lustre: %d GiB at %d MB/s/TiB, lifecycle %q — ~$%.2f/month",
				plan.CapacityGiB, plan.ThroughputMBpsPerTiB, lifecycle, monthly))
			s.MonthlyUSD += monthly
		}
	}

	return s
}

// costLimitPreflight refuses a launch whose storage commitment alone exceeds
// --cost-limit (spawn#616).
//
// Why a pre-flight check rather than leaving it to spored: --cost-limit is enforced
// INSIDE the instance, and spored cannot act on a resource that outlives it. In #613
// a `--cost-limit 2.50` launch created a 1.2 TiB filesystem — ~$174/month, ~70x the
// cap — and the cap was respected to the letter the whole time, because it only ever
// counted compute. Submit time is the last moment the commitment can be refused
// rather than merely reported.
//
// Returns nil when there is nothing to refuse: no cap, no storage, or an explicit
// override.
func costLimitPreflight() error {
	if costLimit <= 0 {
		return nil
	}
	commitment := launchStorageCommitment()
	if commitment.Empty() || commitment.MonthlyUSD <= costLimit {
		return nil
	}
	if allowCostLimitOverrun {
		return nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "this launch would create storage costing ~$%.2f/month, which exceeds --cost-limit %.2f on its own.\n\n",
		commitment.MonthlyUSD, costLimit)
	for _, item := range commitment.Items {
		fmt.Fprintf(&b, "  %s\n", item)
	}
	fmt.Fprintf(&b, "\n")
	fmt.Fprintf(&b, "  --cost-limit covers storage as well as compute. Storage outlives the instance,\n")
	fmt.Fprintf(&b, "  so spored — which enforces the cap from inside it — cannot reclaim it against the\n")
	fmt.Fprintf(&b, "  cap. A 'durable' filesystem is never reclaimed automatically; an 'ephemeral' one\n")
	fmt.Fprintf(&b, "  only by the out-of-band reaper, and only in an account it covers ('spawn doctor').\n")
	fmt.Fprintf(&b, "\n")
	fmt.Fprintf(&b, "  Raise --cost-limit, shrink the filesystem with --fsx-storage-capacity, or pass\n")
	fmt.Fprintf(&b, "  --allow-cost-limit-overrun if you mean it.")
	return fmt.Errorf("%s", b.String())
}

// costLimitScopeNote is the one-line description of what the cap covers, used in the
// launch preview so the scope is stated where the number is shown rather than only in
// flag help.
func costLimitScopeNote() string {
	if costLimit <= 0 {
		return ""
	}
	return fmt.Sprintf("Cost limit:   $%.2f total (compute + storage; spored stops compute, "+
		"but storage that outlives the instance is only reclaimed by the reaper)", costLimit)
}
