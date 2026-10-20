package cmd

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/taskproto"
	truffleaws "github.com/spore-host/truffle/pkg/aws"
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

// recordingSearcher records the matcher and filters it was asked with.
type recordingSearcher struct {
	results  []truffleaws.InstanceTypeResult
	matchers []string
	opts     []truffleaws.FilterOptions
}

func (r *recordingSearcher) SearchInstanceTypes(_ context.Context, _ []string, m *regexp.Regexp, o truffleaws.FilterOptions) ([]truffleaws.InstanceTypeResult, error) {
	r.matchers = append(r.matchers, m.String())
	r.opts = append(r.opts, o)
	return r.results, nil
}

// The AWS-backed finder looks a pin up as that one type: an exact matcher (one
// DescribeInstanceTypes in truffle), no minimums, and a single Price List
// request filtered to the type rather than the region's whole table.
func TestTruffleFinder_PinIsOneTypeLookup(t *testing.T) {
	s := &recordingSearcher{results: []truffleaws.InstanceTypeResult{
		{InstanceType: "r8g.2xlarge", InstanceFamily: "r8g", VCPUs: 8, MemoryMiB: 65536, Architecture: "arm64"},
	}}
	pl := &recordingPriceList{region: "us-west-2", prices: map[string]float64{"r8g.2xlarge": 0.4713, "r8g.xlarge": 0.2357}}
	f := truffleFinder{tc: s, priceList: pl, region: "us-west-2"}

	sized, err := taskproto.Size(context.Background(), f, pinnedR8gSpec().SizingRequest())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.matchers) != 1 || s.matchers[0] != `^r8g\.2xlarge$` {
		t.Errorf("searcher matchers = %q, want one exact match of the pin", s.matchers)
	}
	if len(s.opts) == 1 && (s.opts[0] != truffleaws.FilterOptions{}) {
		t.Errorf("pin lookup applied filters %+v", s.opts[0])
	}
	if pl.calls != 1 || pl.perTypeFilters != 1 {
		t.Errorf("Price List: %d request(s), %d filtered by type; want 1 and 1", pl.calls, pl.perTypeFilters)
	}
	if !sized.SizesKnown() || sized.VCPUs != 8 || sized.MemoryGiB != 64 || sized.OnDemandPrice != 0.4713 {
		t.Errorf("sized = %+v", sized)
	}
}

// The pin checks reuse the sizing lookup: with the pin's sizes known, the
// memory lookup they used to make on their own is not called.
func TestSizedMemoryLookupReusesTheSizingLookup(t *testing.T) {
	known := &taskproto.SizeResult{InstanceType: "t4g.nano", Pinned: true, VCPUs: 2, MemoryGiB: 0.5}
	fb := &fakeMemoryLookup{mib: 99999, ok: true}
	spec := containerCPUOnlySpec()
	spec.Resources.InstanceType = "t4g.nano"

	var buf bytes.Buffer
	warnIfContainerPinBelowFloor(context.Background(), sizedMemoryLookup{sized: known, fallback: fb}, &buf, "us-east-1", spec)
	if fb.seen != "" {
		t.Errorf("fallback memory lookup called for %q although sizing knew the pin", fb.seen)
	}
	if !strings.Contains(buf.String(), "t4g.nano has 0.5 GiB") {
		t.Errorf("warning not driven by the sized memory:\n%s", buf.String())
	}

	// A failed sizing lookup falls back, so the static table still answers.
	failed := &taskproto.SizeResult{InstanceType: "t4g.nano", Pinned: true, PinLookupErr: errors.New("x")}
	fb = &fakeMemoryLookup{mib: 512, ok: true}
	buf.Reset()
	warnIfContainerPinBelowFloor(context.Background(), sizedMemoryLookup{sized: failed, fallback: fb}, &buf, "us-east-1", spec)
	if fb.seen != "t4g.nano" || !strings.Contains(buf.String(), "0.5 GiB") {
		t.Errorf("failed sizing lookup did not fall back: seen=%q\n%s", fb.seen, buf.String())
	}
}
