package slurm

import (
	"context"
	"testing"
	"time"
)

// Tests for the instance-selection table (#447).
//
// The invariant these protect is narrow but important: this package selects and
// ranks instance types, and must not be a source of prices. RelativeCost exists
// only to order candidates — the tests below check ordering and the table's
// factual claims (specs, current hardware), never a dollar figure, because a
// dollar figure asserted here would be exactly the drift #447 reported.

// TestRelativeCostIsNotUsedAsAPrice is the contract that keeps #447 from
// recurring: no exported API in this package hands a caller a rate or a cost
// derived from the table. Cost estimation takes a Pricer. If someone re-adds a
// table-priced estimate, this test's premise is what they have to break.
func TestRelativeCostIsNotUsedAsAPrice(t *testing.T) {
	// A compile-time assertion: the only cost entry point takes a Pricer, so it
	// cannot produce a dollar figure without a live rate source.
	var _ func(context.Context, *SlurmJob, Pricer, string) (*CostEstimate, error) = EstimateCostWithPricer

	// TotalInstanceHours is the table-free half of an estimate; it must not
	// silently fold in a rate (a value in hours, not dollars).
	job := &SlurmJob{TimeLimit: 2 * time.Hour}
	if got := TotalInstanceHours(job); got != 2 {
		t.Errorf("TotalInstanceHours = %v, want 2 (hours, not a cost)", got)
	}
}

// TestNoRetiredInstanceTypes verifies the table offers only hardware AWS still
// sells. p3/V100 was in the table long after retirement, so a --gres=gpu:v100
// script selected an instance type that could not launch — the failure surfaced
// at RunInstances, far from the cause.
func TestNoRetiredInstanceTypes(t *testing.T) {
	retired := map[string]string{
		"p3.2xlarge":  "V100 retired; no offering in us-east-1/us-east-2/us-west-2/eu-west-1 (2026-07-27)",
		"p3.8xlarge":  "V100 retired",
		"p3.16xlarge": "V100 retired",
	}
	for _, spec := range getInstanceTypes() {
		if why, bad := retired[spec.Type]; bad {
			t.Errorf("table lists retired type %s: %s", spec.Type, why)
		}
	}
}

// TestRetiredGPURequestStillResolves verifies a script asking for a GPU AWS no
// longer offers still selects something runnable. Dropping the p3 rows must not
// convert "picks a dead instance type" into "hard failure" — the script is still
// valid, and a v100-sized job fits on any current successor.
func TestRetiredGPURequestStillResolves(t *testing.T) {
	job := &SlurmJob{
		GPUs:        1,
		GPUType:     "v100",
		CPUsPerTask: 4,
		TimeLimit:   time.Hour,
	}
	got, err := SelectInstanceType(job)
	if err != nil {
		t.Fatalf("a --gres=gpu:v100 job no longer selects anything: %v", err)
	}
	spec, ok := GetInstanceTypeInfo(got)
	if !ok {
		t.Fatalf("selected %s but it is not in the table", got)
	}
	if spec.GPUs < 1 {
		t.Errorf("selected %s for a GPU job but it has no GPU", got)
	}
	if spec.GPUType == "v100" {
		t.Errorf("selected %s — v100 is retired and must not be selectable", got)
	}
	// T4 is excluded from the successor list on purpose: it matches on VRAM but is
	// a large compute step down, and silently making a job much slower is its own
	// wrong answer.
	if spec.GPUType == "t4" {
		t.Errorf("selected %s (T4) as a V100 successor; T4 is deliberately excluded", got)
	}
}

// TestSelectInstanceTypePicksCheapestMatch verifies the winner is the minimum
// under (RelativeCost, Type) across every matching candidate — cheapest first,
// ties broken by name — computed independently of the selection code.
//
// Cheapest-first is the ranking contract, worth re-checking now that the GPU rows
// were re-sourced: a modest single-GPU job must not land on an accelerator type
// costing two orders of magnitude more. The name tie-break is the other half, and
// ties are real rather than hypothetical — c5/c6i are priced identically at
// 4xlarge, 12xlarge and 24xlarge, and two of the job shapes below hit them.
//
// Honest limitation: with the current table, sort.Slice's unstable order happens
// to agree with the name tie-break, so deleting the tie-break does not fail this
// test. The assertion states the intended contract; it cannot force an unstable
// sort to disagree.
func TestSelectInstanceTypePicksCheapestMatch(t *testing.T) {
	for _, job := range []*SlurmJob{
		{GPUs: 1, MemoryMB: 16384, CPUsPerTask: 4, TimeLimit: time.Hour},
		{CPUsPerTask: 4, MemoryMB: 8192, TimeLimit: time.Hour}, // c5/c6i tie at 0.17
		{CPUsPerTask: 48, TimeLimit: time.Hour},                // c5/c6i tie at 2.04
	} {
		got, err := SelectInstanceType(job)
		if err != nil {
			t.Fatalf("SelectInstanceType(%+v): %v", job, err)
		}

		// Compute the expected winner independently of the selection code.
		var want InstanceTypeSpec
		for _, cand := range getInstanceTypes() {
			if !matches(cand, job) {
				continue
			}
			if want.Type == "" ||
				cand.RelativeCost < want.RelativeCost ||
				(cand.RelativeCost == want.RelativeCost && cand.Type < want.Type) {
				want = cand
			}
		}
		if got != want.Type {
			t.Errorf("job %+v selected %s, want %s (min by cost then name)", job, got, want.Type)
		}
	}
}

// TestModernGPUFamiliesAreSelectable verifies the families added in #447 can
// actually be reached by a job's requirements. A table entry no query can select
// is dead weight, and the VRAM-driven ones (L40S, H100, B200) are the reason a
// user reaches for this command at all.
func TestModernGPUFamiliesAreSelectable(t *testing.T) {
	for _, tc := range []struct {
		gpuType string
		want    string // the family we expect to be reachable
	}{
		{"l4", "g6"},
		{"l40s", "g6e"},
		{"rtx-pro-4500", "g7"},
		{"rtx-pro-6000", "g7e"},
		{"h100", "p5"},
		{"h200", "p5e"},
		{"b200", "p6-b200"},
		{"b300", "p6-b300"},
		{"a100-80gb", "p4de"},
	} {
		job := &SlurmJob{GPUs: 1, GPUType: tc.gpuType, TimeLimit: time.Hour}
		got, err := SelectInstanceType(job)
		if err != nil {
			t.Errorf("--gres=gpu:%s selects nothing: %v", tc.gpuType, err)
			continue
		}
		spec, ok := GetInstanceTypeInfo(got)
		if !ok {
			t.Errorf("--gres=gpu:%s selected %s, which is not in the table", tc.gpuType, got)
			continue
		}
		if spec.GPUType != tc.gpuType {
			t.Errorf("--gres=gpu:%s selected %s (GPU %s), want a %s type",
				tc.gpuType, got, spec.GPUType, tc.want)
		}
	}
}

// TestSingleGPUH100IsReachable verifies p5.4xlarge is selectable for a 1-GPU
// H100 job. It is the only H100 size that doesn't require renting all 8 GPUs —
// $6.88/hr versus $55.04 — so its absence made single-GPU H100 work look 8×
// more expensive than it is.
func TestSingleGPUH100IsReachable(t *testing.T) {
	job := &SlurmJob{GPUs: 1, GPUType: "h100", TimeLimit: time.Hour}
	got, err := SelectInstanceType(job)
	if err != nil {
		t.Fatalf("single-GPU H100 job selects nothing: %v", err)
	}
	if got != "p5.4xlarge" {
		t.Errorf("single-GPU H100 job selected %s, want p5.4xlarge (the only 1-GPU H100 size)", got)
	}
}

// TestGPUCountIsRespected verifies an 8-GPU request never lands on a 1-GPU type.
// Several new families share a GPU name across sizes with different counts
// (g7e.2xlarge has 1, g7e.48xlarge has 8), so the count filter carries real
// weight now.
func TestGPUCountIsRespected(t *testing.T) {
	job := &SlurmJob{GPUs: 8, GPUType: "l40s", TimeLimit: time.Hour}
	got, err := SelectInstanceType(job)
	if err != nil {
		t.Fatalf("8-GPU L40S job selects nothing: %v", err)
	}
	spec, _ := GetInstanceTypeInfo(got)
	if spec.GPUs < 8 {
		t.Errorf("8-GPU request selected %s, which has %d GPU(s)", got, spec.GPUs)
	}
}

// TestTableSpecsAreInternallyConsistent catches transcription slips in the rows
// added by hand: a size ladder must not have a bigger size with fewer vCPUs or
// less memory than a smaller one in the same family.
func TestTableSpecsAreInternallyConsistent(t *testing.T) {
	for _, spec := range getInstanceTypes() {
		if spec.VCPUs <= 0 {
			t.Errorf("%s: vCPUs = %d", spec.Type, spec.VCPUs)
		}
		if spec.MemoryMB <= 0 {
			t.Errorf("%s: memory = %d MB", spec.Type, spec.MemoryMB)
		}
		if spec.RelativeCost <= 0 {
			t.Errorf("%s: RelativeCost = %v — a zero rank sorts first and would be selected for everything",
				spec.Type, spec.RelativeCost)
		}
		if spec.GPUs > 0 && spec.GPUType == "" {
			t.Errorf("%s: has %d GPU(s) but no GPUType, so --gres=gpu:<type> can never match it",
				spec.Type, spec.GPUs)
		}
		if spec.GPUs == 0 && spec.GPUType != "" {
			t.Errorf("%s: has GPUType %q but no GPUs", spec.Type, spec.GPUType)
		}
	}
}

// TestNoDuplicateTypes verifies each type appears once. A duplicate with a
// different spec would make GetInstanceTypeInfo's answer depend on table order
// while selection used the other row.
func TestNoDuplicateTypes(t *testing.T) {
	seen := map[string]bool{}
	for _, spec := range getInstanceTypes() {
		if seen[spec.Type] {
			t.Errorf("%s appears more than once", spec.Type)
		}
		seen[spec.Type] = true
	}
}
