package cmd

// The raise arithmetic below is what --raise-cost-limit writes; the default
// keeps the cap (#1262).

import (
	"strconv"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
)

// restartedInstance is the #992 shape: launched 10h ago, ran 6h, was stopped,
// and was started again 1h ago. EC2's LaunchTime is the latest start; the
// original launch is only in spawn:launch-time, and spored has counted 6h of
// earlier compute plus this boot's hour in spawn:compute-seconds.
func restartedInstance(now time.Time, extra map[string]string) *aws.InstanceInfo {
	launch := now.Add(-10 * time.Hour)
	tags := map[string]string{
		"spawn:launch-time":     launch.UTC().Format(time.RFC3339),
		"spawn:ttl-deadline":    launch.Add(12 * time.Hour).UTC().Format(time.RFC3339),
		"spawn:compute-seconds": strconv.Itoa(int((7 * time.Hour).Seconds())),
	}
	for k, v := range extra {
		tags[k] = v
	}
	return &aws.InstanceInfo{
		InstanceID: "i-restarted",
		State:      "running",
		LaunchTime: now.Add(-1 * time.Hour), // reset by the start
		TTL:        "12h",
		Tags:       tags,
	}
}

// (1) spawn:ttl is the total from the ORIGINAL launch, not from the latest
// start (#506's meaning, #992's bug).
func TestExtendAnchorsSpawnTTLToTheOriginalLaunch(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	inst := restartedInstance(now, nil)

	tags, deadline := computeExtendedTTLTags(inst, "4h", 4*time.Hour, now)

	launch := now.Add(-10 * time.Hour)
	if want := launch.Add(16 * time.Hour); !deadline.Equal(want) {
		t.Fatalf("deadline = %v, want %v (the old deadline + 4h)", deadline, want)
	}
	if got, want := tags["spawn:ttl"], (16 * time.Hour).String(); got != want {
		t.Errorf("spawn:ttl = %q, want %q: the total from the original launch, not from the latest start "+
			"(EC2 LaunchTime, which would give 7h)", got, want)
	}
}

// (2) the raised cap must cover the cost the agent will actually have counted
// by the new deadline: compute from every boot plus running until then, and
// EBS from the original launch. Anchoring to the latest start priced only
// 1h + 6h here and left out the first boot's 6h, so the "raised" cap still
// fired hours before the deadline (#639 again).
func TestExtendCostLimitCountsComputeFromEveryBoot(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	inst := restartedInstance(now, map[string]string{
		"spawn:cost-limit":      "5.0000",
		"spawn:price-per-hour":  "1.000000",
		"spawn:ebs-hourly-cost": "0.100000",
	})
	newDeadline := now.Add(6 * time.Hour) // original launch + 16h

	d := decideCostLimit(inst, newDeadline, now, 0, true)

	// What the agent will have counted at the new deadline if the instance runs
	// until then: compute (7h so far + 6h more) at $1, EBS over 16h of wall
	// clock at $0.10.
	enforcedAtDeadline := 1.0*(7+6) + 0.1*16
	if !d.Changed() || d.NewValue < enforcedAtDeadline {
		t.Fatalf("cap = $%.4f, but the agent will have counted $%.4f by the new deadline, so the cap "+
			"fires first", d.NewValue, enforcedAtDeadline)
	}
	// And not wildly more: the slack is the compute-tag lag, at most.
	if slack := d.NewValue - enforcedAtDeadline; slack > computeTagLag.Hours()*1.0+1e-9 {
		t.Errorf("cap = $%.4f, $%.4f over what is needed; more than the compute-tag lag allows", d.NewValue, slack)
	}
}

// A stopped instance's earlier compute must not be forgotten either, and the
// time it spent stopped is not compute: an instance launched 20h ago that ran
// 2h and has been stopped since needs 2h + the time to the deadline, not 20h+.
func TestExtendCostLimitDoesNotBillStoppedTimeAsCompute(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	launch := now.Add(-20 * time.Hour)
	inst := &aws.InstanceInfo{
		InstanceID: "i-stopped",
		State:      "stopped",
		LaunchTime: launch.Add(time.Hour), // a later start
		Tags: map[string]string{
			"spawn:launch-time":     launch.UTC().Format(time.RFC3339),
			"spawn:compute-seconds": strconv.Itoa(int((2 * time.Hour).Seconds())),
			"spawn:cost-limit":      "1.0000",
			"spawn:price-per-hour":  "1.000000",
		},
	}
	d := decideCostLimit(inst, now.Add(4*time.Hour), now, 0, true)
	want := 1.0 * (2 + 4)
	if d.NewValue < want || d.NewValue > want+computeTagLag.Hours() {
		t.Errorf("cap = $%.4f, want ~$%.2f (2h of compute so far + 4h to the deadline)", d.NewValue, want)
	}
}

// Without spawn:compute-seconds the most the instance can have run is the wall
// clock since its ORIGINAL launch; that bound keeps the cap from firing early.
func TestExtendCostLimitWithoutComputeTagUsesWallClockFromOriginalLaunch(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	launch := now.Add(-10 * time.Hour)
	inst := &aws.InstanceInfo{
		LaunchTime: now.Add(-time.Hour), // reset by a start
		Tags: map[string]string{
			"spawn:launch-time":    launch.UTC().Format(time.RFC3339),
			"spawn:cost-limit":     "1.0000",
			"spawn:price-per-hour": "1.000000",
		},
	}
	d := decideCostLimit(inst, now.Add(2*time.Hour), now, 0, true)
	if want := 12.0; d.NewValue < want-1e-9 {
		t.Errorf("cap = $%.4f, want >= $%.2f (12h from the original launch); anchoring to the latest start gives $3", d.NewValue, want)
	}
}

func TestOriginalLaunchTime(t *testing.T) {
	ec2 := time.Date(2026, 10, 15, 11, 0, 0, 0, time.UTC)
	tagged := time.Date(2026, 10, 15, 2, 0, 0, 0, time.UTC)
	if got := originalLaunchTime(&aws.InstanceInfo{LaunchTime: ec2, Tags: map[string]string{"spawn:launch-time": tagged.Format(time.RFC3339)}}); !got.Equal(tagged) {
		t.Errorf("with the tag: %v, want %v", got, tagged)
	}
	if got := originalLaunchTime(&aws.InstanceInfo{LaunchTime: ec2, Tags: map[string]string{"spawn:launch-time": "garbage"}}); !got.Equal(ec2) {
		t.Errorf("unparseable tag: %v, want the EC2 fallback %v", got, ec2)
	}
	if got := originalLaunchTime(&aws.InstanceInfo{LaunchTime: ec2}); !got.Equal(ec2) {
		t.Errorf("no tag: %v, want the EC2 fallback %v", got, ec2)
	}
}

// The job-array path goes through the same extensionTags (#983), so it gets
// the same anchors.
func TestJobArrayExtendUsesTheOriginalLaunchToo(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	inst := restartedInstance(now, map[string]string{"spawn:cost-limit": "5.0000", "spawn:price-per-hour": "1.000000"})
	tags, _, d := extensionTags(inst, "4h", 4*time.Hour, now, 0, true)
	if tags["spawn:ttl"] != (16 * time.Hour).String() {
		t.Errorf("spawn:ttl = %q, want 16h0m0s", tags["spawn:ttl"])
	}
	if d.NewValue < 13 { // 7h so far + 6h to the deadline at $1
		t.Errorf("cap = $%.4f, want >= $13", d.NewValue)
	}
}
