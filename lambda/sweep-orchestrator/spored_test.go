package main

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
)

// TestMemberLaunchesWithTheCLIsBootstrap is #1323's Lambda half: a member's
// RunInstances carries, verbatim, the user data the CLI rendered for it (the CLI
// half, cmd/launch_sweep_detached_bootstrap_test.go, proves that is what the
// foreground path builds for the same member), a row's own overriding the
// defaults', and the instance profile spored needs for credentials.
func TestMemberLaunchesWithTheCLIsBootstrap(t *testing.T) {
	params := &ParamFileFormat{
		Defaults: map[string]interface{}{
			"instance_type": "t3.micro", "ami": "ami-0123456789abcdef0",
			userDataParam: "H4sIAAAAAAAA/default", "iam_role": "spored-instance-profile",
			tagsParam: cliTags(nil),
		},
		Params: []map[string]interface{}{
			{"alpha": 1.0},
			{"alpha": 2.0, userDataParam: "H4sIAAAAAAAA/row-1"},
		},
	}
	state := &SweepRecord{SweepID: "s-1", SweepName: "s"}
	for i, want := range []string{"H4sIAAAAAAAA/default", "H4sIAAAAAAAA/row-1"} {
		in, err := buildRunInstancesInput(state, mergeParams(params.Defaults, params.Params[i]), i, "t3.micro", time.Now())
		if err != nil {
			t.Fatalf("row %d: %v", i, err)
		}
		if got := awssdk.ToString(in.UserData); got != want {
			t.Errorf("row %d UserData = %q, want the CLI's %q verbatim", i, got, want)
		}
		if in.IamInstanceProfile == nil || awssdk.ToString(in.IamInstanceProfile.Name) != "spored-instance-profile" {
			t.Errorf("row %d IamInstanceProfile = %+v, want spored-instance-profile: spored has no credentials without it", i, in.IamInstanceProfile)
		}
	}

	// No iam_role at all: the fallback is the profile spore.host creates, not a
	// hand-made one.
	in, err := buildRunInstancesInput(state, map[string]interface{}{userDataParam: "x", tagsParam: cliTags(nil)}, 0, "t3.micro", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := awssdk.ToString(in.IamInstanceProfile.Name); got != spawnaws.SporedInstanceProfileName {
		t.Errorf("fallback profile = %q, want %q", got, spawnaws.SporedInstanceProfileName)
	}
}

// A member with no bootstrap is refused, not launched: it would run with no
// in-instance TTL (#1323).
func TestMemberWithoutBootstrapIsNotLaunched(t *testing.T) {
	in, err := buildRunInstancesInput(&SweepRecord{SweepID: "s"}, map[string]interface{}{"ami": "ami-1", tagsParam: cliTags(nil)}, 3, "t3.micro", time.Now())
	if err == nil || in != nil {
		t.Fatalf("launched a member with no spored bootstrap: input=%+v err=%v", in, err)
	}
	if !strings.Contains(err.Error(), userDataParam) {
		t.Errorf("error %q does not say what was missing", err)
	}
	if capacity, _ := isCapacityError(err); capacity {
		t.Error("a missing bootstrap reads as a capacity error, so the next instance type would be tried")
	}
}

// Every RunInstances call is fed by buildRunInstancesInput, and no other
// RunInstancesInput is composed: a second, hand-built input is how one launch
// path would lose the bootstrap.
func TestEveryRunInstancesCallUsesTheBuilder(t *testing.T) {
	src := readMainForTest(t)
	calls := regexp.MustCompile(`(?m)^\s*result, err := ec2Client\.RunInstances\(ctx, input\)`).FindAllString(src, -1)
	built := regexp.MustCompile(`(?m)^\s*input, err := buildRunInstancesInput\(state, config, paramIndex, instanceType, clock\(\)\)`).FindAllString(src, -1)
	anyCall := regexp.MustCompile(`\.RunInstances\(`).FindAllString(src, -1)
	literals := regexp.MustCompile(`&ec2\.RunInstancesInput\{`).FindAllString(src, -1)
	if len(anyCall) == 0 {
		t.Fatal("found no RunInstances call; the matcher is stale")
	}
	if len(calls) != len(anyCall) || len(built) != len(calls) {
		t.Errorf("%d RunInstances call(s), %d of the expected form, %d fed by buildRunInstancesInput", len(anyCall), len(calls), len(built))
	}
	if len(literals) != 1 {
		t.Errorf("%d RunInstancesInput literals, want exactly the one in buildRunInstancesInput", len(literals))
	}
}

// fakeEC2 answers DescribeInstances with fixed instances and records
// TerminateInstances.
type fakeEC2 struct {
	mu           sync.Mutex
	instances    []ec2types.Instance
	terminated   []string
	terminateErr error
}

func (f *fakeEC2) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	want := map[string]bool{}
	for _, id := range in.InstanceIds {
		want[id] = true
	}
	var out []ec2types.Instance
	for _, i := range f.instances {
		if want[awssdk.ToString(i.InstanceId)] {
			out = append(out, i)
		}
	}
	return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: out}}}, nil
}

func (f *fakeEC2) TerminateInstances(_ context.Context, in *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.terminateErr != nil {
		return nil, f.terminateErr
	}
	f.terminated = append(f.terminated, in.InstanceIds...)
	return &ec2.TerminateInstancesOutput{}, nil
}

func running(id string, windows bool, heartbeat string) ec2types.Instance {
	i := ec2types.Instance{
		InstanceId: awssdk.String(id),
		State:      &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
	}
	if windows {
		i.Platform = ec2types.PlatformValuesWindows
	}
	if heartbeat != "" {
		i.Tags = []ec2types.Tag{{Key: awssdk.String(spawnaws.HeartbeatTag), Value: awssdk.String(heartbeat)}}
	}
	return i
}

// heartbeatFixture is a sweep of four running members, launched at the given
// ages before now: one whose spored never came up past the Linux budget, one
// verified, one still inside the budget, and a Windows one past the Linux
// budget but inside its own.
func heartbeatFixture(now time.Time, region string) (*SweepRecord, *fakeEC2) {
	ago := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	state := &SweepRecord{
		SweepID: "s", Launched: 4,
		Instances: []SweepInstance{
			{Index: 0, Region: region, InstanceID: "i-nospored", State: "running", LaunchedAt: ago(6 * time.Minute)},
			{Index: 1, Region: region, InstanceID: "i-verified", State: "running", LaunchedAt: ago(6 * time.Minute)},
			{Index: 2, Region: region, InstanceID: "i-young", State: "running", LaunchedAt: ago(1 * time.Minute)},
			{Index: 3, Region: region, InstanceID: "i-windows", State: "running", LaunchedAt: ago(6 * time.Minute)},
		},
	}
	api := &fakeEC2{instances: []ec2types.Instance{
		running("i-nospored", false, ""),
		running("i-verified", false, now.Add(-time.Minute).Format(time.RFC3339)),
		running("i-young", false, ""),
		running("i-windows", true, ""),
	}}
	return state, api
}

func assertOnlyNoSporedReaped(t *testing.T, state *SweepRecord, api *fakeEC2, active int) {
	t.Helper()
	if len(api.terminated) != 1 || api.terminated[0] != "i-nospored" {
		t.Errorf("terminated %v, want exactly [i-nospored]: the member past its budget with no heartbeat, "+
			"and none of the verified, young or Windows members", api.terminated)
	}
	if active != 3 {
		t.Errorf("active = %d, want 3: the terminated member must free its --max-concurrent slot", active)
	}
	if state.Failed != 1 || state.Launched != 3 {
		t.Errorf("Failed=%d Launched=%d, want 1 and 3: the member is a failed row, not a launched one", state.Failed, state.Launched)
	}
	for _, m := range state.Instances {
		failed := m.State == "failed"
		if failed != (m.InstanceID == "i-nospored") {
			t.Errorf("%s state = %q", m.InstanceID, m.State)
		}
		if failed && !strings.Contains(m.ErrorMessage, "spored never came up") {
			t.Errorf("%s error = %q; does not say why it was terminated", m.InstanceID, m.ErrorMessage)
		}
	}
}

// TestSingleRegionReapsMembersWithoutSpored is #1313 on the single-region loop.
func TestSingleRegionReapsMembersWithoutSpored(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	prev := clock
	clock = func() time.Time { return now }
	t.Cleanup(func() { clock = prev })

	state, api := heartbeatFixture(now, "us-east-1")
	active, err := countActiveInstances(context.Background(), api, state)
	if err != nil {
		t.Fatal(err)
	}
	assertOnlyNoSporedReaped(t, state, api, active)
	if got := completionStatus(0, state.Failed); got != "FAILED" {
		t.Errorf("a sweep whose only member's spored never came up records %q", got)
	}
}

// TestMultiRegionReapsMembersWithoutSpored is #1313 on the multi-region loop,
// including the per-region tallies.
func TestMultiRegionReapsMembersWithoutSpored(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	state, api := heartbeatFixture(now, "eu-west-1")
	state.MultiRegion = true
	state.RegionStatus = map[string]*RegionProgress{"eu-west-1": {Launched: 4}}

	byRegion, killed := countActiveByRegion(context.Background(), map[string]ec2InstancesAPI{"eu-west-1": api}, state, now)
	assertOnlyNoSporedReaped(t, state, api, byRegion["eu-west-1"])
	if len(killed) != 1 {
		t.Errorf("killed = %v", killed)
	}
	if rs := state.RegionStatus["eu-west-1"]; rs.Failed != 1 || rs.Launched != 3 || rs.ActiveCount != 3 {
		t.Errorf("region tally = %+v, want Failed=1 Launched=3 ActiveCount=3", *rs)
	}
}

// A member whose termination failed is still billing: it stays active and is
// not yet a failed row, so the next poll tries again.
func TestFailedTerminationIsRetriedNotRecorded(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	prev := clock
	clock = func() time.Time { return now }
	t.Cleanup(func() { clock = prev })

	state, api := heartbeatFixture(now, "us-east-1")
	api.terminateErr = context.DeadlineExceeded
	active, err := countActiveInstances(context.Background(), api, state)
	if err != nil {
		t.Fatal(err)
	}
	if active != 4 || state.Failed != 0 {
		t.Errorf("active=%d failed=%d after a failed terminate, want 4 and 0", active, state.Failed)
	}
}

// The budgets are the CLI's (#1305): five minutes on Linux, ten on Windows.
func TestSporedBudgetsMatchTheCLI(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		windows bool
		age     time.Duration
		want    bool
	}{
		{false, 4*time.Minute + 59*time.Second, false},
		{false, 5*time.Minute + time.Second, true},
		{true, 9*time.Minute + 59*time.Second, false},
		{true, 10*time.Minute + time.Second, true},
	}
	for _, c := range cases {
		got := sporedOverdue(running("i", c.windows, ""), now.Add(-c.age).Format(time.RFC3339), now)
		if got != c.want {
			t.Errorf("windows=%v age=%s: overdue=%v, want %v", c.windows, c.age, got, c.want)
		}
	}
	// A pending member has not booted; it is never overdue.
	p := running("i", false, "")
	p.State.Name = ec2types.InstanceStateNamePending
	if sporedOverdue(p, now.Add(-time.Hour).Format(time.RFC3339), now) {
		t.Error("a pending member was judged to have no spored")
	}
}

// Both polling loops decide completion with completionStatus, so a sweep whose
// every member's spored never came up records FAILED on either (#1313, #749).
func TestBothLoopsDecideCompletionTheSameWay(t *testing.T) {
	src := readMainForTest(t)
	loops := regexp.MustCompile(`(?m)^func run(?:MultiRegion)?PollingLoop\(`).FindAllString(src, -1)
	decided := regexp.MustCompile(`(?m)^\s*state\.Status = completionStatus\(state\.Launched, state\.Failed\)`).FindAllString(src, -1)
	if len(loops) != 2 {
		t.Fatalf("found %d polling loops; the matcher is stale", len(loops))
	}
	if len(decided) != len(loops) {
		t.Errorf("%d polling loops but %d decide completion with completionStatus", len(loops), len(decided))
	}
	if regexp.MustCompile(`(?m)^\s*state\.Status = "COMPLETED"`).MatchString(src) {
		t.Error("a loop still sets COMPLETED unconditionally")
	}
}
