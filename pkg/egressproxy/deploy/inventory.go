package deploy

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/spore-host/spawn/pkg/egressproxy"
)

// Listed is an instance tagged as a VPC's egress proxy, as `spawn egress-proxy
// status`, `delete` and `upgrade` see it (#1515 phase 5).
type Listed struct {
	// Proxy is what the instance says about itself. Its Deadline is the
	// parsed spawn:ttl-deadline when that is a time, whether or not it passes
	// the checks.
	Proxy    Proxy
	PublicIP string
	// NotSpawns is [Verify]'s reason: why spawn cannot tell it launched this
	// instance as the VPC's proxy. "" when it can. delete and upgrade
	// terminate only instances for which it is "".
	NotSpawns string
	// NotReused is [Evaluate]'s reason: why find-or-create would not reuse it.
	// "" when it would.
	NotReused string
	// LastActivity and Idle are what spored published in
	// [egressproxy.ActivityTag] and [egressproxy.IdleTag]; zero when absent
	// or unreadable (a proxy older than [egressproxy.ActivityTagsSince]).
	LastActivity time.Time
	Idle         time.Duration
}

// Launched is the proxy's launch time: the earlier of its spawn:launch-time
// tag and EC2's LaunchTime (pkg/ttl LaunchAnchor, the reaper's rule), so a
// rewritten tag cannot make it younger.
func (l Listed) Launched() time.Time { return l.Proxy.LaunchTime }

// IdleExpiry is when the idle period ends if nothing is proxied before then:
// zero when the activity tags are absent.
func (l Listed) IdleExpiry() time.Time {
	if l.LastActivity.IsZero() || l.Idle <= 0 {
		return time.Time{}
	}
	return l.LastActivity.Add(l.Idle)
}

// Expiry is when the proxy ends if nothing changes: the earlier of its idle
// expiry and its deadline, either of which may be unknown (zero).
func (l Listed) Expiry() time.Time {
	idle, dl := l.IdleExpiry(), l.Proxy.Deadline
	switch {
	case idle.IsZero():
		return dl
	case dl.IsZero() || idle.Before(dl):
		return idle
	}
	return dl
}

// VPCInventory is one VPC's proxies and groups.
type VPCInventory struct {
	VPCID   string
	Groups  GroupsState
	Proxies []Listed
}

// InventoryStates are the instance states an inventory lists: all but
// terminated.
var InventoryStates = []string{"pending", "running", "shutting-down", "stopping", "stopped"}

// Inventory reads, through api (one region), every instance tagged [ProxyTag]
// in a state of [InventoryStates], and the two groups of every VPC that either
// a tagged instance or a tagged group names; only vpcID's when it is not "".
// Each instance is judged by [Verify] and [Evaluate] against w (w.VPCID and
// w.ProxyGroupID are filled per VPC). Read-only: ec2:DescribeInstances and
// ec2:DescribeSecurityGroups. VPCs come sorted, and each VPC's proxies by
// launch time, latest first.
func Inventory(ctx context.Context, api EC2, vpcID string, w Want) ([]VPCInventory, error) {
	byVPC := map[string][]types.Instance{}
	filters := []types.Filter{{Name: aws.String("instance-state-name"), Values: InventoryStates}}
	if vpcID != "" {
		filters = append(filters, types.Filter{Name: aws.String("tag:" + ProxyTag), Values: []string{vpcID}})
	} else {
		filters = append(filters, types.Filter{Name: aws.String("tag-key"), Values: []string{ProxyTag}})
	}
	pager := ec2.NewDescribeInstancesPaginator(api, &ec2.DescribeInstancesInput{Filters: filters})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list the egress proxies: %w", err)
		}
		for _, r := range page.Reservations {
			for _, inst := range r.Instances {
				v := tagMap(inst.Tags)[ProxyTag]
				if v == "" || (vpcID != "" && v != vpcID) || !hasString(InventoryStates, stateOf(inst)) {
					continue
				}
				byVPC[v] = append(byVPC[v], inst)
			}
		}
	}
	vpcs := map[string]bool{}
	for v := range byVPC {
		vpcs[v] = true
	}
	if vpcID != "" {
		vpcs[vpcID] = true
	} else {
		// VPCs with groups and no proxy: their groups are still spawn's to
		// show and to delete.
		out, err := api.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
			Filters: []types.Filter{{Name: aws.String("tag-key"), Values: []string{ProxyTag}}},
		})
		if err != nil {
			return nil, fmt.Errorf("list the egress proxy security groups: %w", err)
		}
		for _, sg := range out.SecurityGroups {
			if v := tagMap(sg.Tags)[ProxyTag]; v != "" && v == aws.ToString(sg.VpcId) {
				vpcs[v] = true
			}
		}
	}

	var out []VPCInventory
	for _, v := range sortedKeys(vpcs) {
		groups, err := DescribeGroups(ctx, api, v)
		if err != nil {
			return out, err
		}
		inv := VPCInventory{VPCID: v, Groups: groups}
		vw := w
		vw.VPCID, vw.ProxyGroupID = v, groups.Groups().ProxyID
		for _, inst := range byVPC[v] {
			inv.Proxies = append(inv.Proxies, list(inst, vw))
		}
		sort.SliceStable(inv.Proxies, func(i, j int) bool {
			return inv.Proxies[i].Proxy.LaunchTime.After(inv.Proxies[j].Proxy.LaunchTime)
		})
		out = append(out, inv)
	}
	return out, nil
}

func list(inst types.Instance, w Want) Listed {
	_, notSpawns := Verify(inst, w)
	p, notReused := Evaluate(inst, w)
	tags := tagMap(inst.Tags)
	if p.Deadline.IsZero() {
		if dl, err := time.Parse(time.RFC3339, tags["spawn:ttl-deadline"]); err == nil {
			p.Deadline = dl
		}
	}
	l := Listed{Proxy: p, PublicIP: aws.ToString(inst.PublicIpAddress), NotSpawns: notSpawns, NotReused: notReused}
	if t, err := time.Parse(time.RFC3339, tags[egressproxy.ActivityTag]); err == nil {
		l.LastActivity = t
	}
	if d, err := time.ParseDuration(tags[egressproxy.IdleTag]); err == nil && d > 0 {
		l.Idle = d
	}
	return l
}

func stateOf(inst types.Instance) string {
	if inst.State == nil {
		return ""
	}
	return string(inst.State.Name)
}

func hasString(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
