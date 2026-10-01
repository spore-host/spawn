package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
)

// withSweepRowPricer swaps the resolveSweepRowPrice seam for the duration of a
// test so nothing touches AWS, and restores it afterwards.
func withSweepRowPricer(t *testing.T, fn func(ctx context.Context, region, instanceType string) (aws.DisplayPrice, error)) {
	t.Helper()
	old := resolveSweepRowPrice
	resolveSweepRowPrice = fn
	t.Cleanup(func() { resolveSweepRowPrice = old })
}

// captureStderr redirects os.Stderr for the duration of fn and returns what was
// written. estimateAndReportSweep and reportSweepBudget print there directly.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	old := os.Stderr
	os.Stderr = w

	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				b.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
		done <- b.String()
	}()

	fn()
	os.Stderr = old
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}

func sweepParams(rows ...map[string]interface{}) *ParamFileFormat {
	return &ParamFileFormat{
		Defaults: map[string]interface{}{"region": "us-east-1", "ttl": "1h"},
		Params:   rows,
	}
}

// TestSweepPricerMemoizesPerShape is the reason the pricer exists: a sweep is
// many rows over few distinct shapes, and a pricing call per row would be both
// slow and an inflated "N shapes priced" count.
func TestSweepPricerMemoizesPerShape(t *testing.T) {
	var calls int
	withSweepRowPricer(t, func(_ context.Context, region, instanceType string) (aws.DisplayPrice, error) {
		calls++
		return aws.DisplayPrice{PricePerHour: 1.5, Source: "live"}, nil
	})

	p := newSweepPricer(context.Background())
	for i := 0; i < 5; i++ {
		if _, err := p.Resolve("us-east-1", "c5.xlarge"); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
	}
	if _, err := p.Resolve("us-west-2", "c5.xlarge"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if calls != 2 {
		t.Errorf("expected 2 pricing calls for 2 distinct shapes over 6 lookups, got %d", calls)
	}
	if got := p.SourceSummary(); !strings.Contains(got, "2 instance shape(s) priced") {
		t.Errorf("SourceSummary should count distinct shapes, got %q", got)
	}
}

// TestSweepPricerCachesFailures keeps a dead lookup from being retried once per
// row — a 500-row sweep against an unpriceable type would otherwise make 500
// failing calls.
func TestSweepPricerCachesFailures(t *testing.T) {
	var calls int
	withSweepRowPricer(t, func(_ context.Context, _, instanceType string) (aws.DisplayPrice, error) {
		calls++
		return aws.DisplayPrice{}, fmt.Errorf("no price for %s", instanceType)
	})

	p := newSweepPricer(context.Background())
	for i := 0; i < 4; i++ {
		if _, err := p.Resolve("us-east-1", "p5e.48xlarge"); err == nil {
			t.Fatal("expected an error for an unpriceable shape")
		}
	}
	if calls != 1 {
		t.Errorf("expected the failure to be cached (1 call), got %d", calls)
	}
	if got := p.SourceSummary(); got != "" {
		t.Errorf("SourceSummary should be empty when nothing priced, got %q", got)
	}
}

// TestSweepPricerTreatsZeroPriceAsUnpriced guards the gap between "no error" and
// "usable rate": a $0.00 quote must not be allowed to value a row at nothing.
func TestSweepPricerTreatsZeroPriceAsUnpriced(t *testing.T) {
	withSweepRowPricer(t, func(_ context.Context, _, _ string) (aws.DisplayPrice, error) {
		return aws.DisplayPrice{PricePerHour: 0, Source: "live"}, nil
	})

	p := newSweepPricer(context.Background())
	if _, err := p.Resolve("us-east-1", "c5.xlarge"); err == nil {
		t.Error("a zero price with a nil error must still be treated as unpriced")
	}
}

// TestEstimateAndReportSweepUsesTruffleNotTheStaticGuess is the #29 regression
// guard at the spawn level. The stub returns a rate unlike anything in
// libs/pricing's old table, so if the estimate matches the table the resolver was
// bypassed.
func TestEstimateAndReportSweepUsesTruffleNotTheStaticGuess(t *testing.T) {
	withSweepRowPricer(t, func(_ context.Context, region, instanceType string) (aws.DisplayPrice, error) {
		if instanceType == "g6e.12xlarge" {
			// The real us-east-1 rate. The old static guess answered $2.40/hr
			// here (unknown family 0.10 x 12xlarge 24.0) — 4.4x low.
			return aws.DisplayPrice{PricePerHour: 10.49, Source: "live"}, nil
		}
		return aws.DisplayPrice{}, fmt.Errorf("no price for %s in %s", instanceType, region)
	})

	var est interface{ Partial() bool }
	out := captureStderr(t, func() {
		e, err := estimateAndReportSweep(context.Background(), sweepParams(
			map[string]interface{}{"name": "gpu", "instance_type": "g6e.12xlarge"},
		))
		if err != nil {
			t.Errorf("estimateAndReportSweep: %v", err)
			return
		}
		est = e
		if got := e.ComputeCost; got < 10.48 || got > 10.50 {
			t.Errorf("expected compute ~10.49 from the injected resolver, got %.4f (2.40 would mean the old static guess)", got)
		}
	})

	if est == nil {
		t.Fatal("no estimate returned")
	}
	if est.Partial() {
		t.Error("every row priced, so the estimate must not be partial")
	}
	if !strings.Contains(out, "1 instance shape(s) priced: 1 live") {
		t.Errorf("preview must say where the rate came from (#543), got:\n%s", out)
	}
}

// TestEstimateAndReportSweepNamesUnpriceableRows is the behaviour that replaces
// the fabricated rate: an unpriceable row is named and excluded, and the total is
// labelled a floor rather than passed off as the cost of the sweep.
func TestEstimateAndReportSweepNamesUnpriceableRows(t *testing.T) {
	withSweepRowPricer(t, func(_ context.Context, region, instanceType string) (aws.DisplayPrice, error) {
		if instanceType == "c5.xlarge" {
			// Source carries truffle's raw PriceSource value ("live"/"static");
			// SourceLabel() is what renders "static" as "static fallback".
			return aws.DisplayPrice{PricePerHour: 0.17, Source: "static"}, nil
		}
		return aws.DisplayPrice{}, fmt.Errorf("no price for %s in %s", instanceType, region)
	})

	oldBudget := budget
	budget = 1000 // generous, so the floor "fits" and the partial caveat must fire
	t.Cleanup(func() { budget = oldBudget })

	out := captureStderr(t, func() {
		e, err := estimateAndReportSweep(context.Background(), sweepParams(
			map[string]interface{}{"name": "cpu", "instance_type": "c5.xlarge"},
			map[string]interface{}{"name": "gpu", "instance_type": "p5e.48xlarge"},
			map[string]interface{}{"name": "gpu2", "instance_type": "p5e.48xlarge"},
		))
		if err != nil {
			t.Errorf("a mixed sweep should still estimate: %v", err)
			return
		}
		if !e.Partial() {
			t.Error("estimate must be partial when a row could not be priced")
		}
		if got := e.ComputeCost; got < 0.169 || got > 0.171 {
			t.Errorf("only the priceable row should count (0.17), got %.4f", got)
		}
	})

	for _, want := range []string{
		"p5e.48xlarge@us-east-1", // the row is named
		"FLOOR",                  // the total is labelled
		"1 static fallback",      // provenance of what did price
	} {
		if !strings.Contains(out, want) {
			t.Errorf("preview must contain %q, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "✓ Within budget") {
		t.Errorf("a partial total must not produce an unqualified within-budget pass, got:\n%s", out)
	}
	if !strings.Contains(out, "Cannot confirm this sweep is within budget") {
		t.Errorf("a partial total under budget must say it cannot be confirmed, got:\n%s", out)
	}
}

func TestReportSweepBudget(t *testing.T) {
	oldBudget := budget
	t.Cleanup(func() { budget = oldBudget })

	tests := []struct {
		name      string
		budgetVal float64
		total     float64
		partial   bool
		want      string
		notWant   string
	}{
		{name: "no budget set prints nothing", budgetVal: 0, total: 50, want: ""},
		{name: "over budget warns", budgetVal: 10, total: 25, want: "exceeds budget"},
		{name: "under budget and complete confirms", budgetVal: 100, total: 25, want: "✓ Within budget"},
		{
			name: "under budget but partial cannot confirm", budgetVal: 100, total: 25, partial: true,
			want: "Cannot confirm", notWant: "✓ Within budget",
		},
		{
			// Over budget is decisive even when partial: a floor already above the
			// budget can only get worse, so the warning stands unqualified.
			name: "over budget while partial still warns", budgetVal: 10, total: 25, partial: true,
			want: "exceeds budget", notWant: "Cannot confirm",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			budget = tt.budgetVal
			out := captureStderr(t, func() { reportSweepBudget(tt.total, tt.partial) })
			if tt.want == "" {
				if strings.TrimSpace(out) != "" {
					t.Errorf("expected no output, got %q", out)
				}
				return
			}
			if !strings.Contains(out, tt.want) {
				t.Errorf("expected %q in output, got %q", tt.want, out)
			}
			if tt.notWant != "" && strings.Contains(out, tt.notWant) {
				t.Errorf("did not expect %q in output, got %q", tt.notWant, out)
			}
		})
	}
}
