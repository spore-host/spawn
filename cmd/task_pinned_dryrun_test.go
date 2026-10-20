package cmd

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/taskproto"
)

// pinnedR8gSpec is the #1253 repro: a pinned r8g.2xlarge, 45m TTL.
func pinnedR8gSpec() *taskproto.TaskSpec {
	return &taskproto.TaskSpec{
		TaskID:    "pinned-r8g",
		Command:   []string{"true"},
		Resources: taskproto.ResourceRequest{InstanceType: "r8g.2xlarge", Purchase: taskproto.PurchaseOnDemand},
		Lifecycle: taskproto.Lifecycle{TTL: "45m", OnComplete: "terminate", CostLimit: 0.50},
	}
}

type failingTaskFinder struct{ err error }

func (f failingTaskFinder) FindCandidates(context.Context, taskproto.ResourceRequest) ([]taskproto.Candidate, error) {
	return nil, f.err
}

// spawn#1253 gate: a pinned instance_type's dry run shows the type's real
// sizes and rate, says the type was pinned rather than "cheapest of 1", and
// keeps the Max cost line.
func TestRenderTaskDryRun_PinnedTypeShowsSizesAndMaxCost(t *testing.T) {
	finder := fakeTaskFinder{cands: []taskproto.Candidate{
		{InstanceType: "r8g.2xlarge", Family: "r8g", VCPUs: 8, MemoryGiB: 64, Architecture: "arm64", OnDemandPrice: 0.4713},
	}}
	var out bytes.Buffer
	if err := renderTaskDryRun(context.Background(), &out, pinnedR8gSpec(), finder, "us-west-2"); err != nil {
		t.Fatalf("renderTaskDryRun: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Instance:     r8g.2xlarge  (8 vCPU, 64 GiB, $0.4713/hr on-demand)",
		"pinned by spec",
		"Max cost:     ~$0.35",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("dry run missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "cheapest of") {
		t.Errorf("a pinned type is described as chosen by price:\n%s", got)
	}
}

// A pinned type whose lookup fails still plans (the pin launches as given),
// and says its sizes are unavailable instead of printing zeros.
func TestRenderTaskDryRun_PinnedLookupFailedSaysUnavailable(t *testing.T) {
	var out bytes.Buffer
	finder := failingTaskFinder{err: errors.New("DescribeInstanceTypes: throttled")}
	if err := renderTaskDryRun(context.Background(), &out, pinnedR8gSpec(), finder, "us-west-2"); err != nil {
		t.Fatalf("a failed pin lookup must not fail the plan: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "r8g.2xlarge  (sizes unavailable") || !strings.Contains(got, "throttled") {
		t.Errorf("failed lookup not reported as sizes unavailable:\n%s", got)
	}
	if regexp.MustCompile(`\b0 vCPU|\b0 GiB`).MatchString(got) {
		t.Errorf("failed lookup rendered as zeros:\n%s", got)
	}
	if !strings.Contains(got, "pinned by spec") {
		t.Errorf("missing pinned wording:\n%s", got)
	}
}
