package main

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spore-host/spawn/pkg/tagprefix"
)

// launchEIP is a spawn-allocated launch Elastic IP (#661) of the given age,
// tagged for instanceID, unassociated unless assoc is set.
func launchEIP(alloc, instanceID string, age time.Duration, now time.Time, assoc string) ec2types.Address {
	tags := append(createdTag(age, now),
		ec2types.Tag{Key: awssdk.String(tagprefix.Tag("managed")), Value: awssdk.String("true")},
		ec2types.Tag{Key: awssdk.String(tagprefix.Tag("instance-id")), Value: awssdk.String(instanceID)},
		ec2types.Tag{Key: awssdk.String(tagprefix.Tag("purpose")), Value: awssdk.String("efa-public-ip")},
	)
	a := ec2types.Address{AllocationId: awssdk.String(alloc), PublicIp: awssdk.String("198.51.100.1"), Tags: tags}
	if assoc != "" {
		a.AssociationId = awssdk.String(assoc)
		a.NetworkInterfaceId = awssdk.String("eni-1")
		a.InstanceId = awssdk.String(instanceID)
	}
	return a
}

// withoutTag / withTag edit one tag of an address, for the criterion table.
func withoutTag(a ec2types.Address, key string) ec2types.Address {
	var tags []ec2types.Tag
	for _, t := range a.Tags {
		if awssdk.ToString(t.Key) != key {
			tags = append(tags, t)
		}
	}
	a.Tags = tags
	return a
}

func withTag(a ec2types.Address, key, value string) ec2types.Address {
	a = withoutTag(a, key)
	a.Tags = append(a.Tags, ec2types.Tag{Key: awssdk.String(key), Value: awssdk.String(value)})
	return a
}

func eipInst(id string, state ec2types.InstanceStateName) ec2types.Instance {
	return ec2types.Instance{InstanceId: awssdk.String(id), State: &ec2types.InstanceState{Name: state}}
}

// runEIPSweep runs the net-resources region sweep with the two switches set
// independently, returning the summary and the log.
func runEIPSweep(t *testing.T, f *fakeNetAPI, dryRun bool, netMode, eipMode string) (*Summary, string) {
	t.Helper()
	r := &reaper{dryRun: dryRun, netResources: netMode, elasticIPs: eipMode}
	acct := account{label: "self", netFor: func(string) netResourceAPI { return f }}
	sum := &Summary{}
	out := captureLogs(func() {
		r.reapNetResourcesRegion(context.Background(), acct, "us-east-1", time.Now(), sum, &accountOutcome{})
	})
	return sum, out
}

// The eipInstanceTagKey this module computes must be the tag pkg/aws writes.
// The reaper does not import pkg/aws, so the two can drift silently; a drift
// would make every launch Elastic IP invisible to the sweep, and they bill.
func TestEIPInstanceTagMatchesWhatLaunchWrites(t *testing.T) {
	if eipInstanceTagKey != "spawn:instance-id" {
		t.Fatalf("eipInstanceTagKey = %q; pkg/aws.ElasticIPInstanceTagKey is spawn:instance-id", eipInstanceTagKey)
	}
}

// TestEIPSweepEachCriterionIsRequired is #1232's reap criteria, one at a time.
// The baseline is released in `reap`; each row removes exactly one criterion and
// must be spared. Released is irreversible (the number goes back to the pool),
// so every condition carries its own row.
func TestEIPSweepEachCriterionIsRequired(t *testing.T) {
	now := time.Now()
	old := eipGrace + time.Hour
	orphan := func() ec2types.Address { return launchEIP("eipalloc-x", "i-gone", old, now, "") }

	for _, tc := range []struct {
		name      string
		addr      ec2types.Address
		instances []ec2types.Instance
		release   bool
	}{
		{"baseline: every criterion holds, instance absent", orphan(), nil, true},
		{"baseline: instance terminated but still listed", orphan(),
			[]ec2types.Instance{eipInst("i-gone", ec2types.InstanceStateNameTerminated)}, true},

		{"no spawn:managed tag", withoutTag(orphan(), tagprefix.Tag("managed")), nil, false},
		{"spawn:managed=false", withTag(orphan(), tagprefix.Tag("managed"), "false"), nil, false},
		{"no instance tag (a user's address, #262)", withoutTag(orphan(), eipInstanceTagKey), nil, false},
		{"empty instance tag", withTag(orphan(), eipInstanceTagKey, ""), nil, false},
		{"no purpose tag", withoutTag(orphan(), tagprefix.Tag("purpose")), nil, false},
		{"another purpose", withTag(orphan(), tagprefix.Tag("purpose"), "something-else"), nil, false},

		{"associated", launchEIP("eipalloc-x", "i-gone", old, now, "eipassoc-1"), nil, false},
		{"association id only", func() ec2types.Address {
			a := orphan()
			a.AssociationId = awssdk.String("eipassoc-1")
			return a
		}(), nil, false},
		{"network interface only", func() ec2types.Address {
			a := orphan()
			a.NetworkInterfaceId = awssdk.String("eni-1")
			return a
		}(), nil, false},

		{"instance running", orphan(), []ec2types.Instance{eipInst("i-gone", ec2types.InstanceStateNameRunning)}, false},
		{"instance pending (mid-launch)", orphan(), []ec2types.Instance{eipInst("i-gone", ec2types.InstanceStateNamePending)}, false},
		{"instance shutting-down", orphan(), []ec2types.Instance{eipInst("i-gone", ec2types.InstanceStateNameShuttingDown)}, false},
		{"instance stopped", orphan(), []ec2types.Instance{eipInst("i-gone", ec2types.InstanceStateNameStopped)}, false},

		{"inside the grace", launchEIP("eipalloc-x", "i-gone", eipGrace-time.Minute, now, ""), nil, false},
		{"no creation stamp", withoutTag(orphan(), tagprefix.Tag("created")), nil, false},
		{"unparseable creation stamp", withTag(orphan(), tagprefix.Tag("created"), "yesterday"), nil, false},
		{"creation stamp in the future", withTag(orphan(), tagprefix.Tag("created"),
			now.Add(time.Hour).Format(time.RFC3339)), nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeNetAPI{instances: tc.instances, addrs: []ec2types.Address{tc.addr}}
			sum, _ := runEIPSweep(t, f, false, netResourcesOff, elasticIPsReap)
			if got := len(f.released) == 1; got != tc.release {
				t.Fatalf("released %v, want release=%v", f.released, tc.release)
			}
			if tc.release && sum.ElasticIPsReleased != 1 {
				t.Errorf("ElasticIPsReleased = %d, want 1", sum.ElasticIPsReleased)
			}
			if !tc.release && sum.ElasticIPsReleased != 0 {
				t.Errorf("ElasticIPsReleased = %d for a spared address", sum.ElasticIPsReleased)
			}
		})
	}
}

// The "instance is gone" criterion rests on the region's DescribeInstances,
// which must ask for every state that still owns its address and NOT for
// terminated. A filter that dropped stopped or shutting-down would make a live
// instance's address look orphaned.
func TestEIPSweepAsksEC2ForEveryLiveState(t *testing.T) {
	f := &fakeNetAPI{}
	runEIPSweep(t, f, false, netResourcesOff, elasticIPsReap)
	var states []string
	for _, fl := range f.instanceFilters {
		if awssdk.ToString(fl.Name) == "instance-state-name" {
			states = fl.Values
		}
	}
	got := map[string]bool{}
	for _, s := range states {
		got[s] = true
	}
	for _, s := range []string{"pending", "running", "shutting-down", "stopping", "stopped"} {
		if !got[s] {
			t.Errorf("instance filter %v omits %q: an address of a %s instance would be released", states, s, s)
		}
	}
	if got["terminated"] {
		t.Errorf("instance filter %v includes terminated: an orphan would never be released", states)
	}
}

// TestEIPSweepModes: `reap` (the default, as it was before the switch existed)
// releases; `report` and a dry run log exactly the address reap would release
// and release nothing; `off` makes no Elastic IP call at all. Every mode that
// examines the address counts it.
func TestEIPSweepModes(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name        string
		dryRun      bool
		mode        string
		wantRelease bool
		wantWould   bool
	}{
		{"reap", false, elasticIPsReap, true, false},
		{"report", false, elasticIPsReport, false, true},
		{"unset resolves to reap", false, parseElasticIPs("", netResourcesReport), true, false},
		{"unset with net-resources off resolves to off", false, parseElasticIPs("", netResourcesOff), false, false},
		{"a typo resolves to report", false, parseElasticIPs("reep", netResourcesReport), false, true},
		{"dry run beats reap", true, elasticIPsReap, false, true},
		{"off", false, elasticIPsOff, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeNetAPI{addrs: []ec2types.Address{launchEIP("eipalloc-orphan", "i-gone", eipGrace+time.Hour, now, "")}}
			sum, out := runEIPSweep(t, f, tc.dryRun, netResourcesOff, tc.mode)
			if got := len(f.released) == 1; got != tc.wantRelease {
				t.Fatalf("released %v, want release=%v", f.released, tc.wantRelease)
			}
			would := strings.Contains(out, "WOULD release Elastic IP eipalloc-orphan (198.51.100.1) in self/us-east-1 — its instance i-gone is gone")
			if would != tc.wantWould {
				t.Errorf("WOULD-release line present=%v, want %v; log:\n%s", would, tc.wantWould, out)
			}
			wantWould := 0
			if tc.wantWould {
				wantWould = 1
			}
			if sum.ElasticIPsWouldRelease != wantWould {
				t.Errorf("ElasticIPsWouldRelease = %d, want %d", sum.ElasticIPsWouldRelease, wantWould)
			}
			if tc.wantRelease && !strings.Contains(out, "RELEASED Elastic IP eipalloc-orphan") {
				t.Errorf("no RELEASED line; log:\n%s", out)
			}
			if tc.mode == elasticIPsOff {
				if f.describeAddrCalls != 0 || sum.ElasticIPsScanned != 0 {
					t.Errorf("off made %d DescribeAddresses calls, scanned %d", f.describeAddrCalls, sum.ElasticIPsScanned)
				}
			} else if sum.ElasticIPsScanned != 1 {
				t.Errorf("ElasticIPsScanned = %d, want 1", sum.ElasticIPsScanned)
			}
			// Elastic IPs have their own counters; the group counters stay theirs.
			if sum.NetReaped != 0 || sum.NetSkipped != 0 {
				t.Errorf("NetReaped/NetSkipped = %d/%d; Elastic IPs must not be counted there", sum.NetReaped, sum.NetSkipped)
			}
		})
	}
}

// The two switches are independent (#1232): arming Elastic IPs must not arm the
// group deletes, holding Elastic IPs in report must not hold the groups back,
// and either half runs with the other off.
func TestEIPSwitchIsIndependentOfNetResources(t *testing.T) {
	now := time.Now()
	fixture := func() *fakeNetAPI {
		return &fakeNetAPI{
			sgs:   []ec2types.SecurityGroup{sg("sg-old", "spawn-mpi-x", netResourceGrace+time.Hour, now)},
			addrs: []ec2types.Address{launchEIP("eipalloc-orphan", "i-gone", eipGrace+time.Hour, now, "")},
		}
	}
	for _, tc := range []struct {
		net, eip            string
		wantSGDel, wantRel  bool
		wantSGCall, wantEIP bool
	}{
		{netResourcesOff, elasticIPsReap, false, true, false, true},
		{netResourcesReport, elasticIPsReap, false, true, true, true},
		{netResourcesReap, elasticIPsReport, true, false, true, true},
		{netResourcesReap, elasticIPsOff, true, false, true, false},
		{netResourcesOff, elasticIPsOff, false, false, false, false},
	} {
		t.Run(tc.net+"/"+tc.eip, func(t *testing.T) {
			f := fixture()
			runEIPSweep(t, f, false, tc.net, tc.eip)
			if got := len(f.deletedSG) == 1; got != tc.wantSGDel {
				t.Errorf("security group deleted=%v, want %v", got, tc.wantSGDel)
			}
			if got := len(f.released) == 1; got != tc.wantRel {
				t.Errorf("Elastic IP released=%v, want %v", got, tc.wantRel)
			}
			if got := f.describeSGCalls > 0; got != tc.wantSGCall {
				t.Errorf("DescribeSecurityGroups called=%v, want %v", got, tc.wantSGCall)
			}
			if got := f.describeAddrCalls > 0; got != tc.wantEIP {
				t.Errorf("DescribeAddresses called=%v, want %v", got, tc.wantEIP)
			}
		})
	}
}

// The default is `reap`, in the template and in the reaper's own handling of an
// unset variable: since v0.138.0 (#661) these addresses were released by
// default, and adding a switch for them (#1232) must not turn that off on the
// next redeploy. An unset variable still follows REAPER_NET_RESOURCES=off, which
// used to turn this sweep off too.
func TestEIPDefaultIsReap(t *testing.T) {
	for net, want := range map[string]string{
		netResourcesReport: elasticIPsReap,
		netResourcesReap:   elasticIPsReap,
		netResourcesOff:    elasticIPsOff,
	} {
		for _, unset := range []string{"", "  "} {
			if got := parseElasticIPs(unset, net); got != want {
				t.Errorf("parseElasticIPs(%q, net-resources=%s) = %q, want %q", unset, net, got, want)
			}
		}
	}
	// An explicit value wins over the net-resources coupling.
	if got := parseElasticIPs("reap", netResourcesOff); got != elasticIPsReap {
		t.Errorf("explicit reap with net-resources off = %q, want reap", got)
	}
	b, err := os.ReadFile("template.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// The template's default is the empty string, so the function sees REAPER_ELASTIC_IPS
	// unset and applies the rule above: reap, or off when NetResources=off. A
	// literal 'reap' default would start releasing in a stack running
	// NetResources=off; 'report' would stop releasing everywhere else.
	m := regexp.MustCompile(`(?m)^  ElasticIps:\n    Type: String\n    Default: '([a-z]*)'`).FindSubmatch(b)
	if m == nil {
		t.Fatal("template.yaml declares no ElasticIps parameter with a Default")
	}
	if got := parseElasticIPs(string(m[1]), netResourcesReport); got != elasticIPsReap {
		t.Errorf("template ElasticIps default %q resolves to %q, want reap: a stack redeployed "+
			"without the parameter would stop releasing addresses it releases today", m[1], got)
	}
	if got := parseElasticIPs(string(m[1]), netResourcesOff); got != elasticIPsOff {
		t.Errorf("template ElasticIps default %q with NetResources=off resolves to %q, want off: "+
			"that stack has never released Elastic IPs", m[1], got)
	}
	if !regexp.MustCompile(`(?m)^\s+REAPER_ELASTIC_IPS:\s+!Ref\s+ElasticIps\s*$`).Match(b) {
		t.Error("template.yaml does not pass ElasticIps to the function as REAPER_ELASTIC_IPS")
	}
}

// eipTerminateNet is the net-resources client for the reaper's terminate path,
// recording into the same call log as the EC2 client so order is observable.
type eipTerminateNet struct {
	*fakeNetAPI
	calls *[]string
	inUse bool
}

func (e *eipTerminateNet) DescribeAddresses(ctx context.Context, in *ec2.DescribeAddressesInput, o ...func(*ec2.Options)) (*ec2.DescribeAddressesOutput, error) {
	*e.calls = append(*e.calls, "describe-addresses")
	return e.fakeNetAPI.DescribeAddresses(ctx, in, o...)
}

func (e *eipTerminateNet) ReleaseAddress(ctx context.Context, in *ec2.ReleaseAddressInput, o ...func(*ec2.Options)) (*ec2.ReleaseAddressOutput, error) {
	*e.calls = append(*e.calls, "release "+awssdk.ToString(in.AllocationId))
	if e.inUse {
		return nil, errors.New("api error InvalidIPAddress.InUse: Address is in use")
	}
	return e.fakeNetAPI.ReleaseAddress(ctx, in, o...)
}

// #661: an instance the reaper terminates is one whose spored failed to end it,
// so nothing on the instance released its launch Elastic IP. In `reap`, the
// default, the reaper does, after the terminate, and only that instance's. In
// `report` and `off` it does not; the sweep reports the address later.
// REAPER_NET_RESOURCES matters only through an unset REAPER_ELASTIC_IPS.
func TestReapTerminateReleasesTheInstancesLaunchElasticIP(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name      string
		eipMode   string
		netMode   string
		inUse     bool
		wantCalls string
	}{
		{"reap", elasticIPsReap, netResourcesReport, false, "terminate,describe-addresses,release eipalloc-mine"},
		{"reap with net-resources off", elasticIPsReap, netResourcesOff, false, "terminate,describe-addresses,release eipalloc-mine"},
		// Nondefault VPC: InUse until EC2 detaches it; the sweep finishes it.
		{"reap, still associated", elasticIPsReap, netResourcesReport, true, "terminate,describe-addresses,release eipalloc-mine"},
		{"default (unset) is reap", "", netResourcesReport, false, "terminate,describe-addresses,release eipalloc-mine"},
		{"unset with net-resources off", "", netResourcesOff, false, "terminate"},
		{"report", elasticIPsReport, netResourcesReap, false, "terminate"},
		{"off", elasticIPsOff, netResourcesReap, false, "terminate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ec2c := &spotEC2{}
			net := &eipTerminateNet{
				fakeNetAPI: &fakeNetAPI{addrs: []ec2types.Address{
					launchEIP("eipalloc-other", "i-other", time.Hour, now, ""),
					launchEIP("eipalloc-mine", "i-spot", time.Hour, now, "eipassoc-1"),
				}},
				calls: &ec2c.calls,
				inUse: tc.inUse,
			}
			r := &reaper{netResources: tc.netMode, elasticIPs: parseElasticIPs(tc.eipMode, tc.netMode)}
			acct := account{label: "acct", ec2For: func(string) ec2API { return ec2c }, netFor: func(string) netResourceAPI { return net }}
			if err := r.terminate(context.Background(), acct, candidate{id: "i-spot", region: "us-east-1"}); err != nil {
				t.Fatalf("terminate: %v (an Elastic IP problem must never fail the reap)", err)
			}
			if got := strings.Join(ec2c.calls, ","); got != tc.wantCalls {
				t.Errorf("calls = %s, want %s", got, tc.wantCalls)
			}
		})
	}
}

// A failed release is logged and counted, not fatal to the sweep.
func TestEIPSweepReleaseFailureIsNotFatal(t *testing.T) {
	now := time.Now()
	f := &fakeNetAPI{
		addrs: []ec2types.Address{
			launchEIP("eipalloc-a", "i-gone", eipGrace+time.Hour, now, ""),
			launchEIP("eipalloc-b", "i-gone2", eipGrace+time.Hour, now, ""),
		},
		releaseErr: errors.New("UnauthorizedOperation"),
	}
	sum, _ := runEIPSweep(t, f, false, netResourcesOff, elasticIPsReap)
	if sum.ElasticIPsReleased != 0 || sum.ElasticIPsSkipped != 2 {
		t.Errorf("ElasticIPsReleased/Skipped = %d/%d, want 0/2 (both attempted)", sum.ElasticIPsReleased, sum.ElasticIPsSkipped)
	}
}

// An address with no usable spawn:created stamp (a pre-release build, a
// hand-edited tag) is neither skipped forever nor released on a guessed age:
// reap stamps it with the reaper's first-seen tag and releases it once that is
// past the grace. Report and dry run write nothing and say what reap would do.
func TestEIPWithoutCreatedStampIsAgedFromFirstSeen(t *testing.T) {
	now := time.Now()
	noStamp := func() ec2types.Address {
		return withoutTag(launchEIP("eipalloc-x", "i-gone", time.Hour, now, ""), tagprefix.Tag("created"))
	}
	seen := func(age time.Duration) ec2types.Address {
		return withTag(noStamp(), discoveryTagKey, now.Add(-age).Format(time.RFC3339))
	}
	for _, tc := range []struct {
		name        string
		addr        ec2types.Address
		dryRun      bool
		mode        string
		wantStamp   bool
		wantRelease bool
		wantWould   bool
	}{
		{"first sight: stamped, not released", noStamp(), false, elasticIPsReap, true, false, false},
		{"first-seen past the grace: released", seen(eipGrace + time.Minute), false, elasticIPsReap, false, true, false},
		{"first-seen inside the grace: waits", seen(eipGrace - time.Minute), false, elasticIPsReap, false, false, false},
		{"unparseable stamp falls back too", withTag(noStamp(), tagprefix.Tag("created"), "yesterday"), false, elasticIPsReap, true, false, false},
		{"report writes nothing", noStamp(), false, elasticIPsReport, false, false, true},
		{"dry run writes nothing", noStamp(), true, elasticIPsReap, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeNetAPI{addrs: []ec2types.Address{tc.addr}}
			sum, out := runEIPSweep(t, f, tc.dryRun, netResourcesOff, tc.mode)
			if got := len(f.tagged) == 1 && f.tagged[0] == "eipalloc-x"; got != tc.wantStamp {
				t.Errorf("stamped %v, want stamp=%v", f.tagged, tc.wantStamp)
			}
			if got := len(f.released) == 1; got != tc.wantRelease {
				t.Errorf("released %v, want release=%v", f.released, tc.wantRelease)
			}
			if got := sum.ElasticIPsWouldRelease == 1 && strings.Contains(out, "WOULD release Elastic IP eipalloc-x"); got != tc.wantWould {
				t.Errorf("would-release=%v (count %d), want %v; log:\n%s", got, sum.ElasticIPsWouldRelease, tc.wantWould, out)
			}
		})
	}
}

// The handler resolves the two switches from its environment together: an
// unset REAPER_ELASTIC_IPS follows REAPER_NET_RESOURCES=off and is reap
// otherwise, and an explicit value wins.
func TestNetSweepModesFromEnv(t *testing.T) {
	for _, tc := range []struct{ net, eip, wantNet, wantEIP string }{
		{"", "", netResourcesReport, elasticIPsReap},
		{"report", "", netResourcesReport, elasticIPsReap},
		{"off", "", netResourcesOff, elasticIPsOff},
		{"OFF", "", netResourcesOff, elasticIPsOff},
		{"off", "reap", netResourcesOff, elasticIPsReap},
		{"reap", "report", netResourcesReap, elasticIPsReport},
		{"reap", "reep", netResourcesReap, elasticIPsReport},
	} {
		t.Setenv("REAPER_NET_RESOURCES", tc.net)
		t.Setenv("REAPER_ELASTIC_IPS", tc.eip)
		gotNet, gotEIP := netSweepModesFromEnv()
		if gotNet != tc.wantNet || gotEIP != tc.wantEIP {
			t.Errorf("NET_RESOURCES=%q ELASTIC_IPS=%q -> %s/%s, want %s/%s", tc.net, tc.eip, gotNet, gotEIP, tc.wantNet, tc.wantEIP)
		}
	}
}
