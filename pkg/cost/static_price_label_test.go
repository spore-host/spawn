package cost

import (
	"context"
	"strings"
	"testing"
	"time"

	libpricing "github.com/spore-host/libs/pricing"
	"github.com/spore-host/spawn/pkg/testutil"
	truffleaws "github.com/spore-host/truffle/pkg/aws"
)

const staticTestReason = "live AWS Price List unavailable: test outage"

// originFakePricer reports a rate with a full truffle origin, as
// *truffleaws.Client does.
type originFakePricer struct {
	price  float64
	origin truffleaws.PriceOrigin
}

func (p originFakePricer) OnDemandPrice(context.Context, string, string) (float64, error) {
	return p.price, nil
}

func (p originFakePricer) OnDemandPriceWithOrigin(context.Context, string, string) (float64, truffleaws.PriceOrigin, error) {
	return p.price, p.origin, nil
}

// staticPriceRenderers is pkg/cost's half of the #1502/#1510 gate (see
// internal/pricehygiene): each entry prices a sweep through the real function
// with a static rate and returns what it hands the renderer.
var staticPriceRenderers = map[string]func(t *testing.T, origin truffleaws.PriceOrigin) string{
	"Client.GetCostBreakdown": func(t *testing.T, origin truffleaws.PriceOrigin) string {
		env := testutil.SubstrateServer(t)
		setupCostTable(t, env.DynamoClient())
		client := NewClient(env.AWSConfig)
		client.SetOnDemandPricer(originFakePricer{price: 0.17, origin: origin})
		launchedAt := time.Now().Add(-2 * time.Hour).Format(time.RFC3339)
		seedSweepRecord(t, env.DynamoClient(), SweepRecord{
			SweepID: "sweep-static",
			Instances: []SweepInstance{
				{Index: 0, Region: "us-east-1", InstanceID: "i-0001", RequestedType: "c5.xlarge", State: "running", LaunchedAt: launchedAt},
				{Index: 1, Region: "us-east-1", InstanceID: "i-0002", RequestedType: "c5.xlarge", State: "running", LaunchedAt: launchedAt},
			},
		})
		b, err := client.GetCostBreakdown(context.Background(), "sweep-static")
		if err != nil {
			t.Fatalf("GetCostBreakdown: %v", err)
		}
		if b.ComputeCost <= 0 {
			t.Fatalf("ComputeCost = %v; the static rate was not used, so this proves nothing", b.ComputeCost)
		}
		if len(b.StaticPriceNotes) != 1 {
			t.Errorf("StaticPriceNotes = %q, want one note for one distinct static origin", b.StaticPriceNotes)
		}
		return strings.Join(b.StaticPriceNotes, "\n")
	},
}

// TestStaticComputeRateIsLabelledInTheBreakdown: a sweep priced from truffle's
// static table carries, on the breakdown, that the rate is static, why and
// when the table was collected (#1510).
func TestStaticComputeRateIsLabelledInTheBreakdown(t *testing.T) {
	for name, render := range staticPriceRenderers {
		got := render(t, truffleaws.StaticPriceOrigin(staticTestReason))
		for _, want := range []string{"static", "collected " + libpricing.PricesAsOf, staticTestReason} {
			if !strings.Contains(got, want) {
				t.Errorf("%s: %q is missing %q (static, why, when — #1510)", name, got, want)
			}
		}
	}
}

// TestLiveComputeRateAddsNoStaticNote: a live rate, and a pricer that cannot
// report an origin, produce no static note.
func TestLiveComputeRateAddsNoStaticNote(t *testing.T) {
	for _, origin := range []truffleaws.PriceOrigin{truffleaws.LivePriceOrigin(), {}} {
		{
			env := testutil.SubstrateServer(t)
			setupCostTable(t, env.DynamoClient())
			client := NewClient(env.AWSConfig)
			client.SetOnDemandPricer(originFakePricer{price: 0.17, origin: origin})
			seedSweepRecord(t, env.DynamoClient(), SweepRecord{SweepID: "sweep-live", Instances: []SweepInstance{
				{Region: "us-east-1", InstanceID: "i-0001", RequestedType: "c5.xlarge", State: "running", LaunchedAt: time.Now().Add(-time.Hour).Format(time.RFC3339)},
			}})
			b, err := client.GetCostBreakdown(context.Background(), "sweep-live")
			if err != nil {
				t.Fatal(err)
			}
			if len(b.StaticPriceNotes) != 0 {
				t.Errorf("origin %+v: StaticPriceNotes = %q, want none", origin, b.StaticPriceNotes)
			}
		}
	}
}
