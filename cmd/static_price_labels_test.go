package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/testutil"
	truffleaws "github.com/spore-host/truffle/pkg/aws"
)

// staticTestReason is the fallback reason staticTestPrice carries, so a test
// can check it reaches the output.
const staticTestReason = "live AWS Price List unavailable: test outage"

// staticTestPrice is a static rate built the way truffle builds one
// (StaticPriceOrigin: libs/pricing's collection date plus the reason).
func staticTestPrice(rate float64) aws.DisplayPrice {
	return aws.NewDisplayPrice(rate, truffleaws.StaticPriceOrigin(staticTestReason))
}

// staticPriceRenderers is cmd's half of the #1502 gate: every cmd function
// that shows a quoted on-demand rate, each rendering dp through the real
// function. internal/pricehygiene discovers the display sites from the AST and
// fails if one is missing here (or explained there), so a new site can't
// skip TestStaticPriceIsLabelledAtEveryDisplaySite.
var staticPriceRenderers = map[string]func(t *testing.T, dp aws.DisplayPrice) string{
	"renderLaunchConfirmation": func(t *testing.T, dp aws.DisplayPrice) string {
		p := launchPlan{InstanceType: "m7i.large", Region: "us-east-1", TTL: "1h"}
		p.setPrice(dp)
		var b bytes.Buffer
		renderLaunchConfirmation(&b, p)
		return b.String()
	},
	"renderLaunchPlanTable": func(t *testing.T, dp aws.DisplayPrice) string {
		p := launchPlan{InstanceType: "m7i.large", Region: "us-east-1", TTL: "1h"}
		p.setPrice(dp)
		var b bytes.Buffer
		renderLaunchPlanTable(&b, p)
		return b.String()
	},
	"renderInstanceCostEstimate": func(t *testing.T, dp aws.DisplayPrice) string {
		var b bytes.Buffer
		renderInstanceCostEstimate(&b, "m7i.large", "us-east-1", "linux", dp, "1h", 1)
		return b.String()
	},
	"renderServiceDryRun": func(t *testing.T, dp aws.DisplayPrice) string {
		resetServiceFlags(t)
		serviceInstanceType, serviceTTL, spawnRegion = "m7i.large", "1h", "us-east-1"
		withFakeServiceDryRunPrice(t, dp, nil)
		var b bytes.Buffer
		if err := renderServiceDryRun(context.Background(), &b, []string{"./svc"}, "--addr 127.0.0.1:0"); err != nil {
			t.Fatalf("renderServiceDryRun: %v", err)
		}
		return b.String()
	},
	"sweepPricer.SourceSummary": func(t *testing.T, dp aws.DisplayPrice) string {
		withSweepRowPricer(t, func(context.Context, string, string) (aws.DisplayPrice, error) { return dp, nil })
		p := newSweepPricer(context.Background())
		if _, err := p.Resolve("us-east-1", "m7i.large"); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		return p.SourceSummary()
	},
}

// TestStaticPriceIsLabelledAtEveryDisplaySite renders a static rate through
// every registered display site and requires the owner rule's three parts:
// that it is static, why (truffle's fallback reason) and when (the collection
// date) — truffle#184, spawn#1502.
func TestStaticPriceIsLabelledAtEveryDisplaySite(t *testing.T) {
	if len(staticPriceRenderers) == 0 {
		t.Fatal("staticPriceRenderers is empty; the gate covers nothing")
	}
	dp := staticTestPrice(0.1008)
	if dp.AsOf == "" {
		t.Fatal("truffle's StaticPriceOrigin returned no collection date; this test would prove nothing")
	}
	for name, render := range staticPriceRenderers {
		t.Run(name, func(t *testing.T) {
			out := render(t, dp)
			if strings.TrimSpace(out) == "" {
				t.Fatalf("%s rendered nothing for a priced instance", name)
			}
			for _, want := range []string{"static", "collected " + dp.AsOf, staticTestReason} {
				if !strings.Contains(out, want) {
					t.Errorf("%s shows a static rate without %q (static, why, when — #1502):\n%s", name, want, out)
				}
			}
		})
	}
}

// TestLaunchDryRunJSONCarriesThePriceOrigin: `spawn launch --dry-run -o json`
// must carry price_as_of and price_fallback_reason next to price_source, under
// truffle's own field names, so a script reading the plan can tell a stale
// rate from a current one (#1502).
func TestLaunchDryRunJSONCarriesThePriceOrigin(t *testing.T) {
	env := testutil.SubstrateServer(t)
	seedAL2023AMIParams(t, env)
	client := aws.NewClientFromConfig(env.AWSConfig)

	dp := staticTestPrice(0.1008)
	prev := resolveLaunchDryRunPrice
	t.Cleanup(func() { resolveLaunchDryRunPrice = prev })
	resolveLaunchDryRunPrice = func(context.Context, *aws.Client, string, string) (aws.DisplayPrice, error) { return dp, nil }

	prevOS, prevOut := osFlag, spawnOutputFormat
	t.Cleanup(func() { osFlag, spawnOutputFormat = prevOS, prevOut })
	osFlag, spawnOutputFormat = "linux", "json"

	cfg := &aws.LaunchConfig{
		Name: "p", InstanceType: "m7i.large", Region: "us-east-1",
		AMI: "ami-0abc123", TTL: "1h", SecurityGroupIDs: []string{"sg-0123456789abcdef0"},
	}
	var out bytes.Buffer
	if err := runLaunchDryRun(context.Background(), &out, client, cfg); err != nil {
		t.Fatalf("runLaunchDryRun: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("dry-run JSON: %v\n%s", err, out.String())
	}
	for key, want := range map[string]string{
		"price_source":          "static",
		"price_as_of":           dp.AsOf,
		"price_fallback_reason": staticTestReason,
	} {
		if got[key] != want {
			t.Errorf("%s = %v, want %q\n%s", key, got[key], want, out.String())
		}
	}
}
