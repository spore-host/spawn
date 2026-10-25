package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spore-host/spawn/pkg/tagprefix"
)

const (
	evInstance = "i-0123456789abcdef0"
	evMember   = "222222222222"
	evOther    = "111111111111"
)

// terminatedEvent is the event EventBridge delivers for a terminated instance,
// in the shape the EC2 documentation gives for it.
func terminatedEvent(acct, region, id, state string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{
  "version": "0", "id": "7bf73129-1428-4cd3-a780-95db273d1602",
  "detail-type": "EC2 Instance State-change Notification",
  "source": "aws.ec2", "account": %q, "time": "2026-10-24T12:00:00Z", "region": %q,
  "resources": ["arn:aws:ec2:%s:%s:instance/%s"],
  "detail": {"instance-id": %q, "state": %q}
}`, acct, region, region, acct, id, id, state))
}

// countingNet is a fakeNetAPI that counts every call, can answer InUse for the
// first inUse releases, and can fail DescribeInstances after the first call.
type countingNet struct {
	fakeNetAPI
	calls          int
	releaseCalls   int
	inUse          int
	describeErrNth int // fail the Nth DescribeInstances (1-based); 0 never
	describeCalls  int
}

func (c *countingNet) DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, o ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	c.calls++
	c.describeCalls++
	if c.describeErrNth != 0 && c.describeCalls == c.describeErrNth {
		return nil, errors.New("RequestLimitExceeded")
	}
	return c.fakeNetAPI.DescribeInstances(ctx, in, o...)
}

func (c *countingNet) DescribeAddresses(ctx context.Context, in *ec2.DescribeAddressesInput, o ...func(*ec2.Options)) (*ec2.DescribeAddressesOutput, error) {
	c.calls++
	return c.fakeNetAPI.DescribeAddresses(ctx, in, o...)
}

func (c *countingNet) ReleaseAddress(ctx context.Context, in *ec2.ReleaseAddressInput, o ...func(*ec2.Options)) (*ec2.ReleaseAddressOutput, error) {
	c.calls++
	c.releaseCalls++
	if c.inUse > 0 {
		c.inUse--
		return nil, errors.New("api error InvalidIPAddress.InUse: Address is in use")
	}
	return c.fakeNetAPI.ReleaseAddress(ctx, in, o...)
}

// userEIP is an Elastic IP with no spawn tags at all: a user's own (#262).
func userEIP(alloc string) ec2types.Address {
	return ec2types.Address{AllocationId: awssdk.String(alloc), PublicIp: awssdk.String("203.0.113.9")}
}

// eventReaper has two configured accounts, each with its own EC2 fake, so a
// test can see which account's credentials an action went through.
func eventReaper(mode string, dryRun bool, member, other *countingNet) *reaper {
	return &reaper{
		regions:        []string{"us-east-1", "us-west-2"},
		dryRun:         dryRun,
		elasticIPs:     mode,
		eventInUseWait: -1,
		accounts: []account{
			{label: evOther, accountID: evOther, netFor: func(string) netResourceAPI { return other }},
			{label: evMember, accountID: evMember, netFor: func(string) netResourceAPI { return member }},
		},
	}
}

// TestInstanceEventReleasesTheLaunchEIPThroughTheMemberAccount is #1287's main
// property: a terminated event for a spawn instance with a launch Elastic IP
// releases exactly that address, through the credentials of the account the
// event came from, with no age grace — the address was allocated a minute ago,
// which the sweep's 15-minute grace would hold back.
func TestInstanceEventReleasesTheLaunchEIPThroughTheMemberAccount(t *testing.T) {
	now := time.Now()
	member := &countingNet{fakeNetAPI: fakeNetAPI{
		instances: []ec2types.Instance{eipInst(evInstance, ec2types.InstanceStateNameTerminated)},
		addrs: []ec2types.Address{
			launchEIP("eipalloc-launch", evInstance, time.Minute, now, ""),
			// The describe filter is the API's promise; these must be refused
			// even when a (fake) API returns them.
			userEIP("eipalloc-user"),
			withoutTag(launchEIP("eipalloc-nopurpose", evInstance, time.Hour, now, ""), tagprefix.Tag("purpose")),
			withTag(launchEIP("eipalloc-unmanaged", evInstance, time.Hour, now, ""), tagprefix.Tag("managed"), "false"),
			launchEIP("eipalloc-other-instance", "i-0fffffffffffffff0", time.Hour, now, ""),
		},
	}}
	other := &countingNet{}
	rp := eventReaper(elasticIPsReap, false, member, other)

	ev, ok := parseInstanceEvent(terminatedEvent(evMember, "us-west-2", evInstance, "terminated"))
	if !ok {
		t.Fatal("a terminated EC2 state-change event was not recognised")
	}
	var sum Summary
	out := captureLogs(func() { sum = rp.handleInstanceEvent(context.Background(), ev) })

	if got := strings.Join(member.released, ","); got != "eipalloc-launch" {
		t.Fatalf("released %q, want only eipalloc-launch\n%s", got, out)
	}
	if other.calls != 0 {
		t.Errorf("the other account's credentials made %d calls; the event came from %s", other.calls, evMember)
	}
	if sum.ElasticIPsReleased != 1 || sum.EventInstance != evInstance || sum.Event == "" {
		t.Errorf("summary = %+v", sum)
	}
	if !strings.Contains(out, "RELEASED Elastic IP eipalloc-launch") {
		t.Errorf("no RELEASED line (the idle check counts it as work):\n%s", out)
	}
}

// TestInstanceEventReleasesNothingThatIsNotALaunchEIP: a non-spawn instance (no
// addresses tagged for it), an instance with no Elastic IP, and an address
// without spawn tags all end with nothing released.
func TestInstanceEventReleasesNothingThatIsNotALaunchEIP(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name  string
		addrs []ec2types.Address
	}{
		{"no Elastic IP", nil},
		{"a user's Elastic IP, no spawn tags", []ec2types.Address{userEIP("eipalloc-user")}},
		{"spawn:managed but no instance tag", []ec2types.Address{withoutTag(launchEIP("eipalloc-x", evInstance, time.Hour, now, ""), eipInstanceTagKey)}},
		{"tagged for another instance", []ec2types.Address{launchEIP("eipalloc-x", "i-0fffffffffffffff0", time.Hour, now, "")}},
		{"another purpose", []ec2types.Address{withTag(launchEIP("eipalloc-x", evInstance, time.Hour, now, ""), tagprefix.Tag("purpose"), "something-else")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			member := &countingNet{fakeNetAPI: fakeNetAPI{addrs: tc.addrs}}
			rp := eventReaper(elasticIPsReap, false, member, &countingNet{})
			ev, _ := parseInstanceEvent(terminatedEvent(evMember, "us-east-1", evInstance, "terminated"))
			sum := rp.handleInstanceEvent(context.Background(), ev)
			if member.releaseCalls != 0 || sum.ElasticIPsReleased != 0 {
				t.Fatalf("ReleaseAddress called %d times (released %v)", member.releaseCalls, member.released)
			}
		})
	}
}

// TestInstanceEventActsOnlyOnAConfirmedTerminationFromAConfiguredAccount: an
// event that is not a termination, names an account or region the reaper is not
// configured for, or names an instance EC2 says is running is not acted on.
func TestInstanceEventActsOnlyOnAConfirmedTerminationFromAConfiguredAccount(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name      string
		raw       json.RawMessage
		instState ec2types.InstanceStateName
		wantCalls bool // whether any EC2 call is expected at all
	}{
		{"stopping, not terminated", terminatedEvent(evMember, "us-east-1", evInstance, "stopping"), "", false},
		{"shutting-down", terminatedEvent(evMember, "us-east-1", evInstance, "shutting-down"), "", false},
		{"unconfigured account", terminatedEvent("333333333333", "us-east-1", evInstance, "terminated"), "", false},
		{"region not in REAPER_REGIONS", terminatedEvent(evMember, "eu-west-1", evInstance, "terminated"), "", false},
		{"malformed instance id", terminatedEvent(evMember, "us-east-1", "i-; rm -rf", "terminated"), "", false},
		{"another detail type", json.RawMessage(`{"source":"aws.ec2","detail-type":"EC2 Spot Instance Interruption Warning","account":"` +
			evMember + `","region":"us-east-1","detail":{"instance-id":"` + evInstance + `","state":"terminated"}}`), "", false},
		{"EC2 says the instance is running", terminatedEvent(evMember, "us-east-1", evInstance, "terminated"), ec2types.InstanceStateNameRunning, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			member := &countingNet{fakeNetAPI: fakeNetAPI{addrs: []ec2types.Address{launchEIP("eipalloc-x", evInstance, time.Hour, now, "")}}}
			if tc.instState != "" {
				member.instances = []ec2types.Instance{eipInst(evInstance, tc.instState)}
			}
			rp := eventReaper(elasticIPsReap, false, member, &countingNet{})
			ev, ok := parseInstanceEvent(tc.raw)
			if !ok {
				t.Fatal("an aws.ec2 event must take the event path")
			}
			rp.handleInstanceEvent(context.Background(), ev)
			if member.releaseCalls != 0 {
				t.Fatalf("released %v", member.released)
			}
			if !tc.wantCalls && member.calls != 0 {
				t.Errorf("made %d EC2 calls for an event that should be ignored outright", member.calls)
			}
		})
	}
}

// TestInstanceEventFollowsElasticIpsAndDryRun: REAPER_ELASTIC_IPS and
// REAPER_DRY_RUN govern the event path as they do the sweep.
func TestInstanceEventFollowsElasticIpsAndDryRun(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name      string
		mode      string
		dryRun    bool
		release   bool
		wouldLine bool
	}{
		{"reap", elasticIPsReap, false, true, false},
		{"report", elasticIPsReport, false, false, true},
		{"reap under a dry run", elasticIPsReap, true, false, true},
		{"off", elasticIPsOff, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			member := &countingNet{fakeNetAPI: fakeNetAPI{addrs: []ec2types.Address{launchEIP("eipalloc-x", evInstance, time.Minute, now, "")}}}
			rp := eventReaper(tc.mode, tc.dryRun, member, &countingNet{})
			ev, _ := parseInstanceEvent(terminatedEvent(evMember, "us-east-1", evInstance, "terminated"))
			var sum Summary
			out := captureLogs(func() { sum = rp.handleInstanceEvent(context.Background(), ev) })
			if got := len(member.released) == 1; got != tc.release {
				t.Errorf("released %v, want release=%v", member.released, tc.release)
			}
			if got := strings.Contains(out, "WOULD release Elastic IP eipalloc-x"); got != tc.wouldLine {
				t.Errorf("WOULD line present=%v, want %v:\n%s", got, tc.wouldLine, out)
			}
			if tc.wouldLine && sum.ElasticIPsWouldRelease != 1 {
				t.Errorf("ElasticIPsWouldRelease = %d", sum.ElasticIPsWouldRelease)
			}
			if tc.mode == elasticIPsOff && member.calls != 0 {
				t.Errorf("off made %d EC2 calls", member.calls)
			}
		})
	}
}

// TestInstanceEventRetriesAnAddressStillInUse: the terminated event can arrive
// a moment before EC2 detaches the address. The release is retried a bounded
// number of times, counted through the seam, then left to the sweep.
func TestInstanceEventRetriesAnAddressStillInUse(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name      string
		inUse     int
		wantCalls int
		release   bool
	}{
		{"in use twice, then released", 2, 3, true},
		{"in use throughout", 100, eventInUseAttempts, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			member := &countingNet{inUse: tc.inUse, fakeNetAPI: fakeNetAPI{addrs: []ec2types.Address{launchEIP("eipalloc-x", evInstance, time.Minute, now, "eipassoc-1")}}}
			rp := eventReaper(elasticIPsReap, false, member, &countingNet{})
			ev, _ := parseInstanceEvent(terminatedEvent(evMember, "us-east-1", evInstance, "terminated"))
			sum := rp.handleInstanceEvent(context.Background(), ev)
			if member.releaseCalls != tc.wantCalls {
				t.Errorf("ReleaseAddress called %d times, want %d", member.releaseCalls, tc.wantCalls)
			}
			if got := sum.ElasticIPsReleased == 1; got != tc.release {
				t.Errorf("released=%v, want %v (summary %+v)", got, tc.release, sum)
			}
			if !tc.release && sum.ElasticIPsSkipped != 1 {
				t.Errorf("ElasticIPsSkipped = %d, want 1", sum.ElasticIPsSkipped)
			}
		})
	}
}

// TestHandlerRoutesAnInstanceEventAwayFromTheScan: through the real entrypoint,
// a terminated event releases the address and does not run the full scan (no
// instance scan, no sentinel), so a burst of terminations is not a burst of
// full scans.
func TestHandlerRoutesAnInstanceEventAwayFromTheScan(t *testing.T) {
	prev := r
	defer func() { r = prev }()

	member := &countingNet{fakeNetAPI: fakeNetAPI{addrs: []ec2types.Address{launchEIP("eipalloc-x", evInstance, time.Minute, time.Now(), "")}}}
	rp := eventReaper(elasticIPsReap, false, member, &countingNet{})
	scanned := 0
	for i := range rp.accounts {
		rp.accounts[i].ec2For = func(string) ec2API { scanned++; return &failingEC2{err: apiErr("AccessDenied")} }
	}
	r = rp

	var sum Summary
	out := captureLogs(func() {
		sum, _ = handler(context.Background(), terminatedEvent(evMember, "us-east-1", evInstance, "terminated"))
	})
	if len(member.released) != 1 {
		t.Fatalf("released %v through the entrypoint\n%s", member.released, out)
	}
	if scanned != 0 || strings.Contains(out, sentinelAllAccountsFailed) {
		t.Errorf("an instance event ran the full scan (%d instance-scan clients):\n%s", scanned, out)
	}
	if sum.Event == "" {
		t.Errorf("summary does not say it was an event invocation: %+v", sum)
	}
}

// TestEIPSweepSkipsTheGraceOnlyForAConfirmedTermination is the sweep half of
// #1287: the 15-minute age grace (and the first-seen stamp for an address with
// no spawn:created) exist because DescribeInstances may not list an instance
// yet. When it lists the instance as terminated, neither applies. When it does
// not list it, or cannot be asked, both still do.
func TestEIPSweepSkipsTheGraceOnlyForAConfirmedTermination(t *testing.T) {
	now := time.Now()
	young := launchEIP("eipalloc-x", "i-gone", time.Minute, now, "")
	unstamped := withoutTag(launchEIP("eipalloc-x", "i-gone", time.Hour, now, ""), tagprefix.Tag("created"))
	term := []ec2types.Instance{eipInst("i-gone", ec2types.InstanceStateNameTerminated)}
	for _, tc := range []struct {
		name        string
		addr        ec2types.Address
		instances   []ec2types.Instance
		describeErr int
		release     bool
		stamp       bool
	}{
		{"inside the grace, instance terminated", young, term, 0, true, false},
		{"inside the grace, instance absent", young, nil, 0, false, false},
		{"inside the grace, the terminated check fails", young, term, 2, false, false},
		{"no stamp, instance terminated", unstamped, term, 0, true, false},
		{"no stamp, instance absent", unstamped, nil, 0, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &countingNet{describeErrNth: tc.describeErr, fakeNetAPI: fakeNetAPI{instances: tc.instances, addrs: []ec2types.Address{tc.addr}}}
			r := &reaper{netResources: netResourcesOff, elasticIPs: elasticIPsReap}
			acct := account{label: "self", netFor: func(string) netResourceAPI { return f }}
			sum := &Summary{}
			captureLogs(func() {
				r.reapNetResourcesRegion(context.Background(), acct, "us-east-1", now, sum, &accountOutcome{})
			})
			if got := len(f.released) == 1; got != tc.release {
				t.Errorf("released %v, want release=%v", f.released, tc.release)
			}
			if got := len(f.tagged) == 1; got != tc.stamp {
				t.Errorf("stamped %v, want stamp=%v", f.tagged, tc.stamp)
			}
		})
	}
}
