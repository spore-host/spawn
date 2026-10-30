package cmd

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spore-host/spawn/pkg/aws"
)

// A sweep member's EFA interface count (#1407).
//
// A single `spawn launch --efa` resolves --efa-interfaces 0, the default, to
// one EFA per network card (resolveEFAInterfaces, #661). A sweep never did, so
// every member launched with one interface, at the Launch layer's reading of
// 0: one of two on c8gn.48xlarge, one of sixteen on p5en.48xlarge, and nothing
// said so. Rows may name different instance types and regions, so each member
// is resolved on its own, through one lookup memoized per (region, type).

// memoEFACapacity is efaCapacityFor through client, asked once per (region,
// instance type): a sweep of a hundred rows of one type makes one
// DescribeInstanceTypes call. An error is remembered too.
func memoEFACapacity(ctx context.Context, client *aws.Client) efaCapacityLookup {
	type answer struct {
		capacity aws.EFACapacity
		err      error
	}
	cache := map[string]answer{}
	return func(instanceType, region string) (aws.EFACapacity, error) {
		k := region + "|" + instanceType
		a, ok := cache[k]
		if !ok {
			a.capacity, a.err = efaCapacityFor(ctx, client, instanceType, region)
			cache[k] = a
		}
		return a.capacity, a.err
	}
}

// resolveSweepMemberEFA settles one sweep member's EFAInterfaces as a single
// launch of its config would (resolveEFAInterfacesWith): 0 becomes the type's
// EFA card count, a count above it is refused, and --efa-secondary-type efa is
// refused where that count is 1. An explicit count is kept.
//
// An instance type pattern (a detached sweep's "a|b" or "c8gn.*") names no one
// type to read, so "every card" cannot be resolved for it: refused rather than
// launched with one interface, unless the count is explicit.
func resolveSweepMemberEFA(cfg *aws.LaunchConfig, lookup efaCapacityLookup) error {
	if !cfg.EFAEnabled {
		return nil
	}
	if strings.ContainsAny(cfg.InstanceType, "|*") {
		if cfg.EFAInterfaces == 0 {
			return fmt.Errorf("--efa on the instance type pattern %q: how many EFA network cards a member has depends on "+
				"the type it launches as, so --efa-interfaces 0 (every card) cannot be resolved. Pass --efa-interfaces 1 "+
				"for a single EFA interface, or name one instance type", cfg.InstanceType)
		}
		return cfg.ValidateEFASecondaryType()
	}
	_, err := resolveEFAInterfacesWith(cfg, lookup)
	return err
}

// resolveSweepEFA resolves every foreground member's EFA interface count
// (resolveSweepMemberEFA), before anything is created. indexes are the
// members' parameter-set numbers, for the error (nil: their positions).
func resolveSweepEFA(cfgs []*aws.LaunchConfig, indexes []int, lookup efaCapacityLookup) error {
	for i, cfg := range cfgs {
		if err := resolveSweepMemberEFA(cfg, lookup); err != nil {
			idx := i
			if indexes != nil {
				idx = indexes[i]
			}
			return fmt.Errorf("parameter set %d: %w", idx, err)
		}
	}
	return nil
}

// preflightSweepElasticIPs is preflightElasticIPQuota for a foreground sweep:
// every member with more than one EFA interface gets an Elastic IP, so each
// region must have that many left. Run after resolveSweepEFA.
func preflightSweepElasticIPs(ctx context.Context, client *aws.Client, cfgs []*aws.LaunchConfig, warn io.Writer) error {
	need := map[string]int{}
	first := map[string]*aws.LaunchConfig{}
	for _, cfg := range cfgs {
		if cfg.EFAEnabled && cfg.EFAInterfaces > 1 && !cfg.NoPublicIP { // --no-public-ip: no Elastic IP (#1433)
			need[cfg.Region]++
			if first[cfg.Region] == nil {
				first[cfg.Region] = cfg
			}
		}
	}
	regions := make([]string, 0, len(need))
	for r := range need {
		regions = append(regions, r)
	}
	sort.Strings(regions)
	for _, r := range regions {
		if err := preflightElasticIPQuota(ctx, client, first[r], need[r], warn); err != nil {
			return err
		}
	}
	return nil
}
