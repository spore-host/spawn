package main

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"

	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/pipeline"
	truffleaws "github.com/spore-host/truffle/pkg/aws"
)

const staticTestReason = "live AWS Price List unavailable: test outage"

// staticPriceRenderers is the pipeline orchestrator's half of the #1502 gate
// (see internal/pricehygiene): each entry shows a static rate through the real
// function — here the stage state the orchestrator saves, plus its log line.
var staticPriceRenderers = map[string]func(t *testing.T, dp spawnaws.DisplayPrice) string{
	"ensureStagePrice": func(t *testing.T, dp spawnaws.DisplayPrice) string {
		old := priceFn
		t.Cleanup(func() { priceFn = old })
		priceFn = func(context.Context, string, string) (spawnaws.DisplayPrice, error) { return dp, nil }

		var logBuf bytes.Buffer
		oldOut, oldFlags := log.Writer(), log.Flags()
		log.SetOutput(&logBuf)
		log.SetFlags(0)
		t.Cleanup(func() { log.SetOutput(oldOut); log.SetFlags(oldFlags) })

		state := &pipeline.PipelineState{}
		st := &pipeline.StageState{StageID: "train"}
		if err := ensureStagePrice(context.Background(), state, &pipeline.Stage{StageID: "train", InstanceType: "m7i.large"}, st); err != nil {
			t.Fatal(err)
		}
		if st.PricePerHourUSD != dp.PricePerHour {
			t.Fatalf("stage price = %v, want %v", st.PricePerHourUSD, dp.PricePerHour)
		}
		return "state price_source: " + st.PriceSource + "\nlog: " + logBuf.String()
	},
}

// TestStaticStagePriceSaysWhyAndWhen: a stage priced from truffle's static
// table records, in the saved state and the log, that the rate is static, why
// and when the table was collected (#1502).
func TestStaticStagePriceSaysWhyAndWhen(t *testing.T) {
	dp := spawnaws.NewDisplayPrice(0.1008, truffleaws.StaticPriceOrigin(staticTestReason))
	for name, render := range staticPriceRenderers {
		out := render(t, dp)
		state, logLine, _ := strings.Cut(out, "\nlog: ")
		for _, part := range []string{state, logLine} {
			for _, want := range []string{"static", "collected " + dp.AsOf, staticTestReason} {
				if !strings.Contains(part, want) {
					t.Errorf("%s: %q is missing %q (static, why, when — #1502)", name, part, want)
				}
			}
		}
	}
}
