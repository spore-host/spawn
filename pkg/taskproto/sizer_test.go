package taskproto

import (
	"context"
	"errors"
	"strings"
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

func TestSize_ExactInstanceTypePin(t *testing.T) {
	// An exact pin bypasses candidate ranking and returns the type verbatim with
	// its derived family, even when looking its sizes up fails (spawn#1253).
	f := fakeFinder{err: context.Canceled}
	got, err := Size(context.Background(), f, ResourceRequest{InstanceType: "t3.medium", Families: []string{"c7i"}, CPU: 99})
	if err != nil {
		t.Fatalf("exact pin should not error: %v", err)
	}
	if !got.Pinned || got.SizesKnown() || !errors.Is(got.PinLookupErr, context.Canceled) {
		t.Errorf("failed pin lookup: Pinned=%v SizesKnown=%v PinLookupErr=%v", got.Pinned, got.SizesKnown(), got.PinLookupErr)
	}
	if got.InstanceType != "t3.medium" {
		t.Errorf("pin = %q, want t3.medium (verbatim, ignoring families/cpu)", got.InstanceType)
	}
	if got.Family != "t3" {
		t.Errorf("pin family = %q, want t3", got.Family)
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

// TestSize_PriceTiePrefersSmallest is the spawn#610 regression guard: when every
// candidate costs the same, the smallest type that fits must win.
//
// The hpc7g family is the real-world case and it is chosen deliberately, not
// arbitrarily: you rent the socket rather than the cores, so 4xl/8xl/16xl are all
// $1.6832/hr with 128 GiB, and a 16-vCPU request used to be answered with a
// 64-vCPU box. The old tie-break compared type NAMES, and "hpc7g.16xlarge" sorts
// before "hpc7g.4xlarge" because '1' < '4'.
//
// The size choice matters for the test itself: a family whose sizes are
// 2x/4x/8xlarge would pass under the OLD code too (lexicographic order happens to
// put the smallest first there), so such a test would be vacuous. Any regression
// test here must include a 1-prefixed double-digit size.
func TestSize_PriceTiePrefersSmallest(t *testing.T) {
	const rate = 1.6832
	f := fakeFinder{cands: []Candidate{
		{InstanceType: "hpc7g.16xlarge", Family: "hpc7g", VCPUs: 64, MemoryGiB: 128, OnDemandPrice: rate},
		{InstanceType: "hpc7g.4xlarge", Family: "hpc7g", VCPUs: 16, MemoryGiB: 128, OnDemandPrice: rate},
		{InstanceType: "hpc7g.8xlarge", Family: "hpc7g", VCPUs: 32, MemoryGiB: 128, OnDemandPrice: rate},
	}}
	got, err := Size(context.Background(), f, ResourceRequest{
		CPU: 16, MemoryGiB: 128, Architecture: "arm64", Families: []string{"hpc7g"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.InstanceType != "hpc7g.4xlarge" {
		t.Errorf("tie went to %q, want hpc7g.4xlarge (smallest that fits at an equal rate)", got.InstanceType)
	}
	if got.VCPUs != 16 {
		t.Errorf("VCPUs = %d, want 16 (asking for 16 must not yield 64)", got.VCPUs)
	}
	// Guard the guard: if the fixture ever loses its 1-prefixed size, this test
	// stops proving anything, because lexicographic order would agree with us.
	var sawOnePrefixed bool
	for _, c := range f.cands {
		if strings.Contains(c.InstanceType, ".16xlarge") || strings.Contains(c.InstanceType, ".12xlarge") {
			sawOnePrefixed = true
		}
	}
	if !sawOnePrefixed {
		t.Error("fixture lost its 1-prefixed double-digit size; this test can no longer distinguish the old lexicographic tie-break")
	}
}

// TestSize_PriceTieFallsBackToNameWhenSameSize keeps the ordering total: equal
// price AND equal size must still be deterministic across runs.
func TestSize_PriceTieFallsBackToNameWhenSameSize(t *testing.T) {
	f := fakeFinder{cands: []Candidate{
		{InstanceType: "m7i.4xlarge", Family: "m7i", VCPUs: 16, MemoryGiB: 64, OnDemandPrice: 0.80},
		{InstanceType: "m7a.4xlarge", Family: "m7a", VCPUs: 16, MemoryGiB: 64, OnDemandPrice: 0.80},
	}}
	for i := 0; i < 5; i++ {
		got, err := Size(context.Background(), f, ResourceRequest{CPU: 16, MemoryGiB: 64})
		if err != nil {
			t.Fatal(err)
		}
		if got.InstanceType != "m7a.4xlarge" {
			t.Fatalf("run %d: got %q, want m7a.4xlarge (deterministic name fallback)", i, got.InstanceType)
		}
	}
}

// TestSize_PriceTiePrefersLessMemoryWhenVCPUsEqual covers the second tie-break
// rung: same price, same vCPUs, different memory → take the smaller.
func TestSize_PriceTiePrefersLessMemoryWhenVCPUsEqual(t *testing.T) {
	f := fakeFinder{cands: []Candidate{
		{InstanceType: "r7i.2xlarge", Family: "r7i", VCPUs: 8, MemoryGiB: 64, OnDemandPrice: 0.50},
		{InstanceType: "c7i.2xlarge", Family: "c7i", VCPUs: 8, MemoryGiB: 16, OnDemandPrice: 0.50},
	}}
	got, err := Size(context.Background(), f, ResourceRequest{CPU: 8, MemoryGiB: 16})
	if err != nil {
		t.Fatal(err)
	}
	if got.InstanceType != "c7i.2xlarge" {
		t.Errorf("got %q, want c7i.2xlarge (same price + same vCPUs → less memory)", got.InstanceType)
	}
}

// #1180: the container memory floor, from SizingRequest's documented rules.
func TestSizingRequest_ContainerMemoryFloor(t *testing.T) {
	const img = "quay.io/x/y:1"
	cases := []struct {
		name      string
		container string
		res       ResourceRequest
		wantMem   float64 // EffectiveMemoryGiB of the result
	}{
		{"host task unchanged", "", ResourceRequest{CPU: 2}, 0},
		{"container cpu only", img, ResourceRequest{CPU: 2}, ContainerMemoryFloorGiB},
		{"container memory below floor", img, ResourceRequest{MemoryGiB: 1}, ContainerMemoryFloorGiB},
		{"below floor even with headroom", img, ResourceRequest{MemoryGiB: 1, MemoryHeadroomPercent: 50}, ContainerMemoryFloorGiB},
		{"headroom lifts it over the floor", img, ResourceRequest{MemoryGiB: 1.8, MemoryHeadroomPercent: 20}, 1.8 * 1.2},
		{"container above floor unchanged", img, ResourceRequest{MemoryGiB: 4}, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &TaskSpec{Container: tc.container, Resources: tc.res}
			got := s.SizingRequest()
			if mem := EffectiveMemoryGiB(got); mem < tc.wantMem-1e-9 || mem > tc.wantMem+1e-9 {
				t.Errorf("EffectiveMemoryGiB(SizingRequest()) = %v, want %v (req %+v)", mem, tc.wantMem, got)
			}
			if got.CPU != tc.res.CPU {
				t.Errorf("CPU changed: %d -> %d", tc.res.CPU, got.CPU)
			}
		})
	}
}

// Size enforces the request's memory itself, so a floor holds even when the
// finder returns smaller types.
func TestSize_FiltersCandidatesBelowRequestedMemory(t *testing.T) {
	f := fakeFinder{cands: []Candidate{
		{InstanceType: "t4g.nano", Family: "t4g", VCPUs: 2, MemoryGiB: 0.5, OnDemandPrice: 0.0042},
		{InstanceType: "t4g.small", Family: "t4g", VCPUs: 2, MemoryGiB: 2, OnDemandPrice: 0.0168},
	}}
	s := &TaskSpec{Container: "quay.io/x/y:1", Resources: ResourceRequest{CPU: 2}}
	got, err := Size(context.Background(), f, s.SizingRequest())
	if err != nil {
		t.Fatal(err)
	}
	if got.InstanceType != "t4g.small" {
		t.Errorf("sized %s, want t4g.small (nano is below the %.0f GiB container floor)", got.InstanceType, ContainerMemoryFloorGiB)
	}
}

// recordingSizeFinder records each request and returns a fixed list.
type recordingSizeFinder struct {
	cands []Candidate
	got   []ResourceRequest
}

func (f *recordingSizeFinder) FindCandidates(_ context.Context, req ResourceRequest) ([]Candidate, error) {
	f.got = append(f.got, req)
	return f.cands, nil
}

// spawn#1253: a pin is looked up once through the finder, asking for exactly
// the pinned type, and its sizes and price come from that lookup, not zero. The
// ranking filters do not apply: a pin below the request's cpu is still taken.
func TestSize_PinLooksUpSizesAndPrice(t *testing.T) {
	f := &recordingSizeFinder{cands: []Candidate{
		{InstanceType: "r8g.xlarge", Family: "r8g", VCPUs: 4, MemoryGiB: 32, OnDemandPrice: 0.2357},
		{InstanceType: "r8g.2xlarge", Family: "r8g", VCPUs: 8, MemoryGiB: 64, OnDemandPrice: 0.4713},
	}}
	got, err := Size(context.Background(), f, ResourceRequest{InstanceType: "r8g.2xlarge", CPU: 99, MemoryGiB: 512})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.got) != 1 || f.got[0].InstanceType != "r8g.2xlarge" || f.got[0].CPU != 0 || f.got[0].MemoryGiB != 0 {
		t.Fatalf("finder requests = %+v, want one lookup of exactly r8g.2xlarge with no minimums", f.got)
	}
	if !got.Pinned || !got.SizesKnown() || got.VCPUs != 8 || got.MemoryGiB != 64 || got.OnDemandPrice != 0.4713 {
		t.Errorf("pinned size = %+v, want r8g.2xlarge 8 vCPU / 64 GiB / $0.4713", got)
	}

	// The finder answering without the pinned type is a failed lookup, not a
	// different type and not an error.
	miss, err := Size(context.Background(), &recordingSizeFinder{cands: f.cands[:1]}, ResourceRequest{InstanceType: "r8g.2xlarge"})
	if err != nil {
		t.Fatal(err)
	}
	if miss.InstanceType != "r8g.2xlarge" || miss.SizesKnown() || miss.PinLookupErr == nil || miss.VCPUs != 0 {
		t.Errorf("pin absent from the lookup: %+v", miss)
	}
}
