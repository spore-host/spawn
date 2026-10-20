package tasklaunch

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

// recordingSearcher records the matcher and filters it was asked with.
type recordingSearcher struct {
	results  []truffleaws.InstanceTypeResult
	matchers []string
	opts     []truffleaws.FilterOptions
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

// A pinned type is honoured, but a container pin below the floor is warned about.
func TestContainerPinBelowFloorWarns(t *testing.T) {
	spec := containerCPUOnlySpec()
	spec.Resources.InstanceType = "t4g.nano"

	var buf bytes.Buffer
	f := &fakeMemoryLookup{mib: 512, ok: true}
	warnIfContainerPinBelowFloor(context.Background(), f, &buf, "us-east-1", spec)
	if f.seen != "t4g.nano" {
		t.Errorf("looked up %q, want the pinned type", f.seen)
	}
	if !strings.Contains(buf.String(), "t4g.nano has 0.5 GiB") || !strings.Contains(buf.String(), "#1180") {
		t.Errorf("missing pin warning:\n%s", buf.String())
	}

	if got := spec.SizingRequest(); got.InstanceType != "t4g.nano" || got.MemoryGiB != 0 {
		t.Errorf("SizingRequest altered a pinned request: %+v", got)
	}

	for name, tc := range map[string]struct {
		mutate func(*taskproto.TaskSpec)
		mib    int64
	}{
		"at floor":       {func(*taskproto.TaskSpec) {}, 2048},
		"host task":      {func(s *taskproto.TaskSpec) { s.Container = "" }, 512},
		"no pin":         {func(s *taskproto.TaskSpec) { s.Resources.InstanceType = "" }, 512},
		"memory unknown": {func(*taskproto.TaskSpec) {}, 0},
	} {
		s := containerCPUOnlySpec()
		s.Resources.InstanceType = "t4g.nano"
		tc.mutate(s)
		buf.Reset()
		warnIfContainerPinBelowFloor(context.Background(), &fakeMemoryLookup{mib: tc.mib, ok: tc.mib > 0}, &buf, "us-east-1", s)
		if buf.Len() != 0 {
			t.Errorf("%s: unexpected warning:\n%s", name, buf.String())
		}
	}
}

func (r *recordingSearcher) SearchInstanceTypes(_ context.Context, _ []string, m *regexp.Regexp, o truffleaws.FilterOptions) ([]truffleaws.InstanceTypeResult, error) {
	r.matchers = append(r.matchers, m.String())
	r.opts = append(r.opts, o)
	return r.results, nil
}
