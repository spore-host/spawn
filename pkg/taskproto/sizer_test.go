package taskproto

import (
	"context"
	"testing"
)

// fakeFinder returns a fixed candidate list, ignoring the request — the sizer's
// filtering/ranking is what's under test, not the finder.
type fakeFinder struct {
	cands []Candidate
	err   error
}

func (f fakeFinder) FindCandidates(_ context.Context, _ ResourceRequest) ([]Candidate, error) {
	return f.cands, f.err
}

func TestSize_PicksCheapest(t *testing.T) {
	f := fakeFinder{cands: []Candidate{
		{InstanceType: "c7i.4xlarge", Family: "c7i", VCPUs: 16, MemoryGiB: 32, OnDemandPrice: 0.71},
		{InstanceType: "c7a.4xlarge", Family: "c7a", VCPUs: 16, MemoryGiB: 32, OnDemandPrice: 0.62},
		{InstanceType: "m7i.4xlarge", Family: "m7i", VCPUs: 16, MemoryGiB: 64, OnDemandPrice: 0.80},
	}}
	got, err := Size(context.Background(), f, ResourceRequest{CPU: 16, MemoryGiB: 32})
	if err != nil {
		t.Fatal(err)
	}
	if got.InstanceType != "c7a.4xlarge" {
		t.Errorf("cheapest = %q, want c7a.4xlarge", got.InstanceType)
	}
	if got.Considered != 3 {
		t.Errorf("Considered = %d, want 3", got.Considered)
	}
}

func TestSize_FamilyAllowList(t *testing.T) {
	f := fakeFinder{cands: []Candidate{
		{InstanceType: "c7a.4xlarge", Family: "c7a", OnDemandPrice: 0.62}, // cheapest but not allowed
		{InstanceType: "c7i.4xlarge", Family: "c7i", OnDemandPrice: 0.71},
		{InstanceType: "m7i.4xlarge", Family: "m7i", OnDemandPrice: 0.80},
	}}
	got, err := Size(context.Background(), f, ResourceRequest{Families: []string{"c7i", "m7i"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.InstanceType != "c7i.4xlarge" {
		t.Errorf("with allow-list [c7i,m7i], picked %q, want c7i.4xlarge (c7a excluded)", got.InstanceType)
	}
	if got.Considered != 2 {
		t.Errorf("Considered = %d, want 2 (c7a filtered out)", got.Considered)
	}
}

func TestSize_RequiresGPUs(t *testing.T) {
	f := fakeFinder{cands: []Candidate{
		{InstanceType: "c7i.4xlarge", Family: "c7i", GPUs: 0, OnDemandPrice: 0.71},
		{InstanceType: "g5.xlarge", Family: "g5", GPUs: 1, OnDemandPrice: 1.00},
	}}
	got, err := Size(context.Background(), f, ResourceRequest{GPUs: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got.InstanceType != "g5.xlarge" {
		t.Errorf("GPU request picked %q, want g5.xlarge", got.InstanceType)
	}
}

func TestSize_UnknownPriceSortsLast(t *testing.T) {
	f := fakeFinder{cands: []Candidate{
		{InstanceType: "c7i.4xlarge", Family: "c7i", OnDemandPrice: 0}, // unknown
		{InstanceType: "c7a.4xlarge", Family: "c7a", OnDemandPrice: 0.62},
	}}
	got, err := Size(context.Background(), f, ResourceRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got.InstanceType != "c7a.4xlarge" {
		t.Errorf("picked %q, want the priced option c7a.4xlarge over the unknown-price one", got.InstanceType)
	}
}

func TestSize_NoMatch(t *testing.T) {
	f := fakeFinder{cands: []Candidate{{InstanceType: "c7i.4xlarge", Family: "c7i"}}}
	_, err := Size(context.Background(), f, ResourceRequest{Families: []string{"p5"}})
	if err == nil {
		t.Fatal("expected no-match error when allow-list excludes everything")
	}
}

func TestEffectiveMemoryGiB(t *testing.T) {
	if got := EffectiveMemoryGiB(ResourceRequest{MemoryGiB: 32}); got != 32 {
		t.Errorf("no headroom = %v, want 32", got)
	}
	if got := EffectiveMemoryGiB(ResourceRequest{MemoryGiB: 32, MemoryHeadroomPercent: 25}); got != 40 {
		t.Errorf("25%% headroom on 32 = %v, want 40", got)
	}
}
