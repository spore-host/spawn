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
	// CreateTags is used only to stamp an untagged resource with the reaper's
	// first-seen time, so it can be aged out on a later cycle rather than
	// skipped forever.
	CreateTags(ctx context.Context, in *ec2.CreateTagsInput, optFns ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error)
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

// netResourceUntaggedGrace is the fallback for a spawn-managed resource carrying
// NO creation stamp.
//
// Skipping untagged resources entirely was the original behaviour and the
// reasoning was sound in isolation: EC2 reports no creation time for either
// resource, so an untimed group could be seconds old, and deleting one a launch
// is about to use would break that launch.
//
// But nothing WROTE the stamp. The two safety properties — "the creator cleans
// up" and "the reaper never guesses an age" — composed into a leak with no
// collector: the creating Lambda could not delete them (#752, a goroutine the
// runtime froze) and the reaper would not. A live sweep found 21 such security
// groups across three regions, all with zero network interfaces and
// spawn:managed=true, which is essentially the entire set #685 first reported,
// still present after the reaper shipped.
//
// 30 days closes that hole without weakening the short grace. Nothing spawn
// creates legitimately sits unused AND untagged for a month: the creation paths
// now stamp every resource (pkg/aws.LifecycleTags), so an untagged resource is
// by definition from a build that predates that, or from a path nobody has
// tagged yet — and in neither case is it a group a launch is about to use.
//
// It is still a GUESS, which is why it is four times the normal grace and why
// the log line says so.
const netResourceUntaggedGrace = 30 * 24 * time.Hour

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
	if acct.netFor == nil || r.netResources == netResourcesOff {
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
		grace := netResourceGrace
		if !ok {
			// No creation stamp: fall back to the discovery age (how long WE have
			// been seeing it) against a much longer grace. Skipping outright is
			// what made these permanently uncollectable.
			age, ok = untaggedAge(sg.Tags, now)
			if !ok {
				log.Printf("net-resources: %s/%s: security group %s (%s) has no %s tag and no "+
					"discovery stamp — tagging it so a later cycle can age it out",
					acct.label, region, id, name, tagprefix.Tag("created"))
				r.stampDiscovery(ctx, cli, id, acct, region, sum)
				continue
			}
			grace = netResourceUntaggedGrace
		}
		if age < grace {
			continue
		}
		if r.netReportOnly() {
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
		grace := netResourceGrace
		if !ok {
			age, ok = untaggedAge(pg.Tags, now)
			if !ok {
				log.Printf("net-resources: %s/%s: placement group %s has no %s tag and no "+
					"discovery stamp — tagging it so a later cycle can age it out",
					acct.label, region, name, tagprefix.Tag("created"))
				r.stampDiscovery(ctx, cli, awssdk.ToString(pg.GroupId), acct, region, sum)
				continue
			}
			grace = netResourceUntaggedGrace
		}
		if age < grace {
			continue
		}
		if r.netReportOnly() {
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

// The three states of network-resource reclamation.
//
// Three rather than a boolean, because the two questions are genuinely separate:
// "should this sweep run at all" and "may it delete". A boolean would have
// collapsed them onto the global DryRun, and production runs DryRun=false — so
// the only way to preview a brand-new destructive sweep would have been to turn
// OFF the live instance reaper, trading an untested delete for a disarmed TTL
// backstop. That is not a trade worth offering.
const (
	// netResourcesOff skips the sweep entirely. No describes, no cost, no log
	// noise — for an account where someone else owns these resources.
	netResourcesOff = "off"
	// netResourcesReport describes and logs what it WOULD reclaim, and deletes
	// nothing. The default, so updating the Lambda is observable but inert.
	netResourcesReport = "report"
	// netResourcesReap deletes. Explicit opt-in, once the report has been read.
	netResourcesReap = "reap"
)

// parseNetResources maps the env var onto a mode, defaulting to report.
//
// An unrecognised value becomes report rather than reap or an error: a typo in a
// CloudFormation parameter must not arm a destructive sweep, and must not
// silently disable one either — report is the only choice that is wrong in a
// recoverable direction. The caller logs what it resolved to.
func parseNetResources(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case netResourcesOff:
		return netResourcesOff
	case netResourcesReap:
		return netResourcesReap
	default:
		return netResourcesReport
	}
}

// netReportOnly reports whether this cycle may delete network resources.
//
// dryRun wins over netResourcesReap deliberately: a dry run means "change
// nothing", and a second switch that could override it would make the dry run a
// lie.
func (r *reaper) netReportOnly() bool {
	return r.dryRun || r.netResources != netResourcesReap
}

// discoveryTagKey records when the reaper FIRST saw an untagged resource.
//
// It exists because neither DescribeSecurityGroups nor DescribePlacementGroups
// reports a creation time, so for a resource with no spawn:created stamp there is
// no age to measure — and "no age" is what made these permanently uncollectable.
// Stamping on first sight converts an unknowable age into a knowable one: not the
// resource's true age, but a lower bound on it, which is the conservative
// direction and all the grace check needs.
//
// Deliberately a SEPARATE key from spawn:created. Backfilling spawn:created would
// claim the resource was created when we noticed it, which is false and would
// also hide the fact that a creation path is not tagging.
const discoveryTagKey = "spawn:reaper-first-seen"

// untaggedAge returns how long the reaper has been observing a resource that has
// no creation stamp, from the discovery tag it writes itself.
func untaggedAge(tags []ec2types.Tag, now time.Time) (time.Duration, bool) {
	for _, t := range tags {
		if awssdk.ToString(t.Key) != discoveryTagKey {
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

// stampDiscovery writes the first-seen tag so a later cycle can age the resource
// out.
//
// Best-effort and non-fatal: a failure here means the resource is simply
// re-stamped next cycle, which is the same outcome as now. Counted as skipped
// either way, because nothing was reclaimed.
func (r *reaper) stampDiscovery(ctx context.Context, cli netResourceAPI, resourceID string, acct account, region string, sum *Summary) {
	sum.NetSkipped++
	if resourceID == "" {
		return
	}
	if r.netReportOnly() {
		// A report-only cycle must not write either. The stamp is harmless, but a
		// mode that claims to change nothing should change nothing — including
		// tags.
		return
	}
	if _, err := cli.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{resourceID},
		Tags: []ec2types.Tag{{
			Key:   awssdk.String(discoveryTagKey),
			Value: awssdk.String(time.Now().UTC().Format(time.RFC3339)),
		}},
	}); err != nil {
		log.Printf("net-resources: %s/%s: could not stamp %s with %s: %v",
			acct.label, region, resourceID, discoveryTagKey, err)
	}
}
