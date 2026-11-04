package main

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spore-host/spawn/pkg/reaperconfig"
	"github.com/spore-host/spawn/pkg/tagprefix"
)

// The spawn egress proxy (#1515) has no reaper code of its own: the reaper
// covers it as it covers any spawn instance and any spawn-managed security
// group. These tests hold that against the tags the proxy's producers actually
// write, read from testdata/egress_proxy_tags.json, which
// pkg/egressproxy/deploy TestReaperFixtureIsCurrent generates from them and
// fails on when it is stale. Not hand-copied: this module does not import the
// CLI's packages, so the file is how the tags cross.

type egressProxyFixture struct {
	LaunchTime   string            `json:"launch_time"`
	Deadline     string            `json:"deadline"`
	InstanceTags map[string]string `json:"instance_tags"`
	Groups       []struct {
		Name       string            `json:"name"`
		Tags       map[string]string `json:"tags"`
		References string            `json:"references"`
	} `json:"groups"`
}

func loadEgressProxyFixture(t *testing.T) egressProxyFixture {
	t.Helper()
	b, err := os.ReadFile("testdata/egress_proxy_tags.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx egressProxyFixture
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatal(err)
	}
	if len(fx.InstanceTags) == 0 || len(fx.Groups) != 2 {
		t.Fatalf("fixture has %d instance tags and %d groups; regenerate it", len(fx.InstanceTags), len(fx.Groups))
	}
	return fx
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func ec2Tags(m map[string]string) []ec2types.Tag {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]ec2types.Tag, 0, len(m))
	for _, k := range keys {
		out = append(out, ec2types.Tag{Key: awssdk.String(k), Value: awssdk.String(m[k])})
	}
	return out
}

// The proxy is in the instance scan (spawn:managed=true), and past its
// spawn:ttl-deadline it is reaped as any instance is; its deadline is inside
// the max-age ceiling, so spored's own terminate comes first.
func TestReaperTerminatesAnEgressProxyPastItsDeadline(t *testing.T) {
	fx := loadEgressProxyFixture(t)
	launch, deadline := mustTime(t, fx.LaunchTime), mustTime(t, fx.Deadline)
	if v := fx.InstanceTags[strings.TrimPrefix(tagprefix.FilterTag("managed"), "tag:")]; v != "true" {
		t.Fatalf("the proxy's %s is %q: the instance scan's filter would not find it", tagprefix.FilterTag("managed"), v)
	}
	if !deadline.Before(launch.Add(defaultMaxAge)) {
		t.Errorf("the proxy's deadline %s is not inside the %v ceiling from launch %s", deadline, defaultMaxAge, launch)
	}
	inst := ec2types.Instance{
		InstanceId: awssdk.String("i-proxy"),
		State:      &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
		LaunchTime: awssdk.Time(launch),
		Tags:       ec2Tags(fx.InstanceTags),
	}
	r := &reaper{maxAge: defaultMaxAge, ttlGrace: reaperconfig.DefaultTTLGrace}
	if _, expired := r.evaluate(inst, "us-west-2", deadline.Add(-time.Minute)); expired {
		t.Error("reaped before its deadline")
	}
	c, expired := r.evaluate(inst, "us-west-2", deadline.Add(time.Minute))
	if !expired || c.reason != "ttl-deadline" {
		t.Errorf("a minute past its deadline (no heartbeat): expired=%v reason=%q, want the ttl-deadline reap", expired, c.reason)
	}
}

// netRefsAPI is fakeNetAPI with EC2's refusal to delete a group another
// group's rule references, which the proxy group's ingress rule does to the
// clients group.
type netRefsAPI struct {
	*fakeNetAPI
	refs map[string]string // group ID -> the group ID its rules reference
}

func (f *netRefsAPI) DeleteSecurityGroup(ctx context.Context, in *ec2.DeleteSecurityGroupInput, o ...func(*ec2.Options)) (*ec2.DeleteSecurityGroupOutput, error) {
	id := awssdk.ToString(in.GroupId)
	for from, to := range f.refs {
		if to == id && !strings.Contains(strings.Join(f.deletedSG, ","), from) {
			return nil, &dependencyViolation{id}
		}
	}
	out, err := f.fakeNetAPI.DeleteSecurityGroup(ctx, in, o...)
	if err == nil {
		var keep []ec2types.SecurityGroup
		for _, g := range f.sgs {
			if awssdk.ToString(g.GroupId) != id {
				keep = append(keep, g)
			}
		}
		f.sgs = keep
	}
	return out, err
}

type dependencyViolation struct{ id string }

func (e *dependencyViolation) Error() string {
	return "api error DependencyViolation: resource " + e.id + " has a dependent object"
}

func egressProxyGroups(t *testing.T, fx egressProxyFixture) (*netRefsAPI, map[string]string, time.Time) {
	t.Helper()
	f := &netRefsAPI{fakeNetAPI: &fakeNetAPI{}, refs: map[string]string{}}
	ids := map[string]string{}
	var created time.Time
	for i, g := range fx.Groups {
		id := "sg-" + string(rune('a'+i))
		ids[g.Name] = id
		created = mustTime(t, g.Tags[tagprefix.Tag("created")])
		f.sgs = append(f.sgs, ec2types.SecurityGroup{
			GroupId: awssdk.String(id), GroupName: awssdk.String(g.Name), VpcId: awssdk.String("vpc-1"), Tags: ec2Tags(g.Tags),
		})
	}
	for _, g := range fx.Groups {
		if g.References != "" {
			f.refs[ids[g.Name]] = ids[g.References]
		}
	}
	return f, ids, created
}

func sweepGroups(f *netRefsAPI, now time.Time) {
	r := &reaper{netResources: netResourcesReap, elasticIPs: elasticIPsOff}
	acct := account{label: "self", netFor: func(string) netResourceAPI { return f }}
	r.reapNetResourcesRegion(context.Background(), acct, "us-west-2", now, &Summary{}, &accountOutcome{})
}

// Both groups carry spawn:managed and a spawn:created stamp, so once no
// instance carries them and the grace has passed, the managed-network sweep
// deletes both: the proxy group, then (on the same or the next cycle, as EC2
// refuses the clients group while the proxy group's rule references it) the
// clients group. Before the grace, or while a member carries the clients
// group, it is kept.
func TestReaperDeletesTheEgressProxyGroupsOnceUnused(t *testing.T) {
	fx := loadEgressProxyFixture(t)
	for _, g := range fx.Groups {
		if g.Tags[tagprefix.Tag("managed")] != "true" {
			t.Fatalf("group %s is not spawn:managed: the sweep's describe filter would not find it", g.Name)
		}
	}

	f, ids, created := egressProxyGroups(t, fx)
	sweepGroups(f, created.Add(netResourceGrace-time.Hour))
	if len(f.deletedSG) != 0 {
		t.Fatalf("deleted %v inside the grace", f.deletedSG)
	}
	if v := f.sgFilters; len(v) != 1 || awssdk.ToString(v[0].Name) != "tag:"+tagprefix.Tag("managed") || v[0].Values[0] != "true" {
		t.Fatalf("the sweep's group filter %+v is not the spawn:managed=true the fixture carries", v)
	}

	after := created.Add(netResourceGrace + time.Hour)
	sweepGroups(f, after)
	sweepGroups(f, after.Add(time.Hour))
	got := strings.Join(f.deletedSG, ",")
	want := ids["spawn-egress-proxy-vpc-1"] + "," + ids["spawn-egress-clients-vpc-1"]
	if got != want {
		t.Errorf("deleted %q over two cycles, want the proxy group then the clients group (%q)", got, want)
	}

	// A member still carries the clients group: only the proxy group goes.
	f, ids, created = egressProxyGroups(t, fx)
	f.instances = []ec2types.Instance{{
		InstanceId:     awssdk.String("i-member"),
		State:          &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
		SecurityGroups: []ec2types.GroupIdentifier{{GroupId: awssdk.String(ids["spawn-egress-clients-vpc-1"])}},
	}}
	after = created.Add(netResourceGrace + time.Hour)
	sweepGroups(f, after)
	sweepGroups(f, after.Add(time.Hour))
	if got := strings.Join(f.deletedSG, ","); got != ids["spawn-egress-proxy-vpc-1"] {
		t.Errorf("with a member on the clients group, deleted %q; want only the proxy group", got)
	}
}
