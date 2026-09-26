package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/spore-host/spawn/pkg/aws"
)

// fsxLease is the FSx reference an instance carries, read from its tags.
//
// Two tags are leases on a filesystem (the same pair the ttl-reaper's fsxInUse
// counts, lambda/ttl-reaper/main.go):
//
//   - spawn:fsx-id      — mounted: the filesystem is AVAILABLE and bound here.
//   - spawn:fsx-pending — provisioning: written at launch while spored waits for
//     the filesystem to become AVAILABLE, then flipped to spawn:fsx-id.
//
// Either one means "this instance is the thing keeping that filesystem alive",
// which is exactly what a user needs told when they destroy the instance.
type fsxLease struct {
	FilesystemID string
	Pending      bool   // lease came from spawn:fsx-pending (still provisioning)
	MountPoint   string // spawn:fsx-mount-point, if tagged
	CapacityGiB  int32  // 0 = not looked up / lookup failed
	Status       string // FSx lifecycle state (CREATING/AVAILABLE/…); "" = unknown
	Lifecycle    string // spawn:fsx-lifecycle on the filesystem: ephemeral|durable|"" (unknown / not spawn-created)
}

// fsxLeaseFromTags extracts the lease from an instance's tags. Pure: no AWS
// call, so the notice can be tested from tags alone. The mounted lease wins over
// the pending one if (transiently) both are present.
func fsxLeaseFromTags(tags map[string]string) (fsxLease, bool) {
	if len(tags) == 0 {
		return fsxLease{}, false
	}
	lease := fsxLease{MountPoint: tags["spawn:fsx-mount-point"]}
	switch {
	case tags["spawn:fsx-id"] != "":
		lease.FilesystemID = tags["spawn:fsx-id"]
	case tags["spawn:fsx-pending"] != "":
		lease.FilesystemID = tags["spawn:fsx-pending"]
		lease.Pending = true
	default:
		return fsxLease{}, false
	}
	return lease, true
}

// enrichFSxLease fills in capacity/status/lifecycle with a read-only
// DescribeFileSystems. Best-effort by design: a failed or denied lookup still
// leaves a usable notice (id + "not deleted by this command"), because being
// unable to describe the filesystem is not a reason to go quiet about it.
//
// This is a package-level seam (like resolveLaunchDryRunPrice) so tests never
// need an AWS client.
var enrichFSxLease = func(ctx context.Context, client *aws.Client, region string, lease fsxLease) fsxLease {
	brief, err := client.DescribeFSxBrief(ctx, lease.FilesystemID, region)
	if err != nil {
		return lease
	}
	lease.CapacityGiB = brief.StorageCapacityGiB
	lease.Status = brief.Status
	lease.Lifecycle = brief.SpawnLifecycle
	return lease
}

// renderFSxLeaseNotice returns the block printed when an instance holding an FSx
// lease is terminated or stopped. Pure (string in, string out) so it is tested
// without AWS; empty return means "nothing to say".
//
// The point of this notice (spawn#613): nothing in spawn's terminate path
// touches FSx. Reclamation of an ephemeral filesystem is entirely asynchronous —
// the out-of-band ttl-reaper deletes it once no live instance carries either
// lease, after a grace period, and only in accounts the reaper is configured to
// scan. A user who saw "Terminate request sent!" and nothing else reasonably
// concluded their 1.2 TiB filesystem was gone. It was not, and it billed.
//
// It is read-only advice: terminate deliberately does NOT delete filesystems as
// a side effect. Print, don't act.
func renderFSxLeaseNotice(lease fsxLease, action string) string {
	if lease.FilesystemID == "" {
		return ""
	}

	var b strings.Builder
	size := "size unknown"
	if lease.CapacityGiB > 0 {
		size = fmt.Sprintf("%d GiB", lease.CapacityGiB)
	}
	state := ""
	if lease.Status != "" {
		state = ", " + lease.Status
	}
	fmt.Fprintf(&b, "\n📁 FSx Lustre filesystem %s (%s%s)", lease.FilesystemID, size, state)
	if lease.Pending {
		b.WriteString(" — still provisioning for this instance")
	}
	b.WriteString("\n")

	if monthly := fsxLustreMonthlyUSD(lease.CapacityGiB, 0); monthly > 0 {
		fmt.Fprintf(&b, "   Billing while it exists: ~$%.2f/month at the default throughput tier (approximate).\n", monthly)
	}

	switch action {
	case "terminate":
		b.WriteString("   It is NOT deleted by this command.\n")
		switch lease.Lifecycle {
		case "ephemeral":
			b.WriteString("   Lifecycle 'ephemeral' means it is reclaimed asynchronously once no instance\n")
			b.WriteString("   references it — by the out-of-band reaper, minutes-to-hours later, and only\n")
			b.WriteString("   in accounts the reaper scans. Until then it keeps billing.\n")
		case "durable":
			b.WriteString("   Lifecycle 'durable': it is meant to outlive this instance and bills until its\n")
			b.WriteString("   TTL expires with nothing using it.\n")
		default:
			b.WriteString("   spawn has no lifecycle tag for it (likely a filesystem you created yourself),\n")
			b.WriteString("   so spawn will never reclaim it.\n")
		}
	default: // stop / hibernate
		b.WriteString("   It keeps running and billing while the instance is stopped, and this instance\n")
		b.WriteString("   still holds the lease that prevents any automatic reclamation.\n")
	}

	fmt.Fprintf(&b, "   Confirm:  spawn fsx list\n")
	fmt.Fprintf(&b, "   Delete:   spawn fsx delete %s\n", lease.FilesystemID)
	return b.String()
}

// fsxLeaseNoticeFor resolves and renders the notice for one instance, or "" if
// the instance holds no lease. Read-only end to end (instance tags, then a
// DescribeFileSystems).
func fsxLeaseNoticeFor(ctx context.Context, client *aws.Client, inst aws.InstanceInfo, action string) string {
	lease, ok := fsxLeaseFromTags(inst.Tags)
	if !ok {
		return ""
	}
	return renderFSxLeaseNotice(enrichFSxLease(ctx, client, inst.Region, lease), action)
}

// fsxLeaseNoticesFor renders one notice per DISTINCT filesystem across a set of
// instances. A job array created with --fsx-create shares a single filesystem
// across every member, so naming it N times would be noise.
func fsxLeaseNoticesFor(ctx context.Context, client *aws.Client, insts []aws.InstanceInfo, action string) []string {
	seen := map[string]bool{}
	var notices []string
	for _, inst := range insts {
		lease, ok := fsxLeaseFromTags(inst.Tags)
		if !ok || seen[lease.FilesystemID] {
			continue
		}
		seen[lease.FilesystemID] = true
		if notice := renderFSxLeaseNotice(enrichFSxLease(ctx, client, inst.Region, lease), action); notice != "" {
			notices = append(notices, notice)
		}
	}
	return notices
}
