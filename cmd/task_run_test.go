package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/taskproto"
)

type fakeTaskFinder struct{ cands []taskproto.Candidate }

func (f fakeTaskFinder) FindCandidates(context.Context, taskproto.ResourceRequest) ([]taskproto.Candidate, error) {
	return f.cands, nil
}

func TestRenderTaskDryRun(t *testing.T) {
	spec := &taskproto.TaskSpec{
		TaskID:    "align-42",
		Command:   []string{"bwa", "mem", "ref.fa"},
		Resources: taskproto.ResourceRequest{CPU: 16, MemoryGiB: 32},
		Inputs:    []taskproto.Manifest{{Source: "s3://b/in.fq", Destination: "/work/in.fq"}},
		Lifecycle: taskproto.Lifecycle{TTL: "4h", OnComplete: "terminate"},
	}
	finder := fakeTaskFinder{cands: []taskproto.Candidate{
		{InstanceType: "c7a.4xlarge", Family: "c7a", VCPUs: 16, MemoryGiB: 32, OnDemandPrice: 0.62},
		{InstanceType: "c7i.4xlarge", Family: "c7i", VCPUs: 16, MemoryGiB: 32, OnDemandPrice: 0.71},
	}}

	var buf bytes.Buffer
	if err := renderTaskDryRun(context.Background(), &buf, spec, finder, "us-east-1"); err != nil {
		t.Fatalf("renderTaskDryRun: %v", err)
	}
	got := buf.String()

	wants := []string{
		"DRY RUN — nothing will be launched.",
		"Task:         align-42",
		"bwa mem ref.fa",
		"c7a.4xlarge",          // cheapest of the two
		"Max cost:     ~$2.48", // 0.62 × 4h
		"s3://b/in.fq → /work/in.fq",
		"not implemented yet (spawn#386)",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("dry-run output missing %q\n--- output ---\n%s", w, got)
		}
	}
	// Must never claim to have launched.
	if strings.Contains(got, "Instance i-") || strings.Contains(strings.ToLower(got), "launched instance") {
		t.Errorf("dry-run must not launch anything:\n%s", got)
	}
}
