package cmd

import (
	"fmt"
	"io"
	"time"

	libpricing "github.com/spore-host/libs/pricing"
)

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
