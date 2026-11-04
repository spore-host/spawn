package deploy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/egressproxy"
)

// writeCalls are the fake's calls that change something.
var writeCalls = []string{
	"RunInstances", "TerminateInstances", "CreateSecurityGroup", "DeleteSecurityGroup",
	"AuthorizeSecurityGroupIngress", "AuthorizeSecurityGroupEgress",
	"RevokeSecurityGroupIngress", "RevokeSecurityGroupEgress",
}

func writes(f *fakeEC2) []string {
	var out []string
	for _, c := range f.calls {
		if hasString(writeCalls, c) {
			out = append(out, c)
		}
	}
	return out
}

// terminateOn moves a shutting-down instance to terminated when it is
// described by ID, and marks any other instance ready, as heartbeatOn does.
func terminateOn(i *types.Instance, n int) {
	if i.State != nil && i.State.Name == types.InstanceStateNameShuttingDown {
		i.State = &types.InstanceState{Name: types.InstanceStateNameTerminated}
		return
	}
	heartbeatOn(i, n)
}

// member is an instance in the VPC carrying the clients group on two
// network interfaces.
func member(id, clientsID string) types.Instance {
	gi := []types.GroupIdentifier{{GroupId: aws.String("sg-default")}, {GroupId: aws.String(clientsID)}}
	return types.Instance{
		InstanceId: aws.String(id), VpcId: aws.String(testVPC),
		State:          &types.InstanceState{Name: types.InstanceStateNameRunning},
		SecurityGroups: gi,
		NetworkInterfaces: []types.InstanceNetworkInterface{
			{NetworkInterfaceId: aws.String("eni-" + id + "-0"), Groups: gi},
			{NetworkInterfaceId: aws.String("eni-" + id + "-1"), Groups: gi},
		},
	}
}

func inventoryWant() Want {
	return Want{Version: testVersion, InstanceProfile: testProfile, Now: t0}
}

func oneVPC(t *testing.T, f *fakeEC2) VPCInventory {
	t.Helper()
	inv, err := Inventory(context.Background(), f, testVPC, inventoryWant())
	if err != nil {
		t.Fatal(err)
	}
	if len(inv) != 1 || inv[0].VPCID != testVPC {
		t.Fatalf("inventory %+v, want exactly %s", inv, testVPC)
	}
	return inv[0]
}

// forged is an instance tagged as the proxy that spawn did not launch as one:
// it carries a second security group.
func forged(id, proxySG string) types.Instance {
	inst := goodProxy(id, proxySG)
	inst.SecurityGroups = append(inst.SecurityGroups, types.GroupIdentifier{GroupId: aws.String("sg-other")})
	return inst
}

// Status's view: each tagged instance judged by Verify and Evaluate, the
// published activity read back, and who uses the clients group, by ENI.
func TestInventory(t *testing.T) {
	f := &fakeEC2{}
	g, err := EnsureGroups(context.Background(), f, testVPC, t0)
	if err != nil {
		t.Fatal(err)
	}
	current := goodProxy("i-current", g.ProxyID)
	setTag(&current, egressproxy.ActivityTag, t0.Add(-2*time.Hour).Format(time.RFC3339))
	setTag(&current, egressproxy.IdleTag, "72h0m0s")
	old := goodProxy("i-old", g.ProxyID)
	old.LaunchTime = aws.Time(t0.Add(-2 * time.Hour))
	setTag(&old, "spawn:launch-time", t0.Add(-2*time.Hour).Format(time.RFC3339))
	setTag(&old, "spawn:ttl-deadline", t0.Add(-2*time.Hour+egressproxy.MaxLife).Format(time.RFC3339))
	setTag(&old, "spawn:version", "0.156.0")
	f.instances = []types.Instance{current, old, forged("i-forged", g.ProxyID), member("i-member", g.ClientsID)}
	f.calls = nil

	inv := oneVPC(t, f)
	if w := writes(f); len(w) != 0 {
		t.Errorf("the inventory wrote: %v", w)
	}
	if inv.Groups.Proxy.ID != g.ProxyID || inv.Groups.Clients.ID != g.ClientsID {
		t.Errorf("groups %+v, want %+v", inv.Groups, g)
	}
	if u := inv.Groups.Clients.Users; len(u) != 1 || u[0] != "i-member" || inv.Groups.Clients.ENIs != 2 {
		t.Errorf("clients group users %v (%d ENIs), want i-member on 2 ENIs", u, inv.Groups.Clients.ENIs)
	}
	by := map[string]Listed{}
	for _, l := range inv.Proxies {
		by[l.Proxy.InstanceID] = l
	}
	if len(by) != 3 {
		t.Fatalf("listed %v, want the three tagged instances and not the member", inv.Proxies)
	}
	if l := by["i-current"]; l.NotSpawns != "" || l.NotReused != "" {
		t.Errorf("current: %+v", l)
	} else if !l.LastActivity.Equal(t0.Add(-2*time.Hour)) || l.Idle != 72*time.Hour ||
		!l.IdleExpiry().Equal(t0.Add(70*time.Hour)) || !l.Expiry().Equal(l.IdleExpiry()) {
		t.Errorf("current's activity: last %v idle %v expiry %v", l.LastActivity, l.Idle, l.Expiry())
	}
	if l := by["i-old"]; l.NotSpawns != "" || !strings.Contains(l.NotReused, "0.156.0") {
		t.Errorf("old: %+v", l)
	} else if !l.LastActivity.IsZero() || !l.Expiry().Equal(l.Proxy.Deadline) {
		t.Errorf("old has no activity tags: last %v, expiry %v (want its deadline %v)", l.LastActivity, l.Expiry(), l.Proxy.Deadline)
	}
	if l := by["i-forged"]; !strings.Contains(l.NotSpawns, "not exactly the proxy group") || l.NotReused == "" {
		t.Errorf("forged: %+v", l)
	}
}

// With no --vpc, every VPC with a tagged instance or tagged groups is listed.
func TestInventoryFindsEveryVPC(t *testing.T) {
	f := &fakeEC2{}
	if _, err := EnsureGroups(context.Background(), f, testVPC, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureGroups(context.Background(), f, "vpc-2", t0); err != nil {
		t.Fatal(err)
	}
	inv, err := Inventory(context.Background(), f, "", inventoryWant())
	if err != nil {
		t.Fatal(err)
	}
	if len(inv) != 2 || inv[0].VPCID != testVPC || inv[1].VPCID != "vpc-2" {
		t.Errorf("inventory VPCs %+v, want vpc-1 and vpc-2", inv)
	}
}

// Planning a delete reads and writes nothing: --dry-run is the plan.
func TestPlanDeleteWritesNothing(t *testing.T) {
	f := &fakeEC2{}
	g, _ := EnsureGroups(context.Background(), f, testVPC, t0)
	f.instances = []types.Instance{goodProxy("i-p", g.ProxyID), forged("i-forged", g.ProxyID)}
	f.calls = nil
	plan := PlanDelete(oneVPC(t, f))
	if w := writes(f); len(w) != 0 {
		t.Fatalf("planning wrote: %v", w)
	}
	if len(plan.Terminate) != 1 || plan.Terminate[0].InstanceID != "i-p" {
		t.Errorf("terminate %+v, want i-p", plan.Terminate)
	}
	if len(plan.Refused) != 1 || plan.Refused[0].InstanceID != "i-forged" {
		t.Errorf("refused %+v, want i-forged", plan.Refused)
	}
}

// Delete terminates spawn's proxy whatever its version or remaining life, and
// never an instance that fails the structural checks.
func TestDeleteRefusesAStructuralFailure(t *testing.T) {
	f := &fakeEC2{onDescribe: terminateOn}
	g, _ := EnsureGroups(context.Background(), f, testVPC, t0)
	stale := goodProxy("i-stale", g.ProxyID)
	setTag(&stale, "spawn:version", "0.150.0")
	setTag(&stale, "spawn:ttl-deadline", t0.Add(time.Hour).Format(time.RFC3339)) // under the reuse margin
	noProfile := goodProxy("i-noprofile", g.ProxyID)
	noProfile.IamInstanceProfile = nil
	late := goodProxy("i-late", g.ProxyID)
	setTag(&late, "spawn:ttl-deadline", t0.Add(30*24*time.Hour).Format(time.RFC3339))
	f.instances = []types.Instance{stale, forged("i-forged", g.ProxyID), noProfile, late}

	plan := PlanDelete(oneVPC(t, f))
	want := map[string]string{
		"i-forged":    "not exactly the proxy group",
		"i-noprofile": "instance profile",
		"i-late":      "later than launch plus",
	}
	if len(plan.Refused) != len(want) {
		t.Fatalf("refused %+v, want %v", plan.Refused, want)
	}
	for _, r := range plan.Refused {
		if !strings.Contains(r.Reason, want[r.InstanceID]) {
			t.Errorf("%s refused for %q, want %q", r.InstanceID, r.Reason, want[r.InstanceID])
		}
	}
	res, err := Delete(context.Background(), deps(f, &fakeProber{}, &fakeClock{now: t0}), plan, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.terminated, ",") != "i-stale" || strings.Join(res.Terminated, ",") != "i-stale" {
		t.Errorf("terminated %v, want only i-stale", f.terminated)
	}
	// The refused instances still carry the proxy group, so it is kept, and
	// the clients group after it.
	if res.Kept == nil || res.Kept.ID != g.ProxyID || len(f.deleted) != 0 {
		t.Errorf("kept %+v, deleted %v; want the proxy group kept for the refused instances", res.Kept, f.deleted)
	}
}

// Members still carry the clients group: the proxy and its group go, the
// clients group stays, and its users are named.
func TestDeleteKeepsAClientsGroupInUse(t *testing.T) {
	f := &fakeEC2{onDescribe: terminateOn}
	g, _ := EnsureGroups(context.Background(), f, testVPC, t0)
	f.instances = []types.Instance{goodProxy("i-p", g.ProxyID), member("i-m1", g.ClientsID), member("i-m2", g.ClientsID)}
	plan := PlanDelete(oneVPC(t, f))
	res, err := Delete(context.Background(), deps(f, &fakeProber{}, &fakeClock{now: t0}), plan, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Terminated, ",") != "i-p" {
		t.Errorf("terminated %v", res.Terminated)
	}
	if strings.Join(res.DeletedGroups, ",") != g.ProxyID {
		t.Errorf("deleted %v, want only the proxy group %s", res.DeletedGroups, g.ProxyID)
	}
	if res.Kept == nil || res.Kept.ID != g.ClientsID || strings.Join(res.Kept.Users, ",") != "i-m1,i-m2" {
		t.Errorf("kept %+v, want the clients group, used by i-m1 and i-m2", res.Kept)
	}
	if f.group(g.ClientsID) == nil {
		t.Error("the clients group is gone")
	}
}

// Nothing uses either group: both go, the proxy's first.
func TestDeleteRemovesEverything(t *testing.T) {
	f := &fakeEC2{onDescribe: terminateOn}
	g, _ := EnsureGroups(context.Background(), f, testVPC, t0)
	f.instances = []types.Instance{goodProxy("i-p", g.ProxyID)}
	res, err := Delete(context.Background(), deps(f, &fakeProber{}, &fakeClock{now: t0}), PlanDelete(oneVPC(t, f)), 0)
	if err != nil || res.Kept != nil {
		t.Fatalf("res %+v, err %v", res, err)
	}
	if strings.Join(res.DeletedGroups, ",") != g.ProxyID+","+g.ClientsID {
		t.Errorf("deleted %v, want the proxy group then the clients group", res.DeletedGroups)
	}
}

// A proxy that never reaches terminated fails the delete before any group is
// touched, after a bounded wait.
func TestDeleteWaitIsBounded(t *testing.T) {
	f := &fakeEC2{}
	g, _ := EnsureGroups(context.Background(), f, testVPC, t0)
	f.instances = []types.Instance{goodProxy("i-p", g.ProxyID)}
	clock := &fakeClock{now: t0}
	_, err := Delete(context.Background(), deps(f, &fakeProber{}, clock), PlanDelete(oneVPC(t, f)), time.Minute)
	if !errors.Is(err, ErrTerminateTimeout) {
		t.Fatalf("err = %v, want ErrTerminateTimeout", err)
	}
	if f.count("DeleteSecurityGroup") != 0 {
		t.Error("deleted a group before the proxy was gone")
	}
	if clock.sleeps == 0 || clock.now.Sub(t0) > 2*time.Minute {
		t.Errorf("waited %v in %d sleeps; want about the 1m bound", clock.now.Sub(t0), clock.sleeps)
	}
}

func upgradeRequest() Request {
	r := request()
	r.Version = "0.158.0"
	r.BaseTags = func(context.Context) ([]types.Tag, error) {
		return []types.Tag{tg("spawn:managed", "true"), tg("spawn:version", "0.158.0")}, nil
	}
	return r
}

func upgradeWant() Want {
	w := inventoryWant()
	w.Version = "0.158.0"
	return w
}

// A proxy at this version with life left is current: upgrade has nothing to do.
func TestPlanUpgradeAlreadyCurrent(t *testing.T) {
	f := &fakeEC2{}
	g, _ := EnsureGroups(context.Background(), f, testVPC, t0)
	cur := goodProxy("i-cur", g.ProxyID)
	setTag(&cur, "spawn:version", "0.158.0")
	f.instances = []types.Instance{cur}
	inv, _ := Inventory(context.Background(), f, testVPC, upgradeWant())
	plan := PlanUpgrade(inv[0])
	if plan.Current == nil || plan.Current.Proxy.InstanceID != "i-cur" || len(plan.Replace) != 0 {
		t.Errorf("plan %+v, want i-cur current and nothing to replace", plan)
	}
}

// An older proxy is replaced: a new one at this version, ready, then the old
// one terminated, keeping the idle period it published.
func TestUpgradeReplaces(t *testing.T) {
	f := &fakeEC2{onDescribe: heartbeatOn}
	g, _ := EnsureGroups(context.Background(), f, testVPC, t0)
	old := goodProxy("i-old", g.ProxyID)
	setTag(&old, egressproxy.IdleTag, "24h0m0s")
	f.instances = []types.Instance{old}
	inv, _ := Inventory(context.Background(), f, testVPC, upgradeWant())
	plan := PlanUpgrade(inv[0])
	if plan.Current != nil || len(plan.Replace) != 1 || plan.Idle() != 24*time.Hour {
		t.Fatalf("plan %+v (idle %v), want i-old replaced at its 24h idle", plan, plan.Idle())
	}
	r := upgradeRequest()
	r.Idle = plan.Idle()
	var idle time.Duration
	r.UserData = func(d time.Duration) (string, error) { idle = d; return "ud", nil }
	res, err := Upgrade(context.Background(), deps(f, &fakeProber{status: "Success"}, &fakeClock{now: t0}), r, plan, false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.New.Created || res.New.Version != "0.158.0" || f.count("RunInstances") != 1 {
		t.Errorf("new %+v, want one launched at 0.158.0", res.New)
	}
	if idle != 24*time.Hour {
		t.Errorf("new proxy's idle %v, want the old one's 24h", idle)
	}
	if strings.Join(res.Terminated, ",") != "i-old" || strings.Join(f.terminated, ",") != "i-old" {
		t.Errorf("terminated %v, want i-old", f.terminated)
	}
	// The old one is ended only after the new one is up.
	if i, j := lastIndex(f.calls, "RunInstances"), lastIndex(f.calls, "TerminateInstances"); j < i {
		t.Errorf("calls %v: terminated before launching", f.calls)
	}
}

// --keep-old: the new proxy is launched and the old one left to expire.
func TestUpgradeKeepOld(t *testing.T) {
	f := &fakeEC2{onDescribe: heartbeatOn}
	g, _ := EnsureGroups(context.Background(), f, testVPC, t0)
	f.instances = []types.Instance{goodProxy("i-old", g.ProxyID)}
	inv, _ := Inventory(context.Background(), f, testVPC, upgradeWant())
	res, err := Upgrade(context.Background(), deps(f, &fakeProber{status: "Success"}, &fakeClock{now: t0}),
		upgradeRequest(), PlanUpgrade(inv[0]), true)
	if err != nil {
		t.Fatal(err)
	}
	if !res.New.Created || len(f.terminated) != 0 || strings.Join(res.Kept, ",") != "i-old" {
		t.Errorf("res %+v, terminated %v; want a new proxy and i-old kept", res, f.terminated)
	}
}

// A replacement that never becomes ready terminates only itself: the old
// proxy keeps serving.
func TestUpgradeLeavesTheOldProxyWhenTheNewOneFails(t *testing.T) {
	f := &fakeEC2{}
	g, _ := EnsureGroups(context.Background(), f, testVPC, t0)
	f.instances = []types.Instance{goodProxy("i-old", g.ProxyID)}
	inv, _ := Inventory(context.Background(), f, testVPC, upgradeWant())
	r := upgradeRequest()
	r.ReadyTimeout = time.Minute
	_, err := Upgrade(context.Background(), deps(f, &fakeProber{status: "Success"}, &fakeClock{now: t0}), r, PlanUpgrade(inv[0]), false)
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("err = %v, want ErrNotReady", err)
	}
	for _, id := range f.terminated {
		if id == "i-old" {
			t.Error("the old proxy was terminated although its replacement failed")
		}
	}
}

func lastIndex(xs []string, s string) int {
	for i := len(xs) - 1; i >= 0; i-- {
		if xs[i] == s {
			return i
		}
	}
	return -1
}

// A nested spore cannot write IAM (#39), so its proxy launch names the spored
// profile instead of setting it up; anywhere else the setup runs as before.
func TestProxyProfile(t *testing.T) {
	setups := 0
	setup := func(context.Context) (string, error) { setups++; return "spored-instance-profile", nil }
	nested := func(context.Context) (string, error) {
		return "arn:aws:sts::123456789012:assumed-role/spawn-instance-nested-0123abcd/i-1", nil
	}
	if p, err := ProxyProfile(context.Background(), nested, setup); err != nil || p != "spored-instance-profile" || setups != 0 {
		t.Errorf("nested: %q, %v, %d setups; want the spored profile and no setup", p, err, setups)
	}
	user := func(context.Context) (string, error) { return "arn:aws:iam::123456789012:user/me", nil }
	failed := func(context.Context) (string, error) { return "", errors.New("no identity") }
	for _, caller := range []func(context.Context) (string, error){user, failed} {
		setups = 0
		if p, err := ProxyProfile(context.Background(), caller, setup); err != nil || p != "spored-instance-profile" || setups != 1 {
			t.Errorf("not nested: %q, %v, %d setups; want the setup", p, err, setups)
		}
	}
}

// pkg/aws keeps tag slots for the proxy's activity tags on a launch config of
// this Name (post_launch_tags.go isEgressProxy).
func TestEgressProxyLaunchName(t *testing.T) {
	if n := LaunchConfig("us-west-2", "0.158.0").Name; n != "spawn-egress-proxy" {
		t.Errorf("the proxy's launch config is named %q; pkg/aws isEgressProxy matches spawn-egress-proxy", n)
	}
	keys := strings.Join(spawnaws.PostLaunchTagKeys(), " ")
	for _, k := range []string{egressproxy.ActivityTag, egressproxy.IdleTag} {
		if !strings.Contains(keys, k) {
			t.Errorf("%s is not a post-launch tag key in pkg/aws", k)
		}
	}
}
