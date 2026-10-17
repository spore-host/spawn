package cmd

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
)

// The instance from #928: an on-demand c8a.large ($0.1078/hr) in us-west-2,
// launched 17:42 UTC with a 1h TTL, stopped about a minute later.
func stoppedIssueInstance(now time.Time) *aws.InstanceInfo {
	launch := time.Date(2026, 10, 15, 17, 42, 0, 0, time.UTC)
	return &aws.InstanceInfo{
		InstanceID: "i-0abc928", Name: "odtest", InstanceType: "c8a.large",
		State: "stopped", Region: "us-west-2",
		// EC2's LaunchTime is the LAST start; the tag is the original launch.
		LaunchTime:            launch,
		StateTransitionReason: "User initiated (2026-10-15 17:43:10 GMT)",
		StateReason:           "Client.UserInitiatedShutdown: User initiated shutdown",
		Tags: map[string]string{
			"spawn:managed":          "true",
			"spawn:launch-time":      launch.Format(time.RFC3339),
			"spawn:ttl-deadline":     launch.Add(time.Hour).Format(time.RFC3339),
			"spawn:price-per-hour":   "0.107800",
			"spawn:ebs-hourly-cost":  "0.001100",
			"spawn:compute-seconds":  "62",
			"spawn:last-stop-reason": "user-stopped",
		},
	}
}

// `spawn status` must not reach a non-running instance at all (#928): it SSHed
// to the empty host a stopped instance leaves, which resolved to this machine.
// Only a running instance has a spored to ask.
func TestStatus_OnlyARunningInstanceIsAsked(t *testing.T) {
	for _, state := range []string{"stopped", "stopping", "pending", "shutting-down", "terminated", ""} {
		if statusNeedsInstance(&aws.InstanceInfo{State: state, PublicIP: "54.1.2.3"}) {
			t.Errorf("state %q: status would SSH/SSM to the instance", state)
		}
	}
	if !statusNeedsInstance(&aws.InstanceInfo{State: "running"}) {
		t.Error("a running instance is not asked")
	}
	if statusNeedsInstance(nil) {
		t.Error("nil instance is asked")
	}
}

// The issue's expected stopped-instance view, from EC2 data and tags: state,
// stopped-since, TTL deadline, cumulative compute and cost so far, and the EBS
// cost accruing while stopped.
func TestStatus_StoppedInstanceRendersFromEC2(t *testing.T) {
	denver, err := time.LoadLocation("America/Denver")
	if err != nil {
		t.Skip("no tzdata")
	}
	now := time.Date(2026, 10, 15, 17, 44, 10, 0, time.UTC)
	inst := stoppedIssueInstance(now)
	rate := tagStatusRate(inst)
	r := buildEC2StatusReport(inst, rate, now)

	if r.StoppedSince == nil || !r.StoppedSince.Equal(time.Date(2026, 10, 15, 17, 43, 10, 0, time.UTC)) {
		t.Errorf("stopped since = %v, want EC2's transition time 17:43:10", r.StoppedSince)
	}
	if !r.ComputeKnown || r.ComputeSeconds != 62 {
		t.Errorf("compute = %v (known %v), want the 62s checkpoint", r.ComputeSeconds, r.ComputeKnown)
	}
	// $0.1078/hr × 62s, plus EBS × 2m10s since launch.
	if want := 0.1078 * 62 / 3600; !near(r.ComputeCost, want) {
		t.Errorf("compute cost = %v, want %v", r.ComputeCost, want)
	}
	if want := 0.0011 * 130 / 3600; !near(r.StorageCost, want) {
		t.Errorf("storage cost = %v, want EBS over all %v of wall clock (%v)", r.StorageCost, 130, want)
	}
	if !r.CumulativeKnown || !near(r.CumulativeCost, r.ComputeCost+r.StorageCost) {
		t.Errorf("cumulative = %v (known %v)", r.CumulativeCost, r.CumulativeKnown)
	}
	if r.TTLDeadline == nil || r.TTLPastDue {
		t.Fatalf("deadline = %v past=%v", r.TTLDeadline, r.TTLPastDue)
	}

	out := renderEC2Status(r, denver)
	for _, want := range []string{
		"State:            ",
		"stopped",
		// #916's one format: local, with UTC in parentheses.
		"Stopped since:    2026-10-15 11:43 MDT (17:43 UTC)",
		"Client.UserInitiatedShutdown: User initiated shutdown",
		"TTL deadline:     2026-10-15 12:42 MDT (18:42 UTC), in 57m",
		"1m 2s across all boots × $0.1078/hr",
		"STILL BILLING ~$0.0011/hr",
		"during this stop",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stopped status lacks %q:\n%s", want, out)
		}
	}
	// The stopped view must not claim the deadline pauses (#928 part 3).
	if strings.Contains(strings.ToLower(out), "pause") {
		t.Errorf("stopped status says the TTL pauses:\n%s", out)
	}
}

// --output json for a stopped instance is still pure JSON on stdout, marked as
// built from EC2 rather than from spored.
func TestStatus_StoppedInstanceJSON(t *testing.T) {
	now := time.Date(2026, 10, 15, 17, 44, 10, 0, time.UTC)
	inst := stoppedIssueInstance(now)
	out, err := marshalEC2Status(buildEC2StatusReport(inst, tagStatusRate(inst), now))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if doc["source"] != "ec2" || doc["state"] != "stopped" || doc["stopped_since"] == nil || doc["ttl_deadline"] == nil {
		t.Errorf("json = %s", out)
	}
}

// A deadline that passed while stopped must say so, not show a negative
// countdown: starting it grants no fresh time (docs/safety: "starting it does
// not grant fresh time").
func TestStatus_StoppedPastDeadline(t *testing.T) {
	now := time.Date(2026, 10, 15, 19, 0, 0, 0, time.UTC)
	inst := stoppedIssueInstance(now)
	r := buildEC2StatusReport(inst, tagStatusRate(inst), now)
	if !r.TTLPastDue {
		t.Fatal("deadline 18:42 is not past due at 19:00")
	}
	if out := renderEC2Status(r, time.UTC); !strings.Contains(out, "past due") {
		t.Errorf("no past-due line:\n%s", out)
	}
}

// Without the checkpoint tag, compute is shown as unknown, not $0.00 — a zero
// is a claim.
func TestStatus_StoppedWithoutCheckpointIsUnknown(t *testing.T) {
	now := time.Date(2026, 10, 15, 17, 44, 10, 0, time.UTC)
	inst := stoppedIssueInstance(now)
	delete(inst.Tags, "spawn:compute-seconds")
	r := buildEC2StatusReport(inst, tagStatusRate(inst), now)
	if r.ComputeKnown || r.CumulativeKnown || r.EffectiveRate != 0 {
		t.Errorf("report claims a compute figure without a checkpoint: %+v", r)
	}
	if out := renderEC2Status(r, time.UTC); !strings.Contains(out, "Compute:        —") {
		t.Errorf("unknown compute not shown as such:\n%s", out)
	}
}

func near(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}
