package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/provenance"
)

func renderRetentionString(rep *provenance.Report, objects bool) string {
	var b bytes.Buffer
	renderRetention(&b, rep, objects)
	return b.String()
}

// The report must say what it is (read only), label every dollar figure's
// source, list each reason a run can't be proven, and say when it's incomplete.
func TestRenderRetentionSaysWhatItIsAndWhatItDoesntKnow(t *testing.T) {
	rep := &provenance.Report{Bucket: "b", PriceSource: provenance.StoragePriceSource}
	rep.Totals.Objects, rep.Totals.Bytes, rep.Totals.ProvableRuns = 3, 3<<30, 1
	rep.Runs = []provenance.RunSummary{
		{RunID: "aaaaaaaa-1", TaskID: "t1", Class: provenance.ClassCheap, RegenUSD: "0.0500", RegenHours: "0.5000",
			Regenerable: true, Objects: 1, Bytes: 1 << 30, Provable: true},
		{RunID: "bbbbbbbb-2", TaskID: "t2", RegenUSD: "3.0000", RegenHours: "2.0000", Class: provenance.ClassModerate,
			RateSource: provenance.RateSourceOnDemandStatic, LowerBound: true, Objects: 1, Bytes: 1 << 30,
			WhyNot: []string{"not opted in: the task spec didn't set regenerable: true", "input s3://in/x: gone"}},
	}
	rep.Errors = []string{"s3://b/k: read tags: AccessDenied"}
	out := renderRetentionString(rep, false)
	for _, want := range []string{
		"nothing is tiered, tagged or deleted",
		"Storage $ are static us-east-1",
		provenance.StoragePricesAsOf,
		"bbbbbbbb can't be proven regenerable",
		"- not opted in",
		"- input s3://in/x: gone",
		"STATIC",
		"truffle#184",
		"lower bound",
		"INCOMPLETE: 1 object(s)",
		"could prove regenerable today: 1 of 2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "aaaaaaaa can't be proven") {
		t.Errorf("a provable run was listed as unprovable:\n%s", out)
	}
}

// An empty listing must not read as "nothing to worry about": task outputs
// usually go to the spec's own destination, not the default bucket.
func TestRenderRetentionEmptySaysWhereToLook(t *testing.T) {
	out := renderRetentionString(&provenance.Report{Bucket: "spawn-results-1-us-east-1"}, false)
	if !strings.Contains(out, "No objects under") || !strings.Contains(out, "--bucket") {
		t.Errorf("empty report:\n%s", out)
	}
}

func TestUSDLabelNeverRoundsSomethingToZero(t *testing.T) {
	for v, want := range map[float64]string{0: "$0.00", 0.000001: "<$0.0001", 0.0042: "$0.0042", 1.5: "$1.50"} {
		if got := usdLabel(v); got != want {
			t.Errorf("usdLabel(%v) = %q, want %q", v, got, want)
		}
	}
}
