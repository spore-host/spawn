package provenance

import (
	"net/url"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

func cost(hours, rate float64) Cost {
	return NewCost(t0, t0.Add(time.Duration(hours*float64(time.Hour))), rate, RateSourceOnDemandLive, false)
}

// The owner's thresholds (2026-10-17): cheap < $1 AND < 1h; expensive >= $25 OR
// >= 12h; moderate between. Boundaries are tested on both sides.
func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		c    Cost
		want Class
	}{
		{"cheap: 30m at $0.10/h", cost(0.5, 0.10), ClassCheap},
		{"cheap fails on time: 1h exactly, $0.10", cost(1.0, 0.10), ClassModerate},
		{"cheap fails on money: 30m at $2.00/h = $1.00", cost(0.5, 2.0), ClassModerate},
		{"moderate: 3h at $1/h", cost(3, 1), ClassModerate},
		{"expensive by money: 2h at $12.50/h = $25", cost(2, 12.5), ClassExpensive},
		{"expensive by time alone: 12h at $0.01/h", cost(12, 0.01), ClassExpensive},
		{"just under both expensive bounds", cost(11.99, 2.0), ClassModerate},
	}
	for _, tc := range cases {
		got, ok := Classify(tc.c)
		if !ok || got != tc.want {
			t.Errorf("%s: Classify = (%q, %v), want %q (usd=%.4f h=%.4f)", tc.name, got, ok, tc.want, tc.c.USD, tc.c.Hours())
		}
	}
}

// Unknown rate -> no class: such an object must match no lifecycle rule.
func TestUnknownRateIsUnclassifiedAndUntiered(t *testing.T) {
	for _, c := range []Cost{
		NewCost(t0, t0.Add(10*time.Minute), 0, RateSourceOnDemand, false),
		NewCost(t0, t0.Add(10*time.Minute), 0.5, RateSourceUnknown, false),
		NewCost(t0, t0.Add(48*time.Hour), 0, RateSourceUnknown, false), // long, but unknown: still no class
	} {
		if cls, ok := Classify(c); ok {
			t.Errorf("unknown-rate cost classified as %q", cls)
		}
		tags := Record{RunID: "r", Cost: c}.Tags()
		for _, k := range []string{TagClass, TagUSD, TagHours} {
			if _, has := tags[k]; has {
				t.Errorf("unknown-rate record carries %s=%q", k, tags[k])
			}
		}
	}
}

func TestNewCostClampsSkewAndComputesUSD(t *testing.T) {
	if c := NewCost(t0, t0.Add(-time.Minute), 1, RateSourceOnDemand, false); c.ComputeSeconds != 0 || c.USD != 0 {
		t.Errorf("end before start = %+v, want 0s / $0", c)
	}
	c := NewCost(t0, t0.Add(90*time.Minute), 0.40, RateSourceSpot, false)
	if c.USD != 0.60 || c.RateSource != RateSourceSpot {
		t.Errorf("90m at $0.40/h = %+v, want $0.60 spot", c)
	}
}

func TestTags(t *testing.T) {
	r := Record{RunID: "run-1", Regenerable: true, Cost: cost(0.5, 0.10)}
	tags := r.Tags()
	if tags[TagClass] != "cheap" || tags[TagRunID] != "run-1" || tags[TagRegenerable] != "true" || tags[TagUSD] != "0.0500" || tags[TagHours] != "0.5000" {
		t.Errorf("tags = %v", tags)
	}
	if len(tags) > 10 {
		t.Errorf("%d tags; S3 allows 10", len(tags))
	}
	// Not opted in: no regenerable tag at all, so a delete rule keyed on it can't match.
	if _, has := (Record{RunID: "r", Cost: cost(0.5, 0.1)}).Tags()[TagRegenerable]; has {
		t.Error("a record without the opt-in carries spawn:regenerable")
	}
	// The header form round-trips.
	q, err := url.ParseQuery(EncodeTags(tags))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range tags {
		if q.Get(k) != v {
			t.Errorf("x-amz-tagging lost %s=%s: %q", k, v, EncodeTags(tags))
		}
	}
}

// Pinned against the real `aws s3 cp` on a throwaway bucket (2026-10-17).
func TestObjectKeyMatchesAWSS3Cp(t *testing.T) {
	cases := []struct {
		dest, rel string
		recursive bool
		want      string
	}{
		{"r1/dest", "a.txt", true, "r1/dest/a.txt"},
		{"r1/dest", "sub/b.txt", true, "r1/dest/sub/b.txt"},
		{"r2/dest/", "a.txt", true, "r2/dest/a.txt"},
		{"r2/dest/", "sub/b.txt", true, "r2/dest/sub/b.txt"},
		{"", "sub/b.txt", true, "sub/b.txt"},
		{"f1/named.txt", "file.txt", false, "f1/named.txt"},
		{"f2/prefix/", "file.txt", false, "f2/prefix/file.txt"},
	}
	for _, tc := range cases {
		if got := ObjectKey(tc.dest, tc.rel, tc.recursive); got != tc.want {
			t.Errorf("ObjectKey(%q, %q, %v) = %q, want %q (what aws s3 cp wrote)", tc.dest, tc.rel, tc.recursive, got, tc.want)
		}
	}
}

func TestSplitS3URI(t *testing.T) {
	if b, k, err := SplitS3URI("s3://bkt/a/b/"); err != nil || b != "bkt" || k != "a/b/" {
		t.Errorf("= (%q, %q, %v)", b, k, err)
	}
	if b, k, err := SplitS3URI("s3://bkt"); err != nil || b != "bkt" || k != "" {
		t.Errorf("bucket only = (%q, %q, %v)", b, k, err)
	}
	for _, bad := range []string{"", "s3://", "/local/path", "https://x"} {
		if _, _, err := SplitS3URI(bad); err == nil {
			t.Errorf("SplitS3URI(%q) accepted", bad)
		}
	}
}

func TestRateFromTags(t *testing.T) {
	cases := []struct {
		name   string
		tags   map[string]string
		rate   float64
		source string
		lower  bool
	}{
		{"spot price paid wins", map[string]string{"spawn:spot-price-per-hour": "0.03", "spawn:price-per-hour": "0.10", "spawn:price-source": "live"}, 0.03, RateSourceSpot, false},
		{"on-demand live", map[string]string{"spawn:price-per-hour": "0.10", "spawn:price-source": "live"}, 0.10, RateSourceOnDemandLive, false},
		{"on-demand static is labelled static", map[string]string{"spawn:price-per-hour": "0.10", "spawn:price-source": "static"}, 0.10, RateSourceOnDemandStatic, false},
		{"pre-#1076 instance: source not recorded", map[string]string{"spawn:price-per-hour": "0.10"}, 0.10, RateSourceOnDemand, false},
		{"windows is a lower bound", map[string]string{"spawn:price-per-hour": "0.20", "spawn:os": "windows"}, 0.20, RateSourceOnDemand, true},
		{"no rate", map[string]string{}, 0, RateSourceUnknown, false},
		{"garbage rate", map[string]string{"spawn:price-per-hour": "n/a"}, 0, RateSourceUnknown, false},
	}
	for _, tc := range cases {
		r, s, l := RateFromTags(tc.tags)
		if r != tc.rate || s != tc.source || l != tc.lower {
			t.Errorf("%s: = (%v, %q, %v), want (%v, %q, %v)", tc.name, r, s, l, tc.rate, tc.source, tc.lower)
		}
	}
}
