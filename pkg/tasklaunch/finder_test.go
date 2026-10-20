package tasklaunch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/pricing"
	libpricing "github.com/spore-host/libs/pricing"
	"github.com/spore-host/spawn/pkg/pricelist"
	"github.com/spore-host/spawn/pkg/taskproto"
	truffleaws "github.com/spore-host/truffle/pkg/aws"
)

// fakeSearcher returns a fixed candidate list, as truffle's SearchInstanceTypes
// would for one region.
type fakeSearcher struct {
	results []truffleaws.InstanceTypeResult
}

func (f fakeSearcher) SearchInstanceTypes(context.Context, []string, *regexp.Regexp, truffleaws.FilterOptions) ([]truffleaws.InstanceTypeResult, error) {
	return f.results, nil
}

// recordingPriceList is a Price List GetProducts fake that counts every request
// and honours the filters a real request would: regionCode, and instanceType
// when a caller asks for one type. It serves the table in pages of MaxResults.
type recordingPriceList struct {
	region string
	prices map[string]float64 // instanceType → $/hr
	err    error

	mu             sync.Mutex
	calls          int
	perTypeFilters int
}

func (r *recordingPriceList) GetProducts(_ context.Context, in *pricing.GetProductsInput, _ ...func(*pricing.Options)) (*pricing.GetProductsOutput, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.err != nil {
		return nil, r.err
	}
	var onlyType string
	for _, f := range in.Filters {
		switch *f.Field {
		case "instanceType":
			r.perTypeFilters++
			onlyType = *f.Value
		case "regionCode":
			if *f.Value != r.region {
				return &pricing.GetProductsOutput{}, nil
			}
		}
	}
	types := make([]string, 0, len(r.prices))
	for t := range r.prices {
		if onlyType == "" || t == onlyType {
			types = append(types, t)
		}
	}
	sort.Strings(types)

	pageSize := 100
	if in.MaxResults != nil {
		pageSize = int(*in.MaxResults)
	}
	start := 0
	if in.NextToken != nil {
		start, _ = strconv.Atoi(*in.NextToken)
	}
	end := min(start+pageSize, len(types))
	out := &pricing.GetProductsOutput{}
	for _, t := range types[start:end] {
		out.PriceList = append(out.PriceList, fmt.Sprintf(
			`{"product":{"attributes":{"instanceType":%q,"regionCode":%q}},"terms":{"OnDemand":{"X.Y":{"priceDimensions":{"X.Y.Z":{"unit":"Hrs","pricePerUnit":{"USD":"%.4f"}}}}}}}`,
			t, r.region, r.prices[t]))
	}
	if end < len(types) {
		next := strconv.Itoa(end)
		out.NextToken = &next
	}
	return out, nil
}

// syntheticCatalog builds n instance types, all with at least 2 vCPUs, and a
// price for each that is NOT monotonic in the order the searcher returns them,
// so a sizer that ignored prices would not land on the cheapest by accident.
func syntheticCatalog(n int) ([]truffleaws.InstanceTypeResult, map[string]float64, string) {
	sizes := []struct {
		name string
		vcpu int32
	}{{"large", 2}, {"xlarge", 4}, {"2xlarge", 8}, {"4xlarge", 16}, {"8xlarge", 32}}
	results := make([]truffleaws.InstanceTypeResult, 0, n)
	prices := make(map[string]float64, n)
	cheapest, cheapestPrice := "", math.MaxFloat64
	for i := 0; len(results) < n; i++ {
		family := fmt.Sprintf("zz%d", i)
		for j, s := range sizes {
			if len(results) == n {
				break
			}
			it := family + "." + s.name
			results = append(results, truffleaws.InstanceTypeResult{
				InstanceType: it, InstanceFamily: family, VCPUs: s.vcpu,
				MemoryMiB: int64(s.vcpu) * 4096, Architecture: "x86_64",
			})
			// Per-family base varies pseudo-randomly; price doubles with size.
			p := (0.05 + float64((i*7919)%997)/1000) * float64(int(1)<<j)
			prices[it] = p
			if p < cheapestPrice {
				cheapest, cheapestPrice = it, p
			}
		}
	}
	return results, prices, cheapest
}

// TestSizingMakesAConstantNumberOfPricingCalls is the spawn#1181 gate: sizing a
// `cpu: 2` spec must price its candidates with a constant number of Price List
// requests, not one per candidate. Measured in E1 (spawn#1171) the per-candidate
// design made 1325 requests and took 149-256 s before RunInstances.
//
// It asserts the mechanism three ways: no request filters by instanceType (a
// per-type lookup is the thing that scales with candidates); the request count
// equals the pages of the region table, whatever the candidate count; and the
// count is the same for 10 candidates and for 1325 against the same table.
func TestSizingMakesAConstantNumberOfPricingCalls(t *testing.T) {
	const region = "us-east-1"
	allResults, table, _ := syntheticCatalog(1325)
	wantRequests := (len(table) + pricelist.PageSize - 1) / pricelist.PageSize

	spec := &taskproto.TaskSpec{
		TaskID:    "e1-sized",
		Command:   []string{"true"},
		Resources: taskproto.ResourceRequest{CPU: 2},
		Lifecycle: taskproto.Lifecycle{TTL: "1h", OnComplete: "terminate"},
	}

	callsFor := func(t *testing.T, results []truffleaws.InstanceTypeResult) (*recordingPriceList, string) {
		t.Helper()
		pl := &recordingPriceList{region: region, prices: table}
		finder := truffleFinder{tc: fakeSearcher{results: results}, priceList: pl, region: region}
		var out bytes.Buffer
		if err := renderDryRun(context.Background(), &out, spec, finder, region); err != nil {
			t.Fatalf("renderTaskDryRun: %v", err)
		}
		return pl, out.String()
	}

	big, bigOut := callsFor(t, allResults)
	if big.perTypeFilters != 0 {
		t.Errorf("sizing sent %d Price List request(s) filtered by instanceType; pricing must be one region-wide table, not per candidate (spawn#1181)", big.perTypeFilters)
	}
	if big.calls != wantRequests {
		t.Errorf("sizing %d candidates made %d Price List request(s), want %d (the pages of ONE region table)", len(allResults), big.calls, wantRequests)
	}
	if big.calls > 20 {
		t.Errorf("sizing made %d Price List requests; it must be a small constant, not O(candidates)", big.calls)
	}

	small, _ := callsFor(t, allResults[:10])
	if small.calls != big.calls {
		t.Errorf("Price List requests depend on the candidate count: %d for 10 candidates, %d for %d", small.calls, big.calls, len(allResults))
	}

	// And the table is actually used: the cheapest candidate wins.
	_, _, cheapest := syntheticCatalog(1325)
	if !strings.Contains(bigOut, "Instance:     "+cheapest+" ") {
		t.Errorf("dry run did not choose the cheapest type %s from the region table\n%s", cheapest, bigOut)
	}
	if !strings.Contains(bigOut, "cheapest of 1325 matching type(s)") {
		t.Errorf("dry run did not rank all 1325 candidates\n%s", bigOut)
	}
}

// TestSizingFallsBackToTheStaticTableAndSaysSo: when the Price List is
// unavailable the candidates are priced from libs/pricing's static table by
// exact match (the order truffle's default pricer uses), the request is not
// retried per candidate, and the output labels the prices as static with the
// table's collection date.
func TestSizingFallsBackToTheStaticTableAndSaysSo(t *testing.T) {
	const region = "us-east-1"
	static := libpricing.EC2Pricing[region]
	if len(static) < 2 {
		t.Fatalf("static table has %d entries for %s; the test needs at least 2", len(static), region)
	}
	var results []truffleaws.InstanceTypeResult
	cheapest, cheapestPrice := "", math.MaxFloat64
	for it, p := range static {
		results = append(results, truffleaws.InstanceTypeResult{InstanceType: it, InstanceFamily: strings.SplitN(it, ".", 2)[0], VCPUs: 2, MemoryMiB: 8192})
		if p < cheapestPrice || (p == cheapestPrice && it < cheapest) {
			cheapest, cheapestPrice = it, p
		}
	}
	// One type the static table cannot price stays unpriced.
	results = append(results, truffleaws.InstanceTypeResult{InstanceType: "zz9.large", InstanceFamily: "zz9", VCPUs: 2, MemoryMiB: 8192})

	pl := &recordingPriceList{region: region, err: errors.New("AccessDeniedException")}
	var warn bytes.Buffer
	finder := truffleFinder{tc: fakeSearcher{results: results}, priceList: pl, region: region, warn: &warn}
	cands, err := finder.FindCandidates(context.Background(), taskproto.ResourceRequest{CPU: 2})
	if err != nil {
		t.Fatalf("FindCandidates: %v", err)
	}
	if pl.calls != 1 {
		t.Errorf("a failing Price List was called %d times; want 1 (no per-candidate retry)", pl.calls)
	}
	for _, c := range cands {
		want := static[c.InstanceType]
		if c.OnDemandPrice != want {
			t.Errorf("%s priced %.5f, want static %.5f", c.InstanceType, c.OnDemandPrice, want)
		}
	}
	note := warn.String()
	for _, w := range []string{"STATIC", libpricing.PricesAsOf, "AccessDeniedException"} {
		if !strings.Contains(note, w) {
			t.Errorf("static-price note missing %q: %q", w, note)
		}
	}
	sized, err := taskproto.Size(context.Background(), fakeTaskFinder{cands: cands}, taskproto.ResourceRequest{CPU: 2})
	if err != nil {
		t.Fatalf("Size: %v", err)
	}
	if sized.InstanceType != cheapest {
		t.Errorf("chose %s, want the cheapest static type %s", sized.InstanceType, cheapest)
	}
}

// TestSizingVerboseTiming: -v reports where sizing spent its time and how many
// Price List requests it made.
func TestSizingVerboseTiming(t *testing.T) {
	results, table, _ := syntheticCatalog(250)
	pl := &recordingPriceList{region: "us-east-1", prices: table}
	var verbose bytes.Buffer
	finder := truffleFinder{tc: fakeSearcher{results: results}, priceList: pl, region: "us-east-1", verbose: &verbose}
	if _, err := finder.FindCandidates(context.Background(), taskproto.ResourceRequest{CPU: 2}); err != nil {
		t.Fatalf("FindCandidates: %v", err)
	}
	if got := verbose.String(); !strings.Contains(got, "(250 candidates)") || !strings.Contains(got, "(3 Price List request(s))") {
		t.Errorf("verbose timing = %q", got)
	}
}
