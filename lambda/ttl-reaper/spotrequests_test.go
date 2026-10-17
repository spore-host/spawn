package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// fakeSpotEC2 serves a fixed set of Spot requests and instances, and records
// every call the sweep makes in order.
type fakeSpotEC2 struct {
	reqs  []ec2types.SpotInstanceRequest
	insts []ec2types.Instance

	describeErr  error
	cancelErr    error
	terminateErr error

	calls        []string
	reqFilters   []ec2types.Filter
	instInput    []*ec2.DescribeInstancesInput
	cancelled    []string
	terminated   []string
	describeReqs int
}

func (f *fakeSpotEC2) DescribeSpotInstanceRequests(_ context.Context, in *ec2.DescribeSpotInstanceRequestsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSpotInstanceRequestsOutput, error) {
	f.calls = append(f.calls, "describe-requests")
	f.describeReqs++
	f.reqFilters = in.Filters
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	return &ec2.DescribeSpotInstanceRequestsOutput{SpotInstanceRequests: f.reqs}, nil
}

func (f *fakeSpotEC2) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	f.calls = append(f.calls, "describe-instances")
	f.instInput = append(f.instInput, in)
	// Answer only for the instance-id filter, as EC2 would. An ID EC2 no longer
	// knows is simply absent.
	want := map[string]bool{}
	for _, flt := range in.Filters {
		if aws.ToString(flt.Name) == "instance-id" {
			for _, v := range flt.Values {
				want[v] = true
			}
		}
	}
	var out []ec2types.Instance
	for _, inst := range f.insts {
		if want[aws.ToString(inst.InstanceId)] {
			out = append(out, inst)
		}
	}
	return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: out}}}, nil
}

func (f *fakeSpotEC2) CancelSpotInstanceRequests(_ context.Context, in *ec2.CancelSpotInstanceRequestsInput, _ ...func(*ec2.Options)) (*ec2.CancelSpotInstanceRequestsOutput, error) {
	f.calls = append(f.calls, "cancel:"+strings.Join(in.SpotInstanceRequestIds, ","))
	if f.cancelErr != nil {
		return nil, f.cancelErr
	}
	f.cancelled = append(f.cancelled, in.SpotInstanceRequestIds...)
	return &ec2.CancelSpotInstanceRequestsOutput{}, nil
}

func (f *fakeSpotEC2) TerminateInstances(_ context.Context, in *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	f.calls = append(f.calls, "terminate:"+strings.Join(in.InstanceIds, ","))
	if f.terminateErr != nil {
		return nil, f.terminateErr
	}
	f.terminated = append(f.terminated, in.InstanceIds...)
	return &ec2.TerminateInstancesOutput{}, nil
}

var sweepNow = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

func tags(kv ...string) []ec2types.Tag {
	var out []ec2types.Tag
	for i := 0; i+1 < len(kv); i += 2 {
		out = append(out, ec2types.Tag{Key: aws.String(kv[i]), Value: aws.String(kv[i+1])})
	}
	return out
}

func rfc(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// spotReq is a spawn-tagged persistent request. Its spawn:ttl-deadline is in
// the PAST unless overridden: the request's copy of the deadline goes stale
// after `spawn extend`, and the sweep must not act on it while the original
// instance lives.
func spotReq(id, instID string, state ec2types.SpotInstanceState, extra ...string) ec2types.SpotInstanceRequest {
	kv := append([]string{"spawn:managed", "true", "spawn:ttl-deadline", rfc(sweepNow.Add(-time.Hour))}, extra...)
	created := sweepNow.Add(-2 * time.Hour)
	return ec2types.SpotInstanceRequest{
		SpotInstanceRequestId: aws.String(id),
		InstanceId:            aws.String(instID),
		State:                 state,
		Type:                  ec2types.SpotInstanceTypePersistent,
		CreateTime:            &created,
		Tags:                  tags(kv...),
	}
}

func inst(id string, state ec2types.InstanceStateName, kv ...string) ec2types.Instance {
	launched := sweepNow.Add(-2 * time.Hour)
	return ec2types.Instance{
		InstanceId: aws.String(id),
		State:      &ec2types.InstanceState{Name: state},
		LaunchTime: &launched,
		Tags:       tags(kv...),
	}
}

// managedLive is an original, spawn-managed instance whose OWN deadline was
// extended into the future.
func managedLive(id string, state ec2types.InstanceStateName) ec2types.Instance {
	return inst(id, state, "spawn:managed", "true", "spawn:launch-time", rfc(sweepNow.Add(-2*time.Hour)),
		"spawn:ttl-deadline", rfc(sweepNow.Add(5*time.Hour)))
}

func sweepReaper(f *fakeSpotEC2, mode string) *reaper {
	return &reaper{
		accounts: []account{{
			label: "acct", accountID: "111111111111",
			ec2For:  func(string) ec2API { return emptyEC2{} },
			spotFor: func(string) spotRequestAPI { return f },
		}},
		regions:      []string{"us-east-1"},
		maxAge:       defaultMaxAge,
		spotRequests: mode,
	}
}

func sweep(t *testing.T, f *fakeSpotEC2, mode string) (Summary, string) {
	t.Helper()
	r := sweepReaper(f, mode)
	var sum Summary
	out := captureLogs(func() {
		o := accountOutcome{label: "acct"}
		r.reapSpotRequestsRegion(context.Background(), r.accounts[0], "us-east-1", sweepNow, &sum, &o)
	})
	return sum, out
}

// #950: the issue's own cases. Each request is spawn-tagged and persistent.
func TestSpotSweep_Decisions(t *testing.T) {
	cases := []struct {
		name          string
		req           ec2types.SpotInstanceRequest
		insts         []ec2types.Instance
		wantCalls     string // cancel/terminate calls only, in order
		wantCancelled int
		wantTerm      int
	}{
		{
			name:      "instance terminated from the console: cancel",
			req:       spotReq("sir-1", "i-1", ec2types.SpotInstanceStateOpen),
			insts:     []ec2types.Instance{managedLive("i-1", ec2types.InstanceStateNameTerminated)},
			wantCalls: "cancel:sir-1", wantCancelled: 1,
		},
		{
			name:      "instance no longer known to EC2: cancel",
			req:       spotReq("sir-1", "i-gone", ec2types.SpotInstanceStateActive),
			wantCalls: "cancel:sir-1", wantCancelled: 1,
		},
		{
			name:      "open with no instance, past the grace: cancel",
			req:       spotReq("sir-1", "", ec2types.SpotInstanceStateOpen),
			wantCalls: "cancel:sir-1", wantCancelled: 1,
		},
		{
			name:      "untagged replacement: cancel the request, THEN terminate the instance",
			req:       spotReq("sir-1", "i-repl", ec2types.SpotInstanceStateActive),
			insts:     []ec2types.Instance{inst("i-repl", ec2types.InstanceStateNameRunning)},
			wantCalls: "cancel:sir-1 terminate:i-repl", wantCancelled: 1, wantTerm: 1,
		},
		{
			name: "tagged replacement (inherited tags): detected by spawn:original-instance-id",
			req:  spotReq("sir-1", "i-repl", ec2types.SpotInstanceStateActive, "spawn:original-instance-id", "i-orig"),
			insts: []ec2types.Instance{
				managedLive("i-repl", ec2types.InstanceStateNameRunning),
			},
			wantCalls: "cancel:sir-1 terminate:i-repl", wantCancelled: 1, wantTerm: 1,
		},
		{
			name: "replacement whose request is already cancelled: terminate only",
			req:  spotReq("sir-1", "i-repl", ec2types.SpotInstanceStateCancelled),
			insts: []ec2types.Instance{
				inst("i-repl", ec2types.InstanceStateNameRunning),
			},
			wantCalls: "terminate:i-repl", wantTerm: 1,
		},
		{
			// The request's own deadline is in the past (spotReq), but `spawn
			// extend` moved the instance's. Cancelling here would strand the
			// instance the next time it stops.
			name:  "live original, extended past the request's stale deadline: leave it",
			req:   spotReq("sir-1", "i-1", ec2types.SpotInstanceStateDisabled, "spawn:original-instance-id", "i-1"),
			insts: []ec2types.Instance{managedLive("i-1", ec2types.InstanceStateNameStopped)},
		},
		{
			name: "live original past its OWN deadline: cancel (the instance scan terminates it)",
			req:  spotReq("sir-1", "i-1", ec2types.SpotInstanceStateActive, "spawn:original-instance-id", "i-1"),
			insts: []ec2types.Instance{inst("i-1", ec2types.InstanceStateNameRunning, "spawn:managed", "true",
				"spawn:ttl-deadline", rfc(sweepNow.Add(-time.Minute)))},
			wantCalls: "cancel:sir-1", wantCancelled: 1,
		},
		{
			name:  "cancelled request, its managed instance alive: the instance scan's job",
			req:   spotReq("sir-1", "i-1", ec2types.SpotInstanceStateCancelled),
			insts: []ec2types.Instance{managedLive("i-1", ec2types.InstanceStateNameRunning)},
		},
		{
			name:  "cancelled request, instance terminated: done",
			req:   spotReq("sir-1", "i-1", ec2types.SpotInstanceStateCancelled),
			insts: []ec2types.Instance{managedLive("i-1", ec2types.InstanceStateNameTerminated)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeSpotEC2{reqs: []ec2types.SpotInstanceRequest{tc.req}, insts: tc.insts}
			sum, out := sweep(t, f, spotRequestsReap)
			var acted []string
			for _, c := range f.calls {
				if strings.HasPrefix(c, "cancel:") || strings.HasPrefix(c, "terminate:") {
					acted = append(acted, c)
				}
			}
			if got := strings.Join(acted, " "); got != tc.wantCalls {
				t.Errorf("calls = %q, want %q\nlog:\n%s", got, tc.wantCalls, out)
			}
			if sum.SpotRequestsCancelled != tc.wantCancelled || sum.SpotReplacementsTerminated != tc.wantTerm {
				t.Errorf("cancelled=%d terminated=%d, want %d/%d", sum.SpotRequestsCancelled, sum.SpotReplacementsTerminated, tc.wantCancelled, tc.wantTerm)
			}
			if sum.SpotRequestsScanned != 1 {
				t.Errorf("scanned = %d, want 1", sum.SpotRequestsScanned)
			}
		})
	}
}

// A request created moments ago may not show its instance yet. It is not an
// orphan.
func TestSpotSweep_OpenWithinGraceIsLeftAlone(t *testing.T) {
	req := spotReq("sir-1", "", ec2types.SpotInstanceStateOpen)
	created := sweepNow.Add(-time.Minute)
	req.CreateTime = &created
	f := &fakeSpotEC2{reqs: []ec2types.SpotInstanceRequest{req}}
	if sum, _ := sweep(t, f, spotRequestsReap); len(f.cancelled) != 0 || sum.SpotRequestsCancelled != 0 {
		t.Errorf("cancelled a request inside its grace: %v", f.calls)
	}
}

// The sweep asks for what the issue specifies: spawn-tagged, persistent, and in
// a state that can still launch (plus cancelled, to keep a replacement in view).
func TestSpotSweep_DescribeFilters(t *testing.T) {
	f := &fakeSpotEC2{}
	sweep(t, f, spotRequestsReap)
	got := map[string]string{}
	for _, flt := range f.reqFilters {
		got[aws.ToString(flt.Name)] = strings.Join(flt.Values, ",")
	}
	want := map[string]string{
		"tag:spawn:managed": "true",
		"type":              "persistent",
		"state":             "open,active,disabled,cancelled",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("filter %s = %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
}

// A request whose instance EC2 no longer knows is the case the sweep exists
// for. Looking it up with InstanceIds would fail the whole call on that ID
// (InvalidInstanceID.NotFound), so the lookup must be a filter.
func TestSpotSweep_InstanceLookupIsAFilterNotInstanceIds(t *testing.T) {
	f := &fakeSpotEC2{reqs: []ec2types.SpotInstanceRequest{spotReq("sir-1", "i-gone", ec2types.SpotInstanceStateActive)}}
	sweep(t, f, spotRequestsReap)
	if len(f.instInput) == 0 {
		t.Fatal("no DescribeInstances")
	}
	for _, in := range f.instInput {
		if len(in.InstanceIds) != 0 {
			t.Errorf("DescribeInstances used InstanceIds %v; one missing ID fails the whole call", in.InstanceIds)
		}
	}
}

// Report mode and dry run change nothing: production runs DryRun=false, so the
// new sweep arrives inert and says what it would do.
func TestSpotSweep_ReportAndDryRunChangeNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mode   string
		dryRun bool
	}{{"report", spotRequestsReport, false}, {"reap under dry-run", spotRequestsReap, true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeSpotEC2{
				reqs:  []ec2types.SpotInstanceRequest{spotReq("sir-1", "i-repl", ec2types.SpotInstanceStateActive)},
				insts: []ec2types.Instance{inst("i-repl", ec2types.InstanceStateNameRunning)},
			}
			r := sweepReaper(f, tc.mode)
			r.dryRun = tc.dryRun
			var sum Summary
			out := captureLogs(func() {
				o := accountOutcome{}
				r.reapSpotRequestsRegion(context.Background(), r.accounts[0], "us-east-1", sweepNow, &sum, &o)
			})
			for _, c := range f.calls {
				if strings.HasPrefix(c, "cancel") || strings.HasPrefix(c, "terminate") {
					t.Errorf("%s made a destructive call: %v", tc.name, f.calls)
				}
			}
			if sum.SpotRequestsSkipped != 1 || !strings.Contains(out, "WOULD cancel orphaned spot request sir-1") ||
				!strings.Contains(out, "terminate replacement i-repl") {
				t.Errorf("skipped=%d, log:\n%s", sum.SpotRequestsSkipped, out)
			}
		})
	}
}

func TestSpotSweep_OffMakesNoCalls(t *testing.T) {
	f := &fakeSpotEC2{}
	sweep(t, f, spotRequestsOff)
	if len(f.calls) != 0 {
		t.Errorf("off made calls: %v", f.calls)
	}
}

// Never terminate with the request still open: the terminate would reopen it
// and EC2 would launch another replacement.
func TestSpotSweep_CancelFailureBlocksTheTerminate(t *testing.T) {
	f := &fakeSpotEC2{
		reqs:      []ec2types.SpotInstanceRequest{spotReq("sir-1", "i-repl", ec2types.SpotInstanceStateActive)},
		insts:     []ec2types.Instance{inst("i-repl", ec2types.InstanceStateNameRunning)},
		cancelErr: apiErr("RequestLimitExceeded"),
	}
	sum, _ := sweep(t, f, spotRequestsReap)
	for _, c := range f.calls {
		if strings.HasPrefix(c, "terminate") {
			t.Errorf("terminated with the request uncancelled: %v", f.calls)
		}
	}
	if sum.Errors != 1 {
		t.Errorf("errors = %d, want 1", sum.Errors)
	}
}

// An untagged replacement is outside the tag-scoped terminate grant. The
// request is still cancelled, and the instance is reported loudly, every run,
// with the command that ends it. A denial is not an operational error (#469).
func TestSpotSweep_UnterminatableReplacementIsReported(t *testing.T) {
	f := &fakeSpotEC2{
		reqs:         []ec2types.SpotInstanceRequest{spotReq("sir-1", "i-repl", ec2types.SpotInstanceStateActive)},
		insts:        []ec2types.Instance{inst("i-repl", ec2types.InstanceStateNameRunning)},
		terminateErr: apiErr("UnauthorizedOperation"),
	}
	sum, out := sweep(t, f, spotRequestsReap)
	if len(f.cancelled) != 1 {
		t.Errorf("the request must still be cancelled: %v", f.calls)
	}
	if sum.SpotReplacementsStranded != 1 || sum.SpotReplacementsTerminated != 0 || sum.Errors != 0 {
		t.Errorf("stranded=%d terminated=%d errors=%d, want 1/0/0", sum.SpotReplacementsStranded, sum.SpotReplacementsTerminated, sum.Errors)
	}
	for _, want := range []string{sentinelSpotReplacementStranded, "i-repl", "sir-1",
		"aws ec2 terminate-instances --region us-east-1 --instance-ids i-repl"} {
		if !strings.Contains(out, want) {
			t.Errorf("log should contain %q:\n%s", want, out)
		}
	}

	// Next run: the request is cancelled, the replacement still runs. Still
	// reported.
	f2 := &fakeSpotEC2{
		reqs:         []ec2types.SpotInstanceRequest{spotReq("sir-1", "i-repl", ec2types.SpotInstanceStateCancelled)},
		insts:        []ec2types.Instance{inst("i-repl", ec2types.InstanceStateNameRunning)},
		terminateErr: apiErr("UnauthorizedOperation"),
	}
	if sum, out := sweep(t, f2, spotRequestsReap); sum.SpotReplacementsStranded != 1 || !strings.Contains(out, sentinelSpotReplacementStranded) {
		t.Errorf("a cancelled request's live replacement must keep being reported:\n%s", out)
	}
}

// Without the instance lookup every request would look orphaned, so a failed
// lookup acts on none.
type failingInstancesSpotEC2 struct{ fakeSpotEC2 }

func (f *failingInstancesSpotEC2) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	return nil, apiErr("RequestLimitExceeded")
}

func TestSpotSweep_InstanceLookupFailureActsOnNothing(t *testing.T) {
	f := &failingInstancesSpotEC2{fakeSpotEC2{reqs: []ec2types.SpotInstanceRequest{spotReq("sir-1", "i-1", ec2types.SpotInstanceStateActive)}}}
	r := sweepReaper(nil, spotRequestsReap)
	r.accounts[0].spotFor = func(string) spotRequestAPI { return f }
	var sum Summary
	captureLogs(func() {
		o := accountOutcome{}
		r.reapSpotRequestsRegion(context.Background(), r.accounts[0], "us-east-1", sweepNow, &sum, &o)
	})
	if len(f.cancelled) != 0 || sum.Errors != 1 {
		t.Errorf("cancelled %v with errors=%d; a failed lookup must act on nothing", f.cancelled, sum.Errors)
	}
}

// Through run(): a role without ec2:DescribeSpotInstanceRequests (a
// spawn-ttl-reaper-ec2 stack not yet redeployed) is one denied account, with its
// own alarmable sentinel, not an operational error per region (#469, #212).
func TestRun_SpotDescribeDeniedIsOneAccountWithASentinel(t *testing.T) {
	f := &fakeSpotEC2{describeErr: apiErr("UnauthorizedOperation")}
	r := sweepReaper(f, spotRequestsReport)
	r.regions = []string{"us-east-1", "us-west-2", "eu-west-1"}
	var sum Summary
	var err error
	out := captureLogs(func() { sum, err = r.run(context.Background()) })
	if err != nil {
		t.Fatal(err)
	}
	if sum.SpotAccountsDenied != 1 || sum.Errors != 0 {
		t.Errorf("SpotAccountsDenied=%d Errors=%d, want 1/0", sum.SpotAccountsDenied, sum.Errors)
	}
	if strings.Count(out, sentinelSpotRequestsDenied) != 1 {
		t.Errorf("want the sentinel exactly once:\n%s", out)
	}
	if f.describeReqs != 3 {
		t.Errorf("described in %d regions, want 3", f.describeReqs)
	}
}

// Through run(): the sweep runs in every region after the instance scan, and
// its result lands in the Summary the Lambda returns.
func TestRun_SpotSweepRunsAndCounts(t *testing.T) {
	f := &fakeSpotEC2{
		reqs:  []ec2types.SpotInstanceRequest{spotReq("sir-1", "i-gone", ec2types.SpotInstanceStateActive)},
		insts: nil,
	}
	var sum Summary
	var err error
	captureLogs(func() { sum, err = sweepReaper(f, spotRequestsReap).run(context.Background()) })
	if err != nil {
		t.Fatal(err)
	}
	if sum.SpotRequestsCancelled != 1 || len(f.cancelled) != 1 || f.cancelled[0] != "sir-1" {
		t.Errorf("cancelled %v (summary %d), want [sir-1]", f.cancelled, sum.SpotRequestsCancelled)
	}
}

func TestParseSpotRequests(t *testing.T) {
	for in, want := range map[string]string{
		"": spotRequestsReport, "report": spotRequestsReport, "REAP": spotRequestsReap, " off ": spotRequestsOff,
		"reaap": spotRequestsReport, "true": spotRequestsReport,
	} {
		if got := parseSpotRequests(in); got != want {
			t.Errorf("parseSpotRequests(%q) = %q, want %q", in, got, want)
		}
	}
}
