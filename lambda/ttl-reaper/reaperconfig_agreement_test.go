package main

import (
	"testing"

	"github.com/spore-host/spawn/pkg/reaperconfig"
)

// `spawn reaper deploy` validates its flags with pkg/reaperconfig (#1154). This
// gate holds the reaper to the same definition: a value the CLI accepts must be
// one the reaper's own parsers recognise rather than silently replace with a
// default, and a value the CLI rejects must be one they fall back on.
func TestReaperParsersAgreeWithReaperconfig(t *testing.T) {
	corpus := []string{"", "off", "report", "reap", "OFF", " Reap ", "reep", "true", "yes", "auto", "delete"}
	for _, v := range corpus {
		want, ok := reaperconfig.ParseMode(v)
		if !ok {
			want = spotRequestsReport // the reaper's fallback for anything unrecognised
		}
		if got := parseSpotRequests(v); got != want {
			t.Errorf("parseSpotRequests(%q) = %q, reaperconfig says %q", v, got, want)
		}
		if got := parseNetResources(v); got != want {
			t.Errorf("parseNetResources(%q) = %q, reaperconfig says %q", v, got, want)
		}
		if got := parseEmptyBuckets(v); got != want {
			t.Errorf("parseEmptyBuckets(%q) = %q, reaperconfig says %q", v, got, want)
		}
		// REAPER_ELASTIC_IPS differs only for an unset value, which means reap
		// (TestEIPDefaultIsReap); everything else is the same contract.
		eipWant := want
		if v == "" {
			eipWant = elasticIPsReap
		}
		if got := parseElasticIPs(v, netResourcesReport); got != eipWant {
			t.Errorf("parseElasticIPs(%q) = %q, want %q", v, got, eipWant)
		}
	}

	for _, v := range []string{"", "0", "2h", "3d", "336h", "1w", "-1h", "forever", "90m"} {
		d, err := reaperconfig.ParseMaxAge(v)
		got := parseMaxAge(v)
		switch {
		case err == nil && got != d:
			t.Errorf("REAPER_MAX_AGE=%q: CLI accepts it as %s, reaper reads %s", v, d, got)
		case err != nil && got != defaultMaxAge:
			t.Errorf("REAPER_MAX_AGE=%q: CLI rejects it, reaper reads %s instead of its default", v, got)
		}
	}
}
