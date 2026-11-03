package deploy

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/spore-host/spawn/pkg/egressproxy"
	"github.com/spore-host/spawn/pkg/ttl"
)

// Want is what a reusable proxy must be.
type Want struct {
	VPCID string
	// Version is this spawn's buildinfo version. A proxy launched by any
	// other is replaced: a proxy upgrade is a spawn upgrade.
	Version string
	// ProxyGroupID is the VPC's proxy group (Groups.ProxyID).
	ProxyGroupID string
	// InstanceProfile is the name of the profile spawn launches proxies
	// with (pkg/aws SporedInstanceProfileName).
	InstanceProfile string
	Now             time.Time
	// ReuseMargin is how much life a proxy must have left;
	// [egressproxy.ReuseMargin] when zero.
	ReuseMargin time.Duration
}

// deadlineSlack absorbs the second or so between a proxy's spawn:launch-time
// tag and EC2's LaunchTime, which the age anchor takes the earlier of.
const deadlineSlack = 5 * time.Minute

// Evaluate decides whether inst is a proxy a launch in w.VPCID may reuse: the
// proxy, and "" if so; else why not. Pure.
//
// What makes it spawn's proxy. spawn has no signature to check yet: #76's
// signed tag does not exist, and the only spore.host signatures today are the
// ones the bootstrap verifies on spored and on its own body, made by a key the
// CLI does not hold. Tags alone are not proof, because an instance's own role
// may write spawn:* tags on itself (#76), so any spore in the VPC could name
// itself the proxy and have every member's bootstrap sent to it. So a proxy
// must also be what its tags cannot make it:
//
//   - its security groups are exactly the VPC's proxy group, which spawn
//     created and which only an EC2 API caller can attach (the spored role has
//     no ec2:ModifyInstanceAttribute or ModifyNetworkInterfaceAttribute), and
//   - it runs with spawn's spored instance profile, and
//   - its deadline is no later than its launch, by EC2's own clock, plus
//     [egressproxy.MaxLife]: a deadline written later by the instance is
//     refused rather than trusted.
//
// Then it must be one this spawn launched (spawn:version equal to w.Version),
// pending or running, and have more than the reuse margin left.
func Evaluate(inst types.Instance, w Want) (Proxy, string) {
	tags := tagMap(inst.Tags)
	p := Proxy{
		InstanceID:       aws.ToString(inst.InstanceId),
		PrivateIP:        aws.ToString(inst.PrivateIpAddress),
		SubnetID:         aws.ToString(inst.SubnetId),
		Version:          tags["spawn:version"],
		LaunchTime:       ttl.LaunchAnchor(tags["spawn:launch-time"], aws.ToTime(inst.LaunchTime)),
		AvailabilityZone: "",
	}
	if inst.Placement != nil {
		p.AvailabilityZone = aws.ToString(inst.Placement.AvailabilityZone)
	}
	if inst.State != nil {
		p.State = string(inst.State.Name)
	}
	margin := w.ReuseMargin
	if margin <= 0 {
		margin = egressproxy.ReuseMargin
	}

	switch p.State {
	case string(types.InstanceStateNamePending), string(types.InstanceStateNameRunning):
	default:
		return p, "it is " + orUnknown(p.State) + ", not pending or running"
	}
	if tags[ProxyTag] != w.VPCID || aws.ToString(inst.VpcId) != w.VPCID {
		return p, fmt.Sprintf("it is not %s's proxy (%s=%q, in %s)", w.VPCID, ProxyTag, tags[ProxyTag], orUnknown(aws.ToString(inst.VpcId)))
	}
	if tags["spawn:managed"] != "true" {
		return p, "it is not spawn:managed"
	}

	// Provenance: what the instance cannot change about itself.
	groups := map[string]bool{}
	for _, g := range inst.SecurityGroups {
		groups[aws.ToString(g.GroupId)] = true
	}
	if w.ProxyGroupID == "" || len(groups) != 1 || !groups[w.ProxyGroupID] {
		return p, fmt.Sprintf("its security groups are %s, not exactly the proxy group %s, so spawn cannot tell it launched it",
			orUnknown(join(sortedKeys(groups))), orUnknown(w.ProxyGroupID))
	}
	profile := ""
	if inst.IamInstanceProfile != nil {
		profile = aws.ToString(inst.IamInstanceProfile.Arn)
	}
	if w.InstanceProfile == "" || !strings.HasSuffix(profile, ":instance-profile/"+w.InstanceProfile) {
		return p, fmt.Sprintf("its instance profile is %s, not %s, so spawn cannot tell it launched it",
			orUnknown(profile), orUnknown(w.InstanceProfile))
	}
	if p.LaunchTime.IsZero() {
		return p, "it has no launch time to measure its life from"
	}
	dl, err := time.Parse(time.RFC3339, tags["spawn:ttl-deadline"])
	if err != nil {
		return p, fmt.Sprintf("its spawn:ttl-deadline %q is not a time", tags["spawn:ttl-deadline"])
	}
	p.Deadline = dl
	if limit := p.LaunchTime.Add(egressproxy.MaxLife + deadlineSlack); dl.After(limit) {
		return p, fmt.Sprintf("its deadline %s is later than launch plus the proxy's %v life (%s), which spawn never writes",
			dl.UTC().Format(time.RFC3339), egressproxy.MaxLife, limit.UTC().Format(time.RFC3339))
	}

	// Freshness.
	if w.Version == "" || p.Version != w.Version {
		return p, fmt.Sprintf("it was launched by spawn %s, not %s", orUnknown(p.Version), orUnknown(w.Version))
	}
	if left := dl.Sub(w.Now); left <= margin {
		return p, fmt.Sprintf("it has %v left before its deadline, not more than %v", left.Round(time.Minute), margin)
	}
	if p.PrivateIP == "" {
		return p, "it has no private IP yet"
	}
	return p, ""
}

func orUnknown(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// Rejection is a proxy [Find] would not reuse, and why.
type Rejection struct {
	InstanceID, Reason string
}

// Find lists the VPC's pending and running proxies and evaluates each. The
// reusable ones come first, latest deadline first.
func Find(ctx context.Context, api EC2, w Want) (reusable []Proxy, rejected []Rejection, err error) {
	in := &ec2.DescribeInstancesInput{Filters: []types.Filter{
		{Name: aws.String("tag:" + ProxyTag), Values: []string{w.VPCID}},
		{Name: aws.String("instance-state-name"), Values: []string{"pending", "running"}},
	}}
	p := ec2.NewDescribeInstancesPaginator(api, in)
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("list the egress proxies of %s: %w", w.VPCID, err)
		}
		for _, r := range page.Reservations {
			for _, inst := range r.Instances {
				proxy, why := Evaluate(inst, w)
				if why != "" {
					rejected = append(rejected, Rejection{InstanceID: proxy.InstanceID, Reason: why})
					continue
				}
				reusable = append(reusable, proxy)
			}
		}
	}
	sort.SliceStable(reusable, func(i, j int) bool { return reusable[i].Deadline.After(reusable[j].Deadline) })
	return reusable, rejected, nil
}
