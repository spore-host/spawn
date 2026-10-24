package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/spore-host/libs/i18n"
	"github.com/spore-host/spawn/pkg/aws"
)

// orphanNoticeMaxIDs caps how many allocation IDs the warning lists per region.
const orphanNoticeMaxIDs = 5

// orphanAddressFinder returns a region's orphaned launch Elastic IPs
// (aws.IsOrphanedSpawnAddress). *aws.Client's FindOrphanedSpawnAddresses is the
// production one.
type orphanAddressFinder func(ctx context.Context, region string) ([]aws.ManagedResource, error)

type regionOrphans struct {
	region string
	addrs  []aws.ManagedResource
}

// collectOrphanedAddresses asks every region in regions, best-effort: a region
// whose lookup fails (no ec2:DescribeAddresses, throttling, an opt-in region)
// is skipped and the rest are still asked. The warning is a side note on
// `spawn list` / `spawn status`, so it can never fail them (#1232).
func collectOrphanedAddresses(ctx context.Context, regions []string, find orphanAddressFinder) []regionOrphans {
	var out []regionOrphans
	for _, region := range regions {
		addrs, err := find(ctx, region)
		if err != nil || len(addrs) == 0 {
			continue
		}
		out = append(out, regionOrphans{region: region, addrs: addrs})
	}
	return out
}

// orphanedAddressNotice renders the warning, or "" when there is nothing to say.
// The command it prints is the one that releases exactly these addresses:
// `spawn cleanup --elastic-ips` touches nothing else and is not blocked by
// running instances; --all is added when any of them was launched by a
// different principal than callerARN, since cleanup's default scope would skip
// those.
func orphanedAddressNotice(groups []regionOrphans, callerARN string) string {
	var b strings.Builder
	for _, g := range groups {
		ids := make([]string, 0, orphanNoticeMaxIDs)
		foreign := false
		for i, a := range g.addrs {
			if i < orphanNoticeMaxIDs {
				ids = append(ids, a.ID)
			}
			if !a.OwnedBy(callerARN) {
				foreign = true
			}
		}
		list := strings.Join(ids, ", ")
		if extra := len(g.addrs) - len(ids); extra > 0 {
			list += fmt.Sprintf(" (+%d more)", extra)
		}
		cleanup := "spawn cleanup --elastic-ips --region " + g.region
		if foreign {
			cleanup += " --all"
		}
		fmt.Fprintf(&b, "\n%s %d orphaned spawn Elastic IP(s) in %s: %s\n"+
			"    Their instances are gone, but each address still bills and counts toward the region's Elastic IP limit.\n"+
			"    Release them: %s\n",
			i18n.Symbol("warning"), len(g.addrs), g.region, list, cleanup)
	}
	return b.String()
}

// orphanedAddressWarning is the production wiring for list and status: one
// DescribeAddresses per region (plus one DescribeInstances where a candidate
// exists), and a caller-identity lookup only when there is something to warn
// about. Every failure degrades to saying nothing.
func orphanedAddressWarning(ctx context.Context, client *aws.Client, regions []string) string {
	groups := collectOrphanedAddresses(ctx, regions, client.FindOrphanedSpawnAddresses)
	if len(groups) == 0 {
		return ""
	}
	// An unresolved caller leaves callerARN empty, so any address with an owner
	// tag reads as foreign and the command gains --all: wider than needed,
	// never too narrow to release them.
	_, callerARN, _ := client.GetCallerIdentityInfo(ctx)
	return orphanedAddressNotice(groups, callerARN)
}
