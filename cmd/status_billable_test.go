package cmd

import (
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
)

func billableInstance(state string, tags map[string]string) *aws.InstanceInfo {
	return &aws.InstanceInfo{
		InstanceID:   "i-abc",
		InstanceType: "c5.xlarge",
		State:        state,
		Tags:         tags,
	}
}

// TestBillableResourcesNamesTheFSxNobodyWasWatching is spawn#615's core case. In
// #613 the filesystem was the most expensive thing in the launch and the least
// visible thing in the tooling — `spawn status` reported the instance and nothing
// else, so the one surface a user checks to answer "what am I paying for" was silent
// about the largest line item.
func TestBillableResourcesNamesTheFSxNobodyWasWatching(t *testing.T) {
	out := renderBillableResources(billableInstance("running", map[string]string{
		"spawn:managed":         "true",
		"spawn:price-per-hour":  "0.170000",
		"spawn:ebs-hourly-cost": "0.002192",
		"spawn:fsx-id":          "fs-0d0d29cb9e4be40df",
		"spawn:fsx-lifecycle":   "ephemeral",
	}))

	for _, want := range []string{
		"fs-0d0d29cb9e4be40df", // the filesystem, by id
		"ephemeral",            // its lifecycle, which decides who deletes it
		"$174",                 // the cost floor — 1200 GiB minimum
		"c5.xlarge",            // still shows the instance
		// #616 made the cap a total, so the old "caps COMPUTE only" claim became
		// false. What a user still needs told is the cap's REACH: it counts these
		// rows, but cannot reclaim what outlives the instance.
		"cannot RECLAIM storage that outlives the instance",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("billable view missing %q:\n%s", want, out)
		}
	}
}

// TestBillableResourcesFlagsStoppedEBS: "stopped to save money" while still paying
// for storage is the quiet surprise, and spored cannot act once the instance is
// stopped — which is exactly how an instance sat 13 days past its TTL still billing.
func TestBillableResourcesFlagsStoppedEBS(t *testing.T) {
	out := renderBillableResources(billableInstance("stopped", map[string]string{
		"spawn:managed":         "true",
		"spawn:price-per-hour":  "0.680000",
		"spawn:ebs-hourly-cost": "0.002192",
	}))

	if !strings.Contains(out, "STILL BILLING") {
		t.Errorf("a stopped instance's EBS must be called out as still billing:\n%s", out)
	}
	if !strings.Contains(out, "compute not billing") {
		t.Errorf("the compute row should say it is NOT billing while stopped, so the contrast is clear:\n%s", out)
	}
}

// TestBillableResourcesRateUnknownIsNotZero: an unpriced instance must not render as
// free. A fabricated or zeroed rate is how #533's cap silently stopped being
// enforceable.
func TestBillableResourcesRateUnknownIsNotZero(t *testing.T) {
	res := billableResources(billableInstance("running", map[string]string{
		"spawn:managed":         "true",
		"spawn:ebs-hourly-cost": "0.002192",
	}))
	if len(res) == 0 {
		t.Fatal("expected resources")
	}
	if res[0].Kind != "instance" {
		t.Fatalf("first row should be the instance, got %q", res[0].Kind)
	}
	if res[0].USD != "rate unknown" {
		t.Errorf("an unpriced instance must say so, not imply $0: %q", res[0].USD)
	}
	if strings.Contains(res[0].USD, "0.0000") {
		t.Errorf("must not render an unknown rate as zero: %q", res[0].USD)
	}
}

// TestBillableResourcesPendingFSxCountsToo: the #613 failure happened while the
// filesystem was still CREATING, held by the spawn:fsx-pending lease. A view that
// only counted mounted filesystems would have missed it.
func TestBillableResourcesPendingFSxCountsToo(t *testing.T) {
	out := renderBillableResources(billableInstance("running", map[string]string{
		"spawn:managed":        "true",
		"spawn:price-per-hour": "0.170000",
		"spawn:fsx-pending":    "fs-pending1",
	}))
	if !strings.Contains(out, "fs-pending1") {
		t.Errorf("a provisioning filesystem is already billing and must appear:\n%s", out)
	}
	if !strings.Contains(out, "still provisioning") {
		t.Errorf("it should be labelled as provisioning rather than presented as ready:\n%s", out)
	}
}

func TestBillableResourcesIncludesEFS(t *testing.T) {
	out := renderBillableResources(billableInstance("running", map[string]string{
		"spawn:managed":        "true",
		"spawn:price-per-hour": "0.170000",
		"spawn:efs-id":         "fs-efs42",
	}))
	if !strings.Contains(out, "fs-efs42") {
		t.Errorf("EFS must appear:\n%s", out)
	}
	// spawn cannot know the stored size, so it must say that rather than imply free.
	if !strings.Contains(out, "per GiB stored") {
		t.Errorf("EFS billing basis should be stated, not guessed:\n%s", out)
	}
}

// TestBillableResourcesSilentWhenOnlyCompute: the block exists for the resources
// BESIDES the instance. `spawn status` already reports the instance, so a
// compute-only launch must not grow a redundant section — noise is what made the
// old reaper warning useless.
func TestBillableResourcesSilentWhenOnlyCompute(t *testing.T) {
	out := renderBillableResources(billableInstance("running", map[string]string{
		"spawn:managed":        "true",
		"spawn:price-per-hour": "0.170000",
	}))
	if out != "" {
		t.Errorf("expected no block when only compute is billable, got:\n%s", out)
	}
}

func TestBillableResourcesSilentWithoutTags(t *testing.T) {
	if got := renderBillableResources(&aws.InstanceInfo{InstanceID: "i-x"}); got != "" {
		t.Errorf("expected no block for an untagged instance, got:\n%s", got)
	}
	if got := renderBillableResources(nil); got != "" {
		t.Errorf("expected no block for a nil instance, got:\n%s", got)
	}
}

// TestBillableResourcesMarksWhatOutlivesTheInstance: the distinction that matters
// operationally is not "expensive" but "survives termination".
func TestBillableResourcesMarksWhatOutlivesTheInstance(t *testing.T) {
	res := billableResources(billableInstance("running", map[string]string{
		"spawn:managed":        "true",
		"spawn:price-per-hour": "0.170000",
		"spawn:fsx-id":         "fs-1",
		"spawn:efs-id":         "fs-2",
	}))

	byKind := map[string]billableResource{}
	for _, r := range res {
		byKind[r.Kind] = r
	}
	if !byKind["fsx"].OutlivesInstance {
		t.Error("FSx outlives the instance by design")
	}
	if !byKind["efs"].OutlivesInstance {
		t.Error("EFS outlives the instance")
	}
	if byKind["instance"].OutlivesInstance {
		t.Error("compute does not outlive the instance")
	}
	if !byKind["instance"].Capped {
		t.Error("compute is the one thing --cost-limit governs")
	}
	if byKind["fsx"].Capped || byKind["efs"].Capped {
		t.Error("storage is NOT covered by --cost-limit; claiming otherwise is the #616 confusion")
	}
}

func TestParseTagFloat(t *testing.T) {
	cases := map[string]float64{
		"0.170000": 0.17,
		" 0.5 ":    0.5,
		"":         0,
		"abc":      0,
		"-1":       0, // a negative rate is nonsense; treat as unknown
	}
	for in, want := range cases {
		if got := parseTagFloat(in); got != want {
			t.Errorf("parseTagFloat(%q) = %v, want %v", in, got, want)
		}
	}
}
