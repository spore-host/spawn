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
	"github.com/spore-host/spawn/pkg/egressproxy/sweepproxy"
	"github.com/spore-host/spawn/pkg/launcher"
)

// sweepTarget is a detached sweep's target as the CLI records it, its proxy
// i-live ready at submit time.
func sweepTarget(g Groups) *SweepTarget {
	return &SweepTarget{
		Region: "us-west-2", VPCID: testVPC, AvailabilityZone: "us-west-2a",
		PublicSubnets: []sweepproxy.Subnet{{SubnetID: "subnet-pub", AvailabilityZone: "us-west-2a"}},
		Idle:          egressproxy.DefaultIdle, Version: testVersion, InstanceProfile: testProfile,
		AMI: "ami-1", UserData: "proxy-ud",
		BaseTags:     map[string]string{"spawn:managed": "true", "spawn:version": testVersion},
		RenderedAddr: "10.0.1.5:3128", RenderedClientsGroupID: g.ClientsID,
		Phase: PhaseReady, ProxyID: "i-live", ProxyAddr: "10.0.1.5:3128",
		ClientsGroupID: g.ClientsID, ProxyGroupID: g.ProxyID,
	}
}

// sweepFixture is a VPC with its groups and, when live, the ready proxy
// i-live.
func sweepFixture(t *testing.T, live bool) (*fakeEC2, *SweepTarget) {
	t.Helper()
	f := &fakeEC2{}
	g, err := EnsureGroups(context.Background(), f, testVPC, t0)
	if err != nil {
		t.Fatal(err)
	}
	if live {
		inst := goodProxy("i-live", g.ProxyID)
		setTag(&inst, spawnaws.HeartbeatTag, t0.Format(time.RFC3339))
		f.instances = []types.Instance{inst}
	}
	f.calls = nil
	return f, sweepTarget(g)
}

// The recorded proxy, still reusable, is used as it is: no launch, no probe,
// no wait.
func TestEnsureSweepReusesTheRecordedProxy(t *testing.T) {
	f, tgt := sweepFixture(t, true)
	p := &fakeProber{status: "Success"}
	clock := &fakeClock{now: t0}
	step := EnsureSweep(context.Background(), deps(f, p, clock), tgt)
	if step.Outcome != SweepLaunch || step.Changed {
		t.Fatalf("step %+v, want launch with nothing changed", step)
	}
	if f.count("RunInstances") != 0 || len(p.commands) != 0 || clock.sleeps != 0 {
		t.Errorf("%d RunInstances, %d probes, %d sleeps; want none", f.count("RunInstances"), len(p.commands), clock.sleeps)
	}
	if tgt.Current() != tgt.Rendered() {
		t.Errorf("binding %+v, want the rendered %+v", tgt.Current(), tgt.Rendered())
	}
}

// The proxy is gone mid-sweep: the orchestrator launches a new one from the
// recorded inputs and holds the VPC's members, one bounded look per call,
// until it is ready; then they launch bound to its address.
func TestEnsureSweepCreatesPollsAndLaunches(t *testing.T) {
	f, tgt := sweepFixture(t, false)
	p := &fakeProber{status: "Success"}
	clock := &fakeClock{now: t0}
	d := deps(f, p, clock)

	step := EnsureSweep(context.Background(), d, tgt)
	if step.Outcome != SweepHold || !step.Changed || tgt.Phase != PhaseCreating {
		t.Fatalf("step %+v, phase %s; want hold, creating", step, tgt.Phase)
	}
	if f.count("RunInstances") != 1 {
		t.Fatalf("%d RunInstances, want 1", f.count("RunInstances"))
	}
	in := f.runInputs[0]
	if aws.ToString(in.ImageId) != "ami-1" || aws.ToString(in.UserData) != "proxy-ud" ||
		aws.ToString(in.IamInstanceProfile.Name) != testProfile || in.NetworkInterfaces[0].Groups[0] != tgt.ProxyGroupID {
		t.Errorf("RunInstances %+v: not the recorded proxy", in)
	}
	tags := tagMap(in.TagSpecifications[0].Tags)
	if tags["spawn:version"] != testVersion || tags[ProxyTag] != testVPC || tags["spawn:launch-time"] != t0.Format(time.RFC3339) {
		t.Errorf("tags %v: want the recorded version, the proxy tag, and the launch's own launch time", tags)
	}
	created := tgt.ProxyID
	if tgt.CreatingSince != t0 || len(tgt.Created) != 1 || tgt.Created[0] != created {
		t.Errorf("target %+v: the launch is not recorded", tgt)
	}
	if got := tgt.CreatingProxies(); len(got) != 1 || got[0] != created {
		t.Errorf("CreatingProxies %v, want %s", got, created)
	}

	// Booting: no heartbeat yet. Held, without a probe or a sleep.
	clock.now = t0.Add(time.Minute)
	calls := len(f.calls)
	if step := EnsureSweep(context.Background(), d, tgt); step.Outcome != SweepHold || !strings.Contains(step.Reason, "has not written") {
		t.Fatalf("step %+v, want hold on the heartbeat", step)
	}
	if len(f.calls)-calls != 1 || len(p.commands) != 0 || clock.sleeps != 0 {
		t.Errorf("%d calls, %d probes, %d sleeps for one look; want one DescribeInstances", len(f.calls)-calls, len(p.commands), clock.sleeps)
	}

	// spored is up and the port answers: ready.
	f.onDescribe = heartbeatOn
	clock.now = t0.Add(2 * time.Minute)
	step = EnsureSweep(context.Background(), d, tgt)
	if step.Outcome != SweepLaunch || !step.Changed || tgt.Phase != PhaseReady {
		t.Fatalf("step %+v, phase %s; want launch, ready", step, tgt.Phase)
	}
	if tgt.ProxyAddr != "10.0.9.9:3128" || tgt.Current().Addr == tgt.Rendered().Addr {
		t.Errorf("addr %s: the members must be rebound to the new proxy", tgt.ProxyAddr)
	}
	if len(p.commands) != 1 || clock.sleeps != 0 {
		t.Errorf("%d probes, %d sleeps", len(p.commands), clock.sleeps)
	}
	if len(tgt.CreatingProxies()) != 0 {
		t.Error("a ready proxy is still offered to a cancel")
	}

	// And it is reused from then on.
	if step := EnsureSweep(context.Background(), d, tgt); step.Outcome != SweepLaunch || f.count("RunInstances") != 1 {
		t.Errorf("step %+v after ready, %d RunInstances", step, f.count("RunInstances"))
	}
}

// A proxy that is not ready within the budget is terminated and the target
// fails, for good: its members are failed with the reason, not held.
func TestEnsureSweepTimesOutTerminatesAndFails(t *testing.T) {
	f, tgt := sweepFixture(t, false)
	p := &fakeProber{status: "Failed"}
	clock := &fakeClock{now: t0}
	d := deps(f, p, clock)
	if step := EnsureSweep(context.Background(), d, tgt); step.Outcome != SweepHold {
		t.Fatalf("step %+v", step)
	}
	created := tgt.ProxyID
	f.onDescribe = heartbeatOn // up, but the port never answers

	clock.now = t0.Add(ReadyTimeout - time.Second)
	if step := EnsureSweep(context.Background(), d, tgt); step.Outcome != SweepHold {
		t.Fatalf("before the budget: step %+v, want hold", step)
	}
	clock.now = t0.Add(ReadyTimeout)
	step := EnsureSweep(context.Background(), d, tgt)
	if step.Outcome != SweepFail || tgt.Phase != PhaseFailed {
		t.Fatalf("step %+v, phase %s; want fail", step, tgt.Phase)
	}
	if len(f.terminated) != 1 || f.terminated[0] != created {
		t.Errorf("terminated %v, want %s", f.terminated, created)
	}
	for _, want := range []string{"did not become ready", created, "terminated it", testVPC} {
		if !strings.Contains(tgt.FailReason, want) {
			t.Errorf("reason %q does not say %q", tgt.FailReason, want)
		}
	}
	if clock.sleeps != 0 {
		t.Errorf("%d sleeps: the orchestrator's poll must not wait", clock.sleeps)
	}
	calls := len(f.calls)
	if step := EnsureSweep(context.Background(), d, tgt); step.Outcome != SweepFail || step.Reason != tgt.FailReason || len(f.calls) != calls {
		t.Errorf("after failing: step %+v, %d more calls; want the same failure and no AWS", step, len(f.calls)-calls)
	}
}

// A proxy that dies while it is being created fails the target at once.
func TestEnsureSweepCreatedProxyTerminatedFails(t *testing.T) {
	f, tgt := sweepFixture(t, false)
	d := deps(f, &fakeProber{status: "Success"}, &fakeClock{now: t0})
	EnsureSweep(context.Background(), d, tgt)
	f.onDescribe = func(i *types.Instance, _ int) {
		i.State = &types.InstanceState{Name: types.InstanceStateNameTerminated}
	}
	if step := EnsureSweep(context.Background(), d, tgt); step.Outcome != SweepFail || !strings.Contains(tgt.FailReason, "is terminated") {
		t.Errorf("step %+v, reason %q; want an immediate failure naming the state", step, tgt.FailReason)
	}
}

// A recorded proxy that is no longer reusable (near its deadline, another
// version) is replaced, never terminated: other launches may use it.
func TestEnsureSweepReplacesAStaleProxyWithoutTerminatingIt(t *testing.T) {
	for name, edit := range map[string]func(*types.Instance){
		"near its deadline": func(i *types.Instance) {
			setTag(i, "spawn:ttl-deadline", t0.Add(egressproxy.ReuseMargin).Format(time.RFC3339))
		},
		"another version": func(i *types.Instance) { setTag(i, "spawn:version", "0.1.0") },
	} {
		t.Run(name, func(t *testing.T) {
			f, tgt := sweepFixture(t, true)
			edit(&f.instances[0])
			step := EnsureSweep(context.Background(), deps(f, &fakeProber{status: "Success"}, &fakeClock{now: t0}), tgt)
			if step.Outcome != SweepHold || tgt.Phase != PhaseCreating || f.count("RunInstances") != 1 {
				t.Fatalf("step %+v, phase %s, %d RunInstances; want a new proxy", step, tgt.Phase, f.count("RunInstances"))
			}
			if len(f.terminated) != 0 {
				t.Errorf("terminated %v", f.terminated)
			}
			if !strings.Contains(step.Reason, "i-live is no longer reusable") {
				t.Errorf("reason %q does not say why i-live was not used", step.Reason)
			}
		})
	}
}

// Another reusable proxy (a foreground launch's, say) is used if one look
// finds it ready; one that is not is left alone and a new one launched.
func TestEnsureSweepUsesAnotherReadyProxy(t *testing.T) {
	f, tgt := sweepFixture(t, true)
	f.instances[0].InstanceId = aws.String("i-other")
	p := &fakeProber{status: "Success"}
	step := EnsureSweep(context.Background(), deps(f, p, &fakeClock{now: t0}), tgt)
	if step.Outcome != SweepLaunch || tgt.ProxyID != "i-other" || f.count("RunInstances") != 0 || len(p.commands) != 1 {
		t.Fatalf("step %+v, proxy %s, %d RunInstances, %d probes; want i-other after one probe",
			step, tgt.ProxyID, f.count("RunInstances"), len(p.commands))
	}

	f, tgt = sweepFixture(t, true)
	f.instances[0].InstanceId = aws.String("i-starting")
	setTag(&f.instances[0], spawnaws.HeartbeatTag, "")
	step = EnsureSweep(context.Background(), deps(f, &fakeProber{status: "Success"}, &fakeClock{now: t0}), tgt)
	if step.Outcome != SweepHold || f.count("RunInstances") != 1 || len(f.terminated) != 0 {
		t.Errorf("step %+v, %d RunInstances, terminated %v; want a new proxy and i-starting left alone",
			step, f.count("RunInstances"), f.terminated)
	}
}

// AWS errors before a launch hold the members and are retried, up to the
// budget; an authorization error fails at once.
func TestEnsureSweepAWSErrors(t *testing.T) {
	f, tgt := sweepFixture(t, false)
	f.runErrs = []error{errors.New("api error RequestLimitExceeded"), errors.New("api error RequestLimitExceeded")}
	clock := &fakeClock{now: t0}
	d := deps(f, &fakeProber{status: "Success"}, clock)
	if step := EnsureSweep(context.Background(), d, tgt); step.Outcome != SweepHold || tgt.ErrorSince != t0 {
		t.Fatalf("step %+v, error since %v; want hold", step, tgt.ErrorSince)
	}
	clock.now = t0.Add(ReadyTimeout)
	if step := EnsureSweep(context.Background(), d, tgt); step.Outcome != SweepFail || !strings.Contains(tgt.FailReason, "RequestLimitExceeded") {
		t.Fatalf("step %+v, reason %q; want fail after the budget", step, tgt.FailReason)
	}

	f, tgt = sweepFixture(t, false)
	f.runErrs = []error{accessDenied}
	if step := EnsureSweep(context.Background(), deps(f, &fakeProber{}, &fakeClock{now: t0}), tgt); step.Outcome != SweepFail {
		t.Errorf("access denied: step %+v, want an immediate failure", step)
	}
}

// Rebinding a member rewrites exactly the proxy URL the bootstrap renders.
func TestRebindUserData(t *testing.T) {
	env, err := spawnaws.EgressProxyEnv(PendingAddr, "us-west-2")
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/bash\n" + env + " spawn_need_aws || true\necho http://192.0.2.1:31289/\n"
	ud := launcher.EncodeLinuxUserData(script)
	if !UserDataNames(ud, PendingAddr) {
		t.Fatal("the rendered user data does not name the pending address")
	}
	out, err := RebindUserData(ud, PendingAddr, "10.0.4.7:3128")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := decodeLinuxUserData(out)
	want, _ := spawnaws.EgressProxyEnv("10.0.4.7:3128", "us-west-2")
	if !strings.Contains(got, want+" spawn_need_aws") {
		t.Errorf("rebound script:\n%s\nwant it to carry %s", got, want)
	}
	if strings.Contains(got, "http://192.0.2.1:3128 ") || !strings.Contains(got, "http://192.0.2.1:31289/") {
		t.Errorf("rebound script:\n%s\nwant the pending URL gone and the longer port kept", got)
	}
	if UserDataNames(out, PendingAddr) {
		t.Error("the rebound user data still names the pending address")
	}
	if same, err := RebindUserData(ud, PendingAddr, PendingAddr); err != nil || same != ud {
		t.Errorf("an unchanged address rewrote the user data (%v)", err)
	}
	if _, err := RebindUserData(launcher.EncodeLinuxUserData("#!/bin/bash\necho hi\n"), PendingAddr, "10.0.4.7:3128"); err == nil {
		t.Error("user data naming no proxy was rebound")
	}
	if _, err := RebindUserData(ud, PendingAddr, "proxy.example:3128"); err == nil {
		t.Error("a host name was rendered into the user data")
	}
}

func TestRebindGroups(t *testing.T) {
	got, err := RebindGroups([]string{"sg-own", "sg-old"}, "sg-old", "sg-new")
	if err != nil || strings.Join(got, ",") != "sg-own,sg-new" {
		t.Errorf("got %v, %v", got, err)
	}
	if _, err := RebindGroups([]string{"sg-own"}, "sg-old", "sg-new"); err == nil {
		t.Error("groups without the recorded clients group were rebound")
	}
}

// The recorded base tags drop what a proxy's launch writes itself.
func TestBaseTagMap(t *testing.T) {
	m := BaseTagMap([]types.Tag{tg("spawn:managed", "true"), tg("spawn:launch-time", "x"), tg("spawn:ttl", "1h"), tg("Name", "n")})
	if len(m) != 1 || m["spawn:managed"] != "true" {
		t.Errorf("BaseTagMap = %v", m)
	}
}
