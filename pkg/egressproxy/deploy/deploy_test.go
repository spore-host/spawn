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

const (
	testVPC     = "vpc-1"
	testVersion = "0.157.0"
	testProfile = "spored-instance-profile"
)

var t0 = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

func tg(k, v string) types.Tag { return types.Tag{Key: aws.String(k), Value: aws.String(v)} }

// goodProxy is a proxy every check accepts, launched an hour before t0.
func goodProxy(id, proxySG string) types.Instance {
	launch := t0.Add(-time.Hour)
	return types.Instance{
		InstanceId:       aws.String(id),
		State:            &types.InstanceState{Name: types.InstanceStateNameRunning},
		PrivateIpAddress: aws.String("10.0.1.5"),
		SubnetId:         aws.String("subnet-pub"),
		VpcId:            aws.String(testVPC),
		LaunchTime:       aws.Time(launch),
		SecurityGroups:   []types.GroupIdentifier{{GroupId: aws.String(proxySG)}},
		IamInstanceProfile: &types.IamInstanceProfile{
			Arn: aws.String("arn:aws:iam::123456789012:instance-profile/" + testProfile)},
		Tags: []types.Tag{
			tg("spawn:managed", "true"), tg(ProxyTag, testVPC), tg("spawn:version", testVersion),
			tg("spawn:launch-time", launch.Format(time.RFC3339)),
			tg("spawn:ttl-deadline", launch.Add(egressproxy.MaxLife).Format(time.RFC3339)),
		},
	}
}

func want(proxySG string) Want {
	return Want{VPCID: testVPC, Version: testVersion, ProxyGroupID: proxySG, InstanceProfile: testProfile, Now: t0}
}

func setTag(inst *types.Instance, k, v string) {
	for i := range inst.Tags {
		if aws.ToString(inst.Tags[i].Key) == k {
			inst.Tags[i].Value = aws.String(v)
			return
		}
	}
	inst.Tags = append(inst.Tags, tg(k, v))
}

// The reuse decision: one good proxy is reused, and each way a proxy can be
// stale, foreign or forged is refused with a reason.
func TestEvaluate(t *testing.T) {
	if _, why := Evaluate(goodProxy("i-good", "sg-proxy"), want("sg-proxy")); why != "" {
		t.Fatalf("a good proxy was refused: %s", why)
	}
	for name, tc := range map[string]struct {
		edit func(*types.Instance)
		want string
	}{
		"another spawn's version": {func(i *types.Instance) { setTag(i, "spawn:version", "0.156.0") }, "launched by spawn 0.156.0"},
		"no version":              {func(i *types.Instance) { setTag(i, "spawn:version", "") }, "launched by spawn (none)"},
		"near its deadline": {func(i *types.Instance) {
			setTag(i, "spawn:ttl-deadline", t0.Add(egressproxy.ReuseMargin).Format(time.RFC3339))
		}, "not more than 12h"},
		"stopped":  {func(i *types.Instance) { i.State.Name = types.InstanceStateNameStopped }, "it is stopped"},
		"stopping": {func(i *types.Instance) { i.State.Name = types.InstanceStateNameStopping }, "it is stopping"},
		"another VPC's": {func(i *types.Instance) {
			setTag(i, ProxyTag, "vpc-2")
		}, "not vpc-1's proxy"},
		"not managed": {func(i *types.Instance) { setTag(i, "spawn:managed", "false") }, "not spawn:managed"},
		// Provenance, standing in for the signature #76 has not built: a spore
		// that tagged itself the proxy has its own groups and profile.
		"a member that tagged itself the proxy": {func(i *types.Instance) {
			i.SecurityGroups = []types.GroupIdentifier{{GroupId: aws.String("sg-default")}, {GroupId: aws.String("sg-clients")}}
		}, "not exactly the proxy group"},
		"the proxy group and another": {func(i *types.Instance) {
			i.SecurityGroups = append(i.SecurityGroups, types.GroupIdentifier{GroupId: aws.String("sg-other")})
		}, "not exactly the proxy group"},
		"another profile": {func(i *types.Instance) {
			i.IamInstanceProfile.Arn = aws.String("arn:aws:iam::123456789012:instance-profile/my-own")
		}, "instance profile"},
		"no profile": {func(i *types.Instance) { i.IamInstanceProfile = nil }, "instance profile is (none)"},
		"a forged far-future deadline": {func(i *types.Instance) {
			setTag(i, "spawn:ttl-deadline", "2099-01-01T00:00:00Z")
		}, "later than launch plus"},
		"a forged later launch time": {func(i *types.Instance) {
			// EC2's own LaunchTime still anchors it, so the deadline is too far.
			setTag(i, "spawn:launch-time", t0.Format(time.RFC3339))
			setTag(i, "spawn:ttl-deadline", t0.Add(egressproxy.MaxLife).Format(time.RFC3339))
		}, "later than launch plus"},
		"no deadline":   {func(i *types.Instance) { setTag(i, "spawn:ttl-deadline", "") }, "is not a time"},
		"no private IP": {func(i *types.Instance) { i.PrivateIpAddress = nil }, "no private IP"},
	} {
		inst := goodProxy("i-x", "sg-proxy")
		tc.edit(&inst)
		_, why := Evaluate(inst, want("sg-proxy"))
		if why == "" {
			t.Errorf("%s: reused", name)
			continue
		}
		if !strings.Contains(why, tc.want) {
			t.Errorf("%s: reason %q, want it to say %q", name, why, tc.want)
		}
	}
	// No proxy group to compare with: nothing is provably spawn's.
	if _, why := Evaluate(goodProxy("i-good", "sg-proxy"), want("")); why == "" {
		t.Error("reused with no proxy group to check against")
	}
}

// Find asks only for pending and running proxies of the VPC, and puts the one
// with the most life first.
func TestFindOrdersByDeadline(t *testing.T) {
	older := goodProxy("i-older", "sg-proxy")
	newer := goodProxy("i-newer", "sg-proxy")
	setTag(&newer, "spawn:launch-time", t0.Add(-time.Minute).Format(time.RFC3339))
	newer.LaunchTime = aws.Time(t0.Add(-time.Minute))
	setTag(&newer, "spawn:ttl-deadline", t0.Add(-time.Minute).Add(egressproxy.MaxLife).Format(time.RFC3339))
	stopped := goodProxy("i-stopped", "sg-proxy")
	stopped.State.Name = types.InstanceStateNameStopped
	f := &fakeEC2{instances: []types.Instance{older, newer, stopped}}
	got, rejected, err := Find(context.Background(), f, want("sg-proxy"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].InstanceID != "i-newer" || got[1].InstanceID != "i-older" {
		t.Errorf("reusable = %+v, want i-newer then i-older", got)
	}
	if len(rejected) != 0 {
		t.Errorf("rejected = %+v; the stopped one should not even be listed", rejected)
	}
}

func proxySpec() Spec {
	return Spec{
		Region: "us-west-2", VPCID: testVPC, AvailabilityZone: "us-west-2b",
		PublicSubnets: []spawnaws.PublicSubnet{{SubnetID: "subnet-a", AvailabilityZone: "us-west-2a"},
			{SubnetID: "subnet-b", AvailabilityZone: "us-west-2b"}},
		Groups: Groups{ClientsID: "sg-clients", ProxyID: "sg-proxy"}, AMI: "ami-al2023arm", InstanceProfile: testProfile,
		UserData: "dXNlcmRhdGE=",
		BaseTags: []types.Tag{tg("spawn:managed", "true"), tg("spawn:version", testVersion),
			tg("spawn:launch-time", t0.Format(time.RFC3339)), tg("spawn:ttl", "1h"), tg("Name", "spawn-egress-proxy")},
	}
}

// The RunInstances request, field by field.
func TestRunInput(t *testing.T) {
	in, deadline, err := RunInput(proxySpec(), t0)
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(in.ImageId) != "ami-al2023arm" || in.InstanceType != "t4g.micro" ||
		aws.ToInt32(in.MinCount) != 1 || aws.ToInt32(in.MaxCount) != 1 {
		t.Errorf("image %s type %s count %d-%d", aws.ToString(in.ImageId), in.InstanceType, aws.ToInt32(in.MinCount), aws.ToInt32(in.MaxCount))
	}
	if in.IamInstanceProfile == nil || aws.ToString(in.IamInstanceProfile.Name) != testProfile {
		t.Errorf("profile %+v", in.IamInstanceProfile)
	}
	if len(in.NetworkInterfaces) != 1 {
		t.Fatalf("%d interfaces", len(in.NetworkInterfaces))
	}
	ni := in.NetworkInterfaces[0]
	if !aws.ToBool(ni.AssociatePublicIpAddress) {
		t.Error("no public IP requested: the proxy has no other way out")
	}
	if aws.ToString(ni.SubnetId) != "subnet-b" {
		t.Errorf("subnet %s, want subnet-b, the public subnet in the launch's AZ", aws.ToString(ni.SubnetId))
	}
	if len(ni.Groups) != 1 || ni.Groups[0] != "sg-proxy" {
		t.Errorf("groups %v, want only the proxy group", ni.Groups)
	}
	mo := in.MetadataOptions
	if mo == nil || mo.HttpTokens != types.HttpTokensStateRequired || aws.ToInt32(mo.HttpPutResponseHopLimit) != 1 {
		t.Errorf("metadata options %+v, want IMDSv2 required, hop limit 1", mo)
	}
	if in.InstanceInitiatedShutdownBehavior != types.ShutdownBehaviorTerminate {
		t.Errorf("shutdown behaviour %q, want terminate", in.InstanceInitiatedShutdownBehavior)
	}
	if aws.ToString(in.UserData) != "dXNlcmRhdGE=" {
		t.Errorf("user data %q", aws.ToString(in.UserData))
	}
	if !deadline.Equal(t0.Add(egressproxy.MaxLife)) {
		t.Errorf("deadline %v, want launch + max life %v", deadline, t0.Add(egressproxy.MaxLife))
	}
	if len(in.TagSpecifications) != 2 || in.TagSpecifications[1].ResourceType != types.ResourceTypeVolume {
		t.Fatalf("tag specifications %+v, want the instance's and the volume's", in.TagSpecifications)
	}
	tags := tagMap(in.TagSpecifications[0].Tags)
	for k, v := range map[string]string{
		ProxyTag: testVPC, "Name": "spawn-egress-proxy-vpc-1", "spawn:managed": "true", "spawn:version": testVersion,
		"spawn:launch-time": t0.Format(time.RFC3339), "spawn:ttl": "144h0m0s",
		"spawn:ttl-deadline": t0.Add(egressproxy.MaxLife).Format(time.RFC3339),
	} {
		if tags[k] != v {
			t.Errorf("tag %s = %q, want %q", k, tags[k], v)
		}
	}
	if n := len(in.TagSpecifications[0].Tags); n != len(tags) {
		t.Errorf("%d tags for %d keys: a key is written twice", n, len(tags))
	}
	if egressproxy.MaxLife != 6*24*time.Hour {
		t.Errorf("MaxLife %v, want 6 days (the 7-day reaper ceiling less a day)", egressproxy.MaxLife)
	}
}

func TestPickSubnet(t *testing.T) {
	publics := proxySpec().PublicSubnets
	if s, _ := PickSubnet(publics, "us-west-2b"); s.SubnetID != "subnet-b" {
		t.Errorf("in us-west-2b: %s", s.SubnetID)
	}
	if s, _ := PickSubnet(publics, "us-west-2c"); s.SubnetID != "subnet-a" {
		t.Errorf("no public subnet in us-west-2c: %s, want the first", s.SubnetID)
	}
	if _, err := PickSubnet(nil, "us-west-2a"); !errors.Is(err, ErrNoPublicSubnet) {
		t.Errorf("no public subnet: %v", err)
	}
}

func rulesFor(perms []types.IpPermission) string { return rulesString(rulesOf(perms)) }

// The two groups, created in an empty VPC, carry exactly their rules: EC2's
// default allow-all egress is removed from both.
func TestEnsureGroupsRulesAreExact(t *testing.T) {
	f := &fakeEC2{}
	g, err := EnsureGroups(context.Background(), f, testVPC, t0)
	if err != nil {
		t.Fatal(err)
	}
	clients, proxy := f.group(g.ClientsID), f.group(g.ProxyID)
	if clients == nil || proxy == nil {
		t.Fatalf("groups %+v not created", g)
	}
	if aws.ToString(clients.GroupName) != "spawn-egress-clients-vpc-1" || aws.ToString(proxy.GroupName) != "spawn-egress-proxy-vpc-1" {
		t.Errorf("names %s, %s", aws.ToString(clients.GroupName), aws.ToString(proxy.GroupName))
	}
	if got := rulesFor(clients.IpPermissions) + "|" + rulesFor(clients.IpPermissionsEgress); got != "|" {
		t.Errorf("clients group rules %q, want none at all", got)
	}
	if got, want := rulesFor(proxy.IpPermissions), "tcp 3128 "+g.ClientsID; got != want {
		t.Errorf("proxy ingress %q, want %q", got, want)
	}
	if got, want := rulesFor(proxy.IpPermissionsEgress), "tcp 443 0.0.0.0/0; tcp 80 0.0.0.0/0"; got != want {
		t.Errorf("proxy egress %q, want %q", got, want)
	}
	for _, sg := range []*types.SecurityGroup{clients, proxy} {
		tags := tagMap(sg.Tags)
		if tags["spawn:managed"] != "true" || tags[spawnaws.CreatedTagKey] == "" || tags[ProxyTag] != testVPC {
			t.Errorf("%s tags %v: the reaper could not age it, or spawn could not find it", aws.ToString(sg.GroupName), tags)
		}
	}

	// Again, on the groups as they are now: nothing to change.
	before := len(f.calls)
	if _, err := EnsureGroups(context.Background(), f, testVPC, t0); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls[before:] {
		if c != "DescribeSecurityGroups" {
			t.Errorf("second EnsureGroups called %s on groups already exact", c)
		}
	}
}

// A rule added to the proxy group by hand is removed: the proxy is reachable
// from its clients group and nothing else.
func TestEnsureGroupsRemovesDrift(t *testing.T) {
	f := &fakeEC2{}
	g, err := EnsureGroups(context.Background(), f, testVPC, t0)
	if err != nil {
		t.Fatal(err)
	}
	proxy := f.group(g.ProxyID)
	proxy.IpPermissions = append(proxy.IpPermissions, rule{proto: "tcp", from: 3128, to: 3128, cidr: "0.0.0.0/0"}.permission(""))
	proxy.IpPermissionsEgress = append(proxy.IpPermissionsEgress, rule{proto: "-1", cidr: "0.0.0.0/0"}.permission(""))
	if _, err := EnsureGroups(context.Background(), f, testVPC, t0); err != nil {
		t.Fatal(err)
	}
	if got := rulesFor(proxy.IpPermissions); got != "tcp 3128 "+g.ClientsID {
		t.Errorf("proxy ingress after drift %q", got)
	}
	if got := rulesFor(proxy.IpPermissionsEgress); got != "tcp 443 0.0.0.0/0; tcp 80 0.0.0.0/0" {
		t.Errorf("proxy egress after drift %q", got)
	}
}

// A group of spawn's name without spawn's tags is not adopted.
func TestEnsureGroupsRefusesAForeignGroup(t *testing.T) {
	f := &fakeEC2{groups: []types.SecurityGroup{{GroupId: aws.String("sg-theirs"),
		GroupName: aws.String(ClientsGroupName(testVPC)), VpcId: aws.String(testVPC)}}}
	if _, err := EnsureGroups(context.Background(), f, testVPC, t0); !errors.Is(err, ErrForeignGroup) {
		t.Errorf("err = %v, want ErrForeignGroup", err)
	}
	if f.count("AuthorizeSecurityGroupIngress")+f.count("RevokeSecurityGroupEgress") != 0 {
		t.Error("a foreign group's rules were touched")
	}
}

// The deleter: refused while any instance uses a group; otherwise the proxy
// group first (its rule references the clients group), then the clients one.
func TestDeleteGroups(t *testing.T) {
	f := &fakeEC2{}
	g, err := EnsureGroups(context.Background(), f, testVPC, t0)
	if err != nil {
		t.Fatal(err)
	}
	f.instances = []types.Instance{goodProxy("i-live", g.ProxyID)}
	if _, err := DeleteGroups(context.Background(), f, testVPC); !errors.Is(err, ErrGroupsInUse) {
		t.Fatalf("in use: err = %v, want ErrGroupsInUse", err)
	}
	if len(f.deleted) != 0 {
		t.Fatalf("deleted %v while in use", f.deleted)
	}
	f.instances[0].State.Name = types.InstanceStateNameTerminated
	deleted, err := DeleteGroups(context.Background(), f, testVPC)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 2 || deleted[0] != g.ProxyID || deleted[1] != g.ClientsID {
		t.Errorf("deleted %v, want the proxy group then the clients group", deleted)
	}
	// Nothing left: not an error.
	if deleted, err := DeleteGroups(context.Background(), f, testVPC); err != nil || len(deleted) != 0 {
		t.Errorf("second delete: %v, %v", deleted, err)
	}
}

// A member's groups gain the clients group; a member that named none keeps
// the VPC default group EC2 would have given it.
func TestMemberGroups(t *testing.T) {
	f := &fakeEC2{groups: []types.SecurityGroup{{GroupId: aws.String("sg-default"), GroupName: aws.String("default"), VpcId: aws.String(testVPC)}}}
	g := Groups{ClientsID: "sg-clients", ProxyID: "sg-proxy"}
	got, err := MemberGroups(context.Background(), f, testVPC, nil, g)
	if err != nil || strings.Join(got, ",") != "sg-default,sg-clients" {
		t.Errorf("no own groups: %v, %v", got, err)
	}
	got, _ = MemberGroups(context.Background(), f, testVPC, []string{"sg-mpi"}, g)
	if strings.Join(got, ",") != "sg-mpi,sg-clients" {
		t.Errorf("own groups: %v", got)
	}
	got, _ = MemberGroups(context.Background(), f, testVPC, []string{"sg-mpi", "sg-clients"}, g)
	if strings.Join(got, ",") != "sg-mpi,sg-clients" {
		t.Errorf("already has it: %v", got)
	}
}

func heartbeatOn(i *types.Instance, _ int) {
	i.State = &types.InstanceState{Name: types.InstanceStateNameRunning}
	setTag(i, spawnaws.HeartbeatTag, t0.Format(time.RFC3339))
}

func request() Request {
	return Request{
		Region: "us-west-2", VPCID: testVPC, AvailabilityZone: "us-west-2a",
		PublicSubnets: []spawnaws.PublicSubnet{{SubnetID: "subnet-pub", AvailabilityZone: "us-west-2a"}},
		Version:       testVersion, Idle: egressproxy.DefaultIdle, InstanceProfile: testProfile,
		AMI:      func(context.Context) (string, error) { return "ami-1", nil },
		UserData: func(time.Duration) (string, error) { return "ud", nil },
		BaseTags: func(context.Context) ([]types.Tag, error) {
			return []types.Tag{tg("spawn:managed", "true"), tg("spawn:version", testVersion)}, nil
		},
	}
}

func deps(f *fakeEC2, p *fakeProber, c *fakeClock) Deps {
	return Deps{EC2: f, Prober: p, Now: c.Now, Sleep: c.Sleep}
}

// A ready proxy of this version is reused: nothing is launched.
func TestFindOrCreateReuses(t *testing.T) {
	f := &fakeEC2{}
	g, _ := EnsureGroups(context.Background(), f, testVPC, t0)
	live := goodProxy("i-live", g.ProxyID)
	setTag(&live, spawnaws.HeartbeatTag, t0.Format(time.RFC3339))
	f.instances = []types.Instance{live}
	p := &fakeProber{status: "Success"}
	res, err := FindOrCreate(context.Background(), deps(f, p, &fakeClock{now: t0}), request())
	if err != nil {
		t.Fatal(err)
	}
	if res.Proxy.InstanceID != "i-live" || res.Proxy.Created || f.count("RunInstances") != 0 {
		t.Errorf("proxy %+v, %d RunInstances; want i-live reused", res.Proxy, f.count("RunInstances"))
	}
	if res.Proxy.Addr() != "10.0.1.5:3128" {
		t.Errorf("addr %s", res.Proxy.Addr())
	}
	if len(p.commands) != 1 || p.commands[0] != ProbeCommand("10.0.1.5") {
		t.Errorf("probe %q, want the port probe of the reused proxy", p.commands)
	}
}

// An older spawn's proxy is replaced, not terminated: another launch's
// members may be using it, and it ends on its own.
func TestFindOrCreateReplacesAnOldVersionWithoutTerminatingIt(t *testing.T) {
	f := &fakeEC2{onDescribe: heartbeatOn}
	g, _ := EnsureGroups(context.Background(), f, testVPC, t0)
	old := goodProxy("i-old", g.ProxyID)
	setTag(&old, "spawn:version", "0.156.0")
	f.instances = []types.Instance{old}
	res, err := FindOrCreate(context.Background(), deps(f, &fakeProber{status: "Success"}, &fakeClock{now: t0}), request())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Proxy.Created || f.count("RunInstances") != 1 {
		t.Errorf("proxy %+v, want a new one", res.Proxy)
	}
	if len(f.terminated) != 0 {
		t.Errorf("terminated %v: the superseded proxy must keep serving until it expires", f.terminated)
	}
	if len(res.Rejected) != 1 || !strings.Contains(res.Rejected[0].Reason, "0.156.0") {
		t.Errorf("rejected %+v", res.Rejected)
	}
	in := f.runInputs[0]
	if g := in.NetworkInterfaces[0].Groups; len(g) != 1 || g[0] != res.Groups.ProxyID {
		t.Errorf("new proxy's groups %v, want only %s", g, res.Groups.ProxyID)
	}
}

// A proxy this call created that never becomes ready is terminated, and the
// launch fails.
func TestFindOrCreateTerminatesAProxyThatIsNotReady(t *testing.T) {
	f := &fakeEC2{onDescribe: heartbeatOn}
	clock := &fakeClock{now: t0}
	p := &fakeProber{status: "Failed"}
	_, err := FindOrCreate(context.Background(), deps(f, p, clock), request())
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("err = %v, want ErrNotReady", err)
	}
	if len(f.terminated) != 1 || f.terminated[0] != aws.ToString(f.instances[0].InstanceId) {
		t.Errorf("terminated %v, want the proxy just created", f.terminated)
	}
	if clock.now.Sub(t0) > ReadyTimeout+readyPoll {
		t.Errorf("waited %v, past the %v bound", clock.now.Sub(t0), ReadyTimeout)
	}
	if !strings.Contains(err.Error(), "terminated it") {
		t.Errorf("err = %v, want it to say the proxy was terminated", err)
	}
}

// A found proxy that does not answer is not terminated (it may be another
// launch's, still starting); a new one is created.
func TestFindOrCreateLeavesAnUnreadyFoundProxy(t *testing.T) {
	f := &fakeEC2{}
	g, _ := EnsureGroups(context.Background(), f, testVPC, t0)
	f.instances = []types.Instance{goodProxy("i-starting", g.ProxyID)} // no heartbeat, ever
	f.onDescribe = func(i *types.Instance, n int) {
		if aws.ToString(i.InstanceId) != "i-starting" {
			heartbeatOn(i, n)
		}
	}
	res, err := FindOrCreate(context.Background(), deps(f, &fakeProber{status: "Success"}, &fakeClock{now: t0}), request())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Proxy.Created || len(f.terminated) != 0 {
		t.Errorf("proxy %+v, terminated %v; want a new proxy and the found one left alone", res.Proxy, f.terminated)
	}
}

// Readiness needs both spored's heartbeat and the port probe; denied SSM is a
// failure, not a pass.
func TestWaitReady(t *testing.T) {
	inst := goodProxy("i-1", "sg-proxy")
	inst.State.Name = types.InstanceStateNamePending
	f := &fakeEC2{instances: []types.Instance{inst}, onDescribe: func(i *types.Instance, n int) {
		if n >= 3 {
			heartbeatOn(i, n)
		}
	}}
	p := &fakeProber{status: "Success"}
	clock := &fakeClock{now: t0}
	if _, err := WaitReady(context.Background(), deps(f, p, clock), "us-west-2", Proxy{InstanceID: "i-1"}, 0); err != nil {
		t.Fatal(err)
	}
	if len(p.commands) != 1 {
		t.Errorf("%d probes; the probe must wait for the heartbeat", len(p.commands))
	}

	denied := &fakeProber{err: accessDenied}
	if _, err := WaitReady(context.Background(), deps(f, denied, &fakeClock{now: t0}), "us-west-2", Proxy{InstanceID: "i-1"}, 0); !errors.Is(err, ErrNotReady) || !strings.Contains(err.Error(), "cannot check its port") {
		t.Errorf("SSM denied: err = %v", err)
	}

	gone := &fakeEC2{instances: []types.Instance{inst}, onDescribe: func(i *types.Instance, _ int) {
		i.State = &types.InstanceState{Name: types.InstanceStateNameTerminated}
	}}
	clock = &fakeClock{now: t0}
	if _, err := WaitReady(context.Background(), deps(gone, p, clock), "us-west-2", Proxy{InstanceID: "i-1"}, 0); !errors.Is(err, ErrNotReady) || clock.sleeps != 0 {
		t.Errorf("terminated: err = %v after %d sleeps, want an immediate failure", err, clock.sleeps)
	}
}

// An idle period above the proxy's life is refused before any AWS call.
func TestFindOrCreateRefusesAnIdleAboveMaxLife(t *testing.T) {
	f := &fakeEC2{}
	r := request()
	r.Idle = egressproxy.MaxLife + time.Hour
	if _, err := FindOrCreate(context.Background(), deps(f, &fakeProber{}, &fakeClock{now: t0}), r); err == nil {
		t.Fatal("accepted")
	}
	if len(f.calls) != 0 {
		t.Errorf("calls %v before refusing", f.calls)
	}
}

// RunInstances is retried through EC2's instance-profile propagation lag, and
// only through that.
func TestCreateRetriesProfilePropagation(t *testing.T) {
	f := &fakeEC2{runErrs: []error{errors.New("api error InvalidParameterValue: Invalid IAM Instance Profile name")}}
	clock := &fakeClock{now: t0}
	if _, err := Create(context.Background(), deps(f, &fakeProber{}, clock), proxySpec()); err != nil {
		t.Fatal(err)
	}
	if f.count("RunInstances") != 2 || clock.sleeps != 1 {
		t.Errorf("%d RunInstances, %d sleeps", f.count("RunInstances"), clock.sleeps)
	}
	f = &fakeEC2{runErrs: []error{errors.New("api error InvalidParameterValue: bad AMI")}}
	if _, err := Create(context.Background(), deps(f, &fakeProber{}, &fakeClock{now: t0}), proxySpec()); err == nil || f.count("RunInstances") != 1 {
		t.Errorf("another InvalidParameterValue was retried (%d calls): %v", f.count("RunInstances"), err)
	}
}

// The unit runs `spored egress-proxy` on the proxy port with the idle period,
// in place of the bare daemon.
func TestUnitScript(t *testing.T) {
	s := UnitScript(72 * time.Hour)
	for _, want := range []string{
		"ExecStart=\n",
		"ExecStart=/usr/local/bin/spored egress-proxy --port 3128 --idle 72h0m0s\n",
		"systemctl daemon-reload", "systemctl restart spored", UnitDropIn,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("unit script lacks %q:\n%s", want, s)
		}
	}
}

// What the readiness probe fetches must be a host the proxy forwards to, or
// no proxy would ever be ready.
func TestProbeHostIsAllowlisted(t *testing.T) {
	if !egressproxy.Allowed(ProbeHost) {
		t.Errorf("%s is not on egressproxy.Allowlist", ProbeHost)
	}
	if !strings.Contains(ProbeCommand("10.0.0.7"), "--proxy http://10.0.0.7:3128 https://"+ProbeHost+"/") {
		t.Errorf("probe %q", ProbeCommand("10.0.0.7"))
	}
}
