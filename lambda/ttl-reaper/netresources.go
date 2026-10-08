package main

import (
	"context"
	"log"
	"strings"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spore-host/spawn/pkg/tagprefix"
)

// netResourceAPI is the slice of EC2 this sweep needs.
//
// Deliberately NOT added to ec2API: that interface is the instance scan's, and
// the test fakes implementing it would all have to grow four methods they do not
// use. A separate interface keeps the blast radius of this feature to this file.
type netResourceAPI interface {
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	DescribeSecurityGroups(ctx context.Context, in *ec2.DescribeSecurityGroupsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error)
	DeleteSecurityGroup(ctx context.Context, in *ec2.DeleteSecurityGroupInput, optFns ...func(*ec2.Options)) (*ec2.DeleteSecurityGroupOutput, error)
	DescribePlacementGroups(ctx context.Context, in *ec2.DescribePlacementGroupsInput, optFns ...func(*ec2.Options)) (*ec2.DescribePlacementGroupsOutput, error)
	DeletePlacementGroup(ctx context.Context, in *ec2.DeletePlacementGroupInput, optFns ...func(*ec2.Options)) (*ec2.DeletePlacementGroupOutput, error)
}

// netResourceGrace is how long a spawn-managed security group or placement group
// may sit with no instance referencing it before the reaper reclaims it.
//
// Seven days, and the number is a policy decision rather than a technical one:
// nothing spawn creates should be durable indefinitely. The MPI security group is
// the awkward case — it is keyed on spawn-mpi-<job-array-name> and reused by name
// across runs, which is why CreateOrGetMPISecurityGroup is a get-or-create and
// why #659 backfills rules onto an existing group. So a short grace would force a
// recreate on every repeat run of the same named array.
//
// Seven days makes a named array re-run within a week free and anything older
// pay a recreate, which is cheap: the group is rebuilt on demand and #659's
// backfill already handles an existing one. Neither resource costs money — the
// pressure is the per-VPC quota, which bites at LAUNCH time, the worst moment to
// discover it.
const netResourceGrace = 7 * 24 * time.Hour

// reapNetResourcesRegion reclaims spawn-managed security groups and cluster
// placement groups that no instance references and that are older than
// netResourceGrace.
//
// This is spawn#685's other half. The ordering and race bugs were fixed in the
// CLI (v0.121.0) and discovery was added to `spawn orphans`/`cleanup`, but
// nothing reclaimed these without someone running a command: 19 security groups
// and 9 placement groups were found in one region of one account, the oldest
// three months old.
//
// Membership is asked of EC2 rather than inferred from spawn's own instances,
// because ANY member blocks the delete — including one spawn did not create.
// Inferring would report a group as empty and then fail to delete it.
func (r *reaper) reapNetResourcesRegion(ctx context.Context, acct account, region string, now time.Time, sum *Summary, outcome *accountOutcome) {
	if acct.netFor == nil {
		return
	}
	cli := acct.netFor(region)

	// One DescribeInstances for the whole region: which groups and placement
	// groups are occupied. Terminated instances hold nothing; shutting-down ones
	// still block a delete, so they count as occupied and the next cycle picks
	// the resource up.
	occupiedSG := map[string]bool{}
	occupiedPG := map[string]bool{}
	instPager := ec2.NewDescribeInstancesPaginator(cli, &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{{
			Name:   awssdk.String("instance-state-name"),
			Values: []string{"pending", "running", "shutting-down", "stopping", "stopped"},
		}},
	})
	for instPager.HasMorePages() {
		page, perr := instPager.NextPage(ctx)
		if perr != nil {
			log.Printf("net-resources: %s/%s: describe instances: %v", acct.label, region, perr)
			return
		}
		for _, res := range page.Reservations {
			for _, inst := range res.Instances {
				for _, g := range inst.SecurityGroups {
					if id := awssdk.ToString(g.GroupId); id != "" {
						occupiedSG[id] = true
					}
				}
				if inst.Placement != nil {
					if n := awssdk.ToString(inst.Placement.GroupName); n != "" {
						occupiedPG[n] = true
					}
				}
			}
		}
	}

	r.reapSecurityGroups(ctx, cli, acct, region, now, occupiedSG, sum, outcome)
	r.reapPlacementGroups(ctx, cli, acct, region, now, occupiedPG, sum, outcome)
}

func (r *reaper) reapSecurityGroups(ctx context.Context, cli netResourceAPI, acct account, region string, now time.Time, occupied map[string]bool, sum *Summary, outcome *accountOutcome) {
	out, err := cli.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		Filters: []ec2types.Filter{{
			Name:   awssdk.String("tag:" + tagprefix.Tag("managed")),
			Values: []string{"true"},
		}},
	})
	if err != nil {
		log.Printf("net-resources: %s/%s: describe security groups: %v", acct.label, region, err)
		return
	}

	for _, sg := range out.SecurityGroups {
		id := awssdk.ToString(sg.GroupId)
		name := awssdk.ToString(sg.GroupName)
		if id == "" || occupied[id] {
			continue
		}
		// The VPC default group is never spawn's to delete, and EC2 refuses
		// anyway — but tagging accidents happen, so do not even try.
		if name == "default" {
			continue
		}
		age, ok := netResourceAge(sg.Tags, now)
		if !ok {
			// No creation stamp. Skipped rather than guessed: an untimed group
			// could be seconds old, and deleting a group a launch is about to use
			// would break that launch. `spawn orphans` still surfaces it.
			log.Printf("net-resources: %s/%s: security group %s (%s) has no %s tag — skipping (age unknown)",
				acct.label, region, id, name, tagprefix.Tag("created"))
			sum.NetSkipped++
			continue
		}
		if age < netResourceGrace {
			continue
		}
		if r.dryRun {
			log.Printf("WOULD reap security group %s (%s) in %s/%s — unused for %s",
				id, name, acct.label, region, age.Round(time.Hour))
			sum.NetSkipped++
			continue
		}
		if _, derr := cli.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{GroupId: awssdk.String(id)}); derr != nil {
			// DependencyViolation means something still references it that
			// DescribeInstances does not show — another group's rule, an ENI, a
			// load balancer. Not an error worth alarming on; the next cycle
			// retries and `spawn orphans` shows it meanwhile.
			if strings.Contains(derr.Error(), "DependencyViolation") {
				sum.NetSkipped++
				continue
			}
			log.Printf("net-resources: %s/%s: delete security group %s: %v", acct.label, region, id, derr)
			outcome.record(err)
			continue
		}
		log.Printf("REAPED security group %s (%s) in %s/%s — unused for %s",
			id, name, acct.label, region, age.Round(time.Hour))
		sum.NetReaped++
	}
}

func (r *reaper) reapPlacementGroups(ctx context.Context, cli netResourceAPI, acct account, region string, now time.Time, occupied map[string]bool, sum *Summary, outcome *accountOutcome) {
	out, err := cli.DescribePlacementGroups(ctx, &ec2.DescribePlacementGroupsInput{
		Filters: []ec2types.Filter{{
			Name:   awssdk.String("tag:" + tagprefix.Tag("managed")),
			Values: []string{"true"},
		}},
	})
	if err != nil {
		log.Printf("net-resources: %s/%s: describe placement groups: %v", acct.label, region, err)
		return
	}

	for _, pg := range out.PlacementGroups {
		// Placement groups delete by NAME, not id — getting this wrong is how
		// spawn#713's undeletable copy happened.
		name := awssdk.ToString(pg.GroupName)
		if name == "" || occupied[name] {
			continue
		}
		if pg.State != ec2types.PlacementGroupStateAvailable {
			continue // creating/deleting/failed: nothing to do, and delete would fail
		}
		age, ok := netResourceAge(pg.Tags, now)
		if !ok {
			log.Printf("net-resources: %s/%s: placement group %s has no %s tag — skipping (age unknown)",
				acct.label, region, name, tagprefix.Tag("created"))
			sum.NetSkipped++
			continue
		}
		if age < netResourceGrace {
			continue
		}
		if r.dryRun {
			log.Printf("WOULD reap placement group %s in %s/%s — unused for %s",
				name, acct.label, region, age.Round(time.Hour))
			sum.NetSkipped++
			continue
		}
		if _, derr := cli.DeletePlacementGroup(ctx, &ec2.DeletePlacementGroupInput{GroupName: awssdk.String(name)}); derr != nil {
			if strings.Contains(derr.Error(), "InvalidPlacementGroup.InUse") {
				// A member appeared between the describe and the delete, or is
				// mid-termination. The next cycle gets it.
				sum.NetSkipped++
				continue
			}
			log.Printf("net-resources: %s/%s: delete placement group %s: %v", acct.label, region, name, derr)
			outcome.record(err)
			continue
		}
		log.Printf("REAPED placement group %s in %s/%s — unused for %s",
			name, acct.label, region, age.Round(time.Hour))
		sum.NetReaped++
	}
}

// netResourceAge returns how long ago the resource was created, from spawn's own
// creation tag.
//
// ok=false when there is no usable stamp, and the caller SKIPS rather than
// assuming old. Neither DescribeSecurityGroups nor DescribePlacementGroups
// returns a creation time, so the tag is the only source — and an untimed group
// could be seconds old, where deleting it would break the launch that is about to
// use it.
func netResourceAge(tags []ec2types.Tag, now time.Time) (time.Duration, bool) {
	for _, t := range tags {
		if awssdk.ToString(t.Key) != tagprefix.Tag("created") {
			continue
		}
		ts, err := time.Parse(time.RFC3339, awssdk.ToString(t.Value))
		if err != nil {
			return 0, false
		}
		age := now.Sub(ts)
		if age < 0 {
			return 0, false // clock skew; treat as unknown rather than negative
		}
		return age, true
	}
	return 0, false
}
