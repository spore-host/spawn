package cmd

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
)

// #1341: a terminated instance's wall-clock cost (EBS, Elastic IPs) used to
// accrue to now, so `spawn cost` / `spawn status` reported more spend every
// time they ran, though nothing bills after termination.

var (
	termTestLaunch = time.Date(2026, 10, 19, 6, 0, 0, 0, time.UTC)
	termTestEnd    = time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC) // 4h after launch
	termTestNow    = time.Date(2026, 10, 19, 10, 30, 0, 0, time.UTC)
)

// costInstance is an instance whose every cost component is priced: compute
// from spored's checkpoint, EBS, and one auto-assigned + one Elastic IPv4.
func costInstance(state, transitionReason string) *aws.InstanceInfo {
	return &aws.InstanceInfo{
		InstanceID: "i-0term1341", Name: "termbox", InstanceType: "m7i.large",
		State: state, Region: "us-west-2",
		LaunchTime:            termTestLaunch,
		StateTransitionReason: transitionReason,
		Tags: map[string]string{
			"spawn:managed":            "true",
			"spawn:launch-time":        termTestLaunch.Format(time.RFC3339),
			"spawn:price-per-hour":     "0.100000",
			"spawn:ebs-hourly-cost":    "0.010000",
			"spawn:compute-seconds":    "7200",
			"spawn:public-ipv4-count":  "1",
			"spawn:elastic-ipv4-count": "1",
		},
	}
}

// What EC2 actually returns, from a DescribeInstances of a terminated instance.
const realTerminatedReason = "User initiated (2026-10-19 10:00:00 GMT)"

func TestAccrualEnd_ParsesRealStateTransitionReason(t *testing.T) {
	for _, state := range []string{"terminated", "shutting-down"} {
		w := accrualEnd(costInstance(state, realTerminatedReason), termTestNow)
		if !w.Ended || !w.Known || !w.End.Equal(termTestEnd) {
			t.Errorf("%s: accrualEnd = %+v, want Ended, Known, End=%s", state, w, termTestEnd)
		}
	}
	// The reasons EC2 gives once the moment has gone, or never had one.
	for _, reason := range []string{"", "User initiated", "Service.SpotInstanceTermination"} {
		w := accrualEnd(costInstance("terminated", reason), termTestNow)
		if !w.Ended || w.Known {
			t.Errorf("reason %q: accrualEnd = %+v, want Ended and !Known", reason, w)
		}
	}
}

func TestTerminatedCostDoesNotGrow(t *testing.T) {
	later := termTestNow.Add(24 * time.Hour)
	rate := statusRate{PerHour: 0.1, Basis: "on-demand"}
	for _, state := range []string{"terminated", "shutting-down"} {
		inst := costInstance(state, realTerminatedReason)

		a, b := singleInstanceCostReport(inst, termTestNow), singleInstanceCostReport(inst, later)
		if a != b {
			t.Errorf("%s: spawn cost changed a day after termination:\n--- now\n%s\n--- now+1d\n%s", state, a, b)
		}
		// EBS over launch..termination (4h × $0.01), not launch..now.
		if !strings.Contains(a, "Est. storage:    ~$0.04") {
			t.Errorf("%s: storage not bounded at termination:\n%s", state, a)
		}

		ra, rb := buildEC2StatusReport(inst, rate, termTestNow), buildEC2StatusReport(inst, rate, later)
		ja, _ := marshalEC2Status(ra)
		jb, _ := marshalEC2Status(rb)
		if ja != jb {
			t.Errorf("%s: spawn status JSON changed a day after termination:\n%s\n%s", state, ja, jb)
		}
		if ta, tb := renderEC2Status(ra, time.UTC), renderEC2Status(rb, time.UTC); ta != tb {
			t.Errorf("%s: spawn status text changed a day after termination:\n%s\n%s", state, ta, tb)
		}
		if math.Abs(ra.StorageCost-0.04) > 1e-9 || ra.TerminatedAt == nil || !ra.TerminatedAt.Equal(termTestEnd) {
			t.Errorf("%s: status storage = %v, terminated_at = %v; want 0.04 at %s", state, ra.StorageCost, ra.TerminatedAt, termTestEnd)
		}
		// Elastic IP over 4h + auto-assigned over 2h compute, × $0.005.
		if math.Abs(ra.IPv4Cost-0.03) > 1e-9 {
			t.Errorf("%s: status IPv4 = %v, want 0.03", state, ra.IPv4Cost)
		}
	}
}

func TestTerminatedUnknownTimeIsLabelledAndDoesNotGrow(t *testing.T) {
	later := termTestNow.Add(24 * time.Hour)
	rate := statusRate{PerHour: 0.1, Basis: "on-demand"}
	inst := costInstance("terminated", "User initiated")

	a, b := singleInstanceCostReport(inst, termTestNow), singleInstanceCostReport(inst, later)
	if a != b {
		t.Errorf("spawn cost with unknown termination time grew:\n%s\n%s", a, b)
	}
	for _, want := range []string{"Terminated:      time unknown", "Est. storage:    unknown, not counted", "Est. total:      at least ~$"} {
		if !strings.Contains(a, want) {
			t.Errorf("spawn cost missing %q:\n%s", want, a)
		}
	}

	ra, rb := buildEC2StatusReport(inst, rate, termTestNow), buildEC2StatusReport(inst, rate, later)
	ja, _ := marshalEC2Status(ra)
	jb, _ := marshalEC2Status(rb)
	if ja != jb {
		t.Errorf("spawn status JSON with unknown termination time grew:\n%s\n%s", ja, jb)
	}
	if !ra.TerminationTimeUnknown || ra.StorageCost != 0 || ra.CumulativeKnown {
		t.Errorf("status = unknown %v, storage %v, cumulative known %v; want true, 0, false",
			ra.TerminationTimeUnknown, ra.StorageCost, ra.CumulativeKnown)
	}
	text := renderEC2Status(ra, time.UTC)
	if text != renderEC2Status(rb, time.UTC) {
		t.Errorf("spawn status text with unknown termination time changed")
	}
	for _, want := range []string{"Terminated:       time unknown", "Storage (EBS):  — (not counted", "Total:          at least $"} {
		if !strings.Contains(text, want) {
			t.Errorf("spawn status missing %q:\n%s", want, text)
		}
	}
}

// Running and stopped instances still exist and still bill EBS and Elastic IPs
// on the wall clock, so their window still runs to now.
func TestRunningAndStoppedCostStillAccrues(t *testing.T) {
	later := termTestNow.Add(24 * time.Hour)
	rate := statusRate{PerHour: 0.1, Basis: "on-demand"}
	for _, state := range []string{"running", "stopped", "stopping"} {
		// A stopped instance's reason carries a time too; it must not end the window.
		inst := costInstance(state, realTerminatedReason)
		for _, now := range []time.Time{termTestNow, later} {
			rt := runtimeSoFar(inst, now)
			if rt.Since != now.Sub(termTestLaunch) || rt.Window.Ended {
				t.Errorf("%s at %s: Since = %s ended %v, want %s, not ended", state, now, rt.Since, rt.Window.Ended, now.Sub(termTestLaunch))
			}
			r := buildEC2StatusReport(inst, rate, now)
			if want := 0.01 * now.Sub(termTestLaunch).Hours(); math.Abs(r.StorageCost-want) > 1e-9 {
				t.Errorf("%s at %s: status storage = %v, want %v", state, now, r.StorageCost, want)
			}
			if r.TerminatedAt != nil || r.TerminationTimeUnknown {
				t.Errorf("%s: reported as terminated", state)
			}
		}
		if singleInstanceCostReport(inst, termTestNow) == singleInstanceCostReport(inst, later) {
			t.Errorf("%s: spawn cost stopped accruing EBS", state)
		}
	}
}
