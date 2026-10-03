package cmd

import (
	"bytes"
	"strings"
	"testing"
)

// The bug these tests guard (spawn#613): --estimate-only printed the instance's
// hourly and TTL cost and nothing else, so a launch that was about to create a
// 1200 GiB FSx Lustre filesystem (~$174/month — two orders of magnitude more than
// the "$0.80 TTL cost" it did print) previewed as if that filesystem did not
// exist. Pure renderer, no AWS.

func TestRenderFSxCostEstimate_NamesTheFilesystemAndItsCost(t *testing.T) {
	plan, ok := fsxCreatePlanFromFlags(true, 1200, 125, "ephemeral", "", "/mnt/fsx")
	if !ok {
		t.Fatal("fsxCreatePlanFromFlags(create=true) returned ok=false; the preview would print nothing")
	}

	var buf bytes.Buffer
	renderFSxCostEstimate(&buf, plan)
	out := buf.String()

	for _, want := range []string{
		"FSx Lustre",
		"1200 GiB",      // the capacity it would provision
		"125 MB/s/TiB",  // the throughput tier the price depends on
		"$174.00/month", // 1200 * 0.145 — the figure the user never saw
		"--cost-limit",  // the caveat, at the point of use
		"spawn fsx list",
		"spawn fsx delete",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("estimate is missing %q; got:\n%s", want, out)
		}
	}
	// The caveat must state the cap's REACH, not merely mention the flag.
	//
	// This assertion used to require "does NOT cover", which was the truth when
	// --cost-limit was compute-only. #616 made the cap a total — it now counts this
	// filesystem and refuses the launch if the storage exceeds the cap on its own — so
	// the old wording would be the lie. What has NOT changed, and is the thing a user
	// still needs told, is that the cap cannot RECLAIM storage that outlives the
	// instance: spored enforces the cap from inside, and the filesystem survives it.
	if !strings.Contains(out, "cannot RECLAIM") {
		t.Errorf("estimate mentions --cost-limit but never says it cannot reclaim the filesystem; got:\n%s", out)
	}
	if strings.Contains(out, "does NOT cover") {
		t.Errorf("estimate still claims --cost-limit does not cover the filesystem, which stopped being "+
			"true in #616; got:\n%s", out)
	}
	// Ephemeral must not be described as deleted by terminate.
	if !strings.Contains(out, "asynchronously") {
		t.Errorf("ephemeral lifecycle line does not say reclamation is asynchronous; got:\n%s", out)
	}
}

func TestFSxCreatePlanFromFlags_NoFilesystemMeansNoLine(t *testing.T) {
	if _, ok := fsxCreatePlanFromFlags(false, 1200, 125, "", "", "/fsx"); ok {
		t.Fatal("a launch without --fsx-create must produce no FSx estimate line")
	}
}

func TestRenderFSxCostEstimate_DurableNamesItsTTL(t *testing.T) {
	plan, _ := fsxCreatePlanFromFlags(true, 2400, 250, "durable", "7d", "/fsx")
	var buf bytes.Buffer
	renderFSxCostEstimate(&buf, plan)
	out := buf.String()

	if !strings.Contains(out, "2400 GiB") || !strings.Contains(out, "250 MB/s/TiB") {
		t.Errorf("durable estimate lost capacity/throughput; got:\n%s", out)
	}
	if !strings.Contains(out, "$696.00/month") { // 2400 * 0.29
		t.Errorf("durable estimate priced the 250 MB/s/TiB tier wrong; got:\n%s", out)
	}
	if !strings.Contains(out, "7d") {
		t.Errorf("durable estimate does not name the TTL that bounds the bill; got:\n%s", out)
	}
}

func TestFSxCreatePlanFromFlags_MirrorsCreatePathDefaults(t *testing.T) {
	// The create path defaults a zero throughput to 125 and the flag defaults
	// capacity to 1200; a preview quoting 0 GiB @ 0 MB/s would be worse than
	// silence because it reads as "free".
	plan, ok := fsxCreatePlanFromFlags(true, 0, 0, "ephemeral", "", "")
	if !ok {
		t.Fatal("ok=false with --fsx-create set")
	}
	if plan.CapacityGiB != 1200 {
		t.Errorf("CapacityGiB = %d, want the 1200 GiB minimum the create path uses", plan.CapacityGiB)
	}
	if plan.ThroughputMBpsPerTiB != 125 {
		t.Errorf("ThroughputMBpsPerTiB = %d, want 125", plan.ThroughputMBpsPerTiB)
	}
}

func TestFSxLustreMonthlyUSD(t *testing.T) {
	for _, tc := range []struct {
		capacity   int32
		throughput int32
		want       float64
	}{
		{1200, 125, 174}, // the filesystem in spawn#613
		{1200, 250, 348}, // the tier is folded into the per-GiB price
		{1200, 999, 174}, // unknown tier falls back to the 125 rate
		{0, 125, 0},      // unknown capacity prices nothing rather than guessing
	} {
		if got := fsxLustreMonthlyUSD(tc.capacity, tc.throughput); got != tc.want {
			t.Errorf("fsxLustreMonthlyUSD(%d, %d) = %.2f, want %.2f", tc.capacity, tc.throughput, got, tc.want)
		}
	}
}
