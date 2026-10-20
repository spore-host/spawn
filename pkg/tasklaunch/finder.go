package tasklaunch

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	libpricing "github.com/spore-host/libs/pricing"
	"github.com/spore-host/spawn/pkg/pricelist"
	"github.com/spore-host/spawn/pkg/taskproto"
	truffleaws "github.com/spore-host/truffle/pkg/aws"
)

// truffleFinder adapts truffle's SearchInstanceTypes to taskproto.InstanceFinder.
// It searches one region (region-spread is a later increment) and projects each
// result down to a taskproto.Candidate. Read-only: offerings + pricing only.
type truffleFinder struct {
	tc instanceSearcher
	// priceList prices the candidates: ONE region-wide table fetch per sizing,
	// never a request per candidate (spawn#1181). See pkg/pricelist.
	priceList pricelist.API
	region    string
	// warn receives the static-price note; verbose receives the -v timing
	// breakdown. Either may be nil.
	warn, verbose io.Writer
}

// instanceSearcher is the part of truffle's client sizing uses to enumerate
// candidates; tests inject a fake.
type instanceSearcher interface {
	SearchInstanceTypes(ctx context.Context, regions []string, matcher *regexp.Regexp, opts truffleaws.FilterOptions) ([]truffleaws.InstanceTypeResult, error)
}

// NewFinder returns the AWS-backed taskproto.InstanceFinder spawn task run
// sizes with: truffle's instance search for one region, priced from ONE
// region-wide Price List table per sizing (spawn#1181), with libs/pricing's
// static table as the exact-match fallback, and a pinned
// resources.instance_type looked up on its own (spawn#1253). warn receives the
// note that labels static prices; verbose receives the -v timing breakdown.
// Either may be nil.
func NewFinder(cfg awssdk.Config, region string, warn, verbose io.Writer) taskproto.InstanceFinder {
	return truffleFinder{
		tc:        truffleaws.NewClientFromConfig(cfg),
		priceList: pricelist.NewClient(cfg),
		region:    region,
		warn:      warn,
		verbose:   verbose,
	}
}

func (f truffleFinder) FindCandidates(ctx context.Context, req taskproto.ResourceRequest) ([]taskproto.Candidate, error) {
	// Match all types; the resource minimums do the filtering. Family allow-list
	// is applied by the sizer (truffle's FilterOptions has only a single family).
	matcher := regexp.MustCompile(`.*`)
	opts := truffleaws.FilterOptions{
		Architecture: req.Architecture,
		MinVCPUs:     req.CPU,
		MinMemory:    taskproto.EffectiveMemoryGiB(req),
	}
	// A pinned type is looked up on its own (#1253): an exact matcher, which
	// truffle turns into one DescribeInstanceTypes for that type, and no
	// minimums, since a pin is honoured whatever the request says.
	pin := strings.TrimSpace(req.InstanceType)
	if pin != "" {
		matcher = regexp.MustCompile("^" + regexp.QuoteMeta(pin) + "$")
		opts = truffleaws.FilterOptions{}
	}
	var timing sizingTiming
	start := time.Now()
	results, err := f.tc.SearchInstanceTypes(ctx, []string{f.region}, matcher, opts)
	timing.search = time.Since(start)
	if err != nil {
		return nil, err
	}
	timing.candidates = len(results)

	// SearchInstanceTypes does NOT populate on-demand price, so r.OnDemandPrice
	// is 0 here. The sizer ranks on price — without it, "cheapest" degenerates
	// to a size tie-break — so price every candidate from one region-wide table
	// (live Price List, with the static table as an exact-match fallback only
	// for a type it covers — truffle#114). A type neither table prices keeps
	// price 0, and the sizer sorts unpriced options last, so a priced option
	// always wins and we never silently pick a huge box because pricing was
	// unavailable.
	//
	// The table is fetched once, whatever the candidate count: pricing each
	// candidate separately cost 1325 Price List requests and minutes per launch
	// (spawn#1181).
	prices := &sizingPrices{region: f.region}
	start = time.Now()
	for _, r := range results {
		if r.OnDemandPrice <= 0 {
			switch {
			case f.priceList == nil:
			case pin != "":
				// One type needs one rate, not the region's table.
				prices.live, timing.priceRequests, prices.liveErr = pricelist.InstanceOnDemandPrices(ctx, f.priceList, f.region, pin)
			default:
				prices.live, timing.priceRequests, prices.liveErr = pricelist.RegionOnDemandPrices(ctx, f.priceList, f.region)
			}
			break
		}
	}
	timing.pricing = time.Since(start)

	cands := make([]taskproto.Candidate, 0, len(results))
	for _, r := range results {
		price := r.OnDemandPrice
		if price <= 0 {
			price = prices.price(r.InstanceType)
		}
		cands = append(cands, taskproto.Candidate{
			InstanceType:  r.InstanceType,
			Family:        r.InstanceFamily,
			VCPUs:         int(r.VCPUs),
			MemoryGiB:     float64(r.MemoryMiB) / 1024,
			GPUs:          int(r.GPUs),
			Architecture:  r.Architecture,
			OnDemandPrice: price,
		})
	}
	prices.report(f.warn)
	timing.report(f.verbose)
	return cands, nil
}

// sizingPrices is the price table one sizing run ranks on: the live region
// table, with libs/pricing's static table as an exact-match fallback for a type
// the live table lacks, which is the same order truffle's default pricer uses.
type sizingPrices struct {
	live    map[string]float64
	liveErr error
	region  string
	// staticUsed counts the candidates priced from the static table, so the
	// caller can say that it happened.
	staticUsed int
}

// price returns the rate for instanceType, or 0 when neither table has one.
func (p *sizingPrices) price(instanceType string) float64 {
	if v := p.live[instanceType]; v > 0 {
		return v
	}
	if v, err := libpricing.GetEC2HourlyRate(p.region, instanceType); err == nil && v > 0 {
		p.staticUsed++
		return v
	}
	return 0
}

// report writes the provenance of the ranking prices to w: a static rate is
// labelled with why it was used and when the table was collected. Silent when
// every price was live.
func (p *sizingPrices) report(w io.Writer) {
	if w == nil || p.staticUsed == 0 {
		return
	}
	why := "the live Price List has no rate for them"
	if p.liveErr != nil {
		why = fmt.Sprintf("the live Price List lookup failed: %v", p.liveErr)
	}
	fmt.Fprintf(w, "note: %d candidate type(s) ranked on spawn's STATIC price table (collected %s), because %s\n",
		p.staticUsed, libpricing.PricesAsOf, why)
}

// sizingTiming is the -v breakdown of where sizing spent its time.
type sizingTiming struct {
	search, pricing time.Duration
	candidates      int
	priceRequests   int
}

func (t sizingTiming) report(w io.Writer) {
	if w == nil {
		return
	}
	fmt.Fprintf(w, "sizing: instance search %s (%d candidates), pricing %s (%d Price List request(s))\n",
		t.search.Round(time.Millisecond), t.candidates, t.pricing.Round(time.Millisecond), t.priceRequests)
}
