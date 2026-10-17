package aws

import (
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/spotrequest"
	"github.com/spore-host/spawn/pkg/ttl"
)

// #950: a persistent Spot request has no end date unless one is sent, so a
// request orphaned by a terminate outside spawn (the console,
// `aws ec2 terminate-instances`) would keep launching replacements for ever.
// spawn sets ValidUntil to the TTL deadline: the instant the instance itself
// stops being allowed to exist.
func TestLaunch_PersistentSpotValidUntilIsTheTTLDeadline(t *testing.T) {
	before := time.Now().UTC().Add(-time.Second)
	run := launchSpot(t, LaunchConfig{Spot: true, TTL: "3h", IdleTimeout: "15m"})
	after := time.Now().UTC().Add(time.Second)

	got, err := time.Parse(time.RFC3339, run.Get("InstanceMarketOptions.SpotOptions.ValidUntil"))
	if err != nil {
		t.Fatalf("ValidUntil not sent or unparseable (%q): %v", run.Get("InstanceMarketOptions.SpotOptions.ValidUntil"), err)
	}
	if got.Before(before.Add(3*time.Hour).Truncate(time.Second)) || got.After(after.Add(3*time.Hour)) {
		t.Errorf("ValidUntil = %s, want launch + 3h", got)
	}
	deadline := tagValue(run, "instance", "spawn:ttl-deadline")
	if deadline == "" || deadline != got.Format(time.RFC3339) {
		t.Errorf("ValidUntil %s must be the instance's spawn:ttl-deadline %q, the same instant", got.Format(time.RFC3339), deadline)
	}
	for _, spec := range []string{"instance", "spot-instances-request"} {
		if v := tagValue(run, spec, spotrequest.ValidUntilTag); v != got.Format(time.RFC3339) {
			t.Errorf("%s tag %s = %q, want %s", spec, spotrequest.ValidUntilTag, v, got.Format(time.RFC3339))
		}
	}
}

// With no TTL there is no deadline. The request is bounded by the reaper's
// max-age ceiling instead, which reaps the instance at that age anyway.
func TestLaunch_PersistentSpotValidUntilWithoutTTLIsTheCeiling(t *testing.T) {
	before := time.Now().UTC()
	run := launchSpot(t, LaunchConfig{Spot: true, IdleTimeout: "15m"})
	got, err := time.Parse(time.RFC3339, run.Get("InstanceMarketOptions.SpotOptions.ValidUntil"))
	if err != nil {
		t.Fatalf("ValidUntil not sent: %v", err)
	}
	want := before.Add(ttl.DefaultReaperMaxAge)
	if d := got.Sub(want); d < -2*time.Second || d > 2*time.Second {
		t.Errorf("ValidUntil = %s, want launch + %s (%s)", got, ttl.DefaultReaperMaxAge, want)
	}
}

// One-time requests don't support ValidUntil (EC2 rejects it) and close with
// their instance anyway.
func TestLaunch_OneTimeSpotSendsNoValidUntil(t *testing.T) {
	run := launchSpot(t, LaunchConfig{Spot: true, TTL: "1h"})
	if v := run.Get("InstanceMarketOptions.SpotOptions.ValidUntil"); v != "" {
		t.Errorf("one-time request sent ValidUntil=%q", v)
	}
	if v := tagValue(run, "instance", spotrequest.ValidUntilTag); v != "" {
		t.Errorf("one-time instance tagged %s=%q", spotrequest.ValidUntilTag, v)
	}
}

func TestDefaultMaxLifetimeMatchesReaperCeiling(t *testing.T) {
	if spotrequest.DefaultMaxLifetime != ttl.DefaultReaperMaxAge {
		t.Errorf("spotrequest.DefaultMaxLifetime = %s, ttl.DefaultReaperMaxAge = %s: a persistent request "+
			"without a TTL must be bounded by the same ceiling the reaper enforces", spotrequest.DefaultMaxLifetime, ttl.DefaultReaperMaxAge)
	}
}

// After RunInstances the persistent request is tagged with the instance it
// launched, so the reaper can tell a replacement apart (#950).
func TestLaunch_PersistentSpotRequestNamesItsOriginalInstance(t *testing.T) {
	f := &fakeEC2{runSpotRequestID: "sir-orig"}
	launchWith(t, f, LaunchConfig{Spot: true, TTL: "1h", IdleTimeout: "15m"})
	ct := f.request("CreateTags")
	if ct == nil {
		t.Fatalf("no CreateTags after a persistent launch (actions %v)", f.actions())
	}
	if ct.Get("ResourceId.1") != "sir-orig" || ct.Get("Tag.1.Key") != spotrequest.OriginalInstanceTag || ct.Get("Tag.1.Value") != "i-new" {
		t.Errorf("CreateTags = %v, want sir-orig tagged %s=i-new", ct, spotrequest.OriginalInstanceTag)
	}

	// One-time and on-demand: nothing to tag.
	for _, lc := range []LaunchConfig{{Spot: true, TTL: "1h"}, {TTL: "1h", IdleTimeout: "15m"}} {
		f := &fakeEC2{runSpotRequestID: "sir-x"}
		launchWith(t, f, lc)
		if strings.Contains(strings.Join(f.actions(), ","), "CreateTags") {
			t.Errorf("%+v: tagged a request that is not persistent", lc)
		}
	}
}

// tagValue returns the value of key in the TagSpecification of specType.
func tagValue(form map[string][]string, specType, key string) string {
	get := func(k string) string {
		if v := form[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	for i := 1; ; i++ {
		rt := get("TagSpecification." + itoa(i) + ".ResourceType")
		if rt == "" {
			return ""
		}
		if rt != specType {
			continue
		}
		for j := 1; ; j++ {
			k := get("TagSpecification." + itoa(i) + ".Tag." + itoa(j) + ".Key")
			if k == "" {
				break
			}
			if k == key {
				return get("TagSpecification." + itoa(i) + ".Tag." + itoa(j) + ".Value")
			}
		}
	}
}
