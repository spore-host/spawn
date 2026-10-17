package aws

import (
	"strings"
	"testing"
	"time"

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/spore-host/spawn/pkg/ttl"
)

func mustBuildTags(t *testing.T, cfg LaunchConfig, accountID, userARN, accountNameSlug string) []ec2types.Tag {
	t.Helper()
	tags, err := buildTags(cfg, accountID, userARN, accountNameSlug)
	if err != nil {
		t.Fatalf("buildTags: %v", err)
	}
	return tags
}

// acceptedTTLForms is every spelling class the shared parser accepts, built
// from ttl.Units() rather than a hand-written list: each unit as an integer and
// as a fraction, plus the multi-segment and mixed-day forms users type.
func acceptedTTLForms(t *testing.T) []string {
	t.Helper()
	units := ttl.Units()
	if len(units) == 0 {
		t.Fatal("ttl.Units() is empty; discovery is broken, not the parser clean")
	}
	var forms []string
	for _, u := range units {
		forms = append(forms, "3"+u, "1.5"+u)
	}
	forms = append(forms, "2d12h", "1d30m", "1h30m", "12h1d", "1d2h30m15s")
	return forms
}

// #990: every TTL the parser accepts must produce a spawn:ttl-deadline, and the
// deadline must be launch + the parsed TTL. buildTags used to write spawn:ttl
// unconditionally and the deadline only when time.ParseDuration succeeded, so
// "1d" got a TTL tag and no deadline. A source-level gate cannot see that a
// write is conditional on the value; this test can.
func TestBuildTags_EveryAcceptedTTLFormYieldsADeadline(t *testing.T) {
	for _, form := range acceptedTTLForms(t) {
		t.Run(form, func(t *testing.T) {
			want, err := ttl.ParseTTL(form)
			if err != nil {
				t.Fatalf("fixture %q is not an accepted TTL: %v", form, err)
			}
			before := time.Now().Truncate(time.Second)
			tags := mustBuildTags(t, LaunchConfig{Name: "t", TTL: form}, "123456789012", "arn:aws:iam::1:user/u", "")
			after := time.Now()

			dl := findTagValue(tags, "spawn:ttl-deadline")
			if dl == "" {
				t.Fatalf("--ttl %s: spawn:ttl-deadline not written; only the reaper's max-age ceiling would bound the instance", form)
			}
			deadline, err := time.Parse(time.RFC3339, dl)
			if err != nil {
				t.Fatalf("spawn:ttl-deadline %q: %v", dl, err)
			}
			// RFC3339 drops sub-second precision, so allow one second either side.
			lo, hi := before.Add(want).Add(-time.Second), after.Add(want).Add(time.Second)
			if deadline.Before(lo) || deadline.After(hi) {
				t.Errorf("--ttl %s: deadline %s, want launch + %s (between %s and %s)", form, dl, want, lo, hi)
			}

			// spawn:ttl must be present and must mean the same duration to a
			// reader that only knows time.ParseDuration (older spored, dashboard).
			v := findTagValue(tags, "spawn:ttl")
			got, err := time.ParseDuration(v)
			if err != nil || got != want {
				t.Errorf("--ttl %s: spawn:ttl = %q, which time.ParseDuration reads as %v, %v; want %v", form, v, got, err, want)
			}
		})
	}
}

// buildTags must refuse — not quietly omit the deadline — when the TTL cannot
// be enforced.
func TestBuildTags_RefusesATTLItCannotEnforce(t *testing.T) {
	for _, bad := range []string{"1w", "2weeks", "-1h", "0", "0d", "1D", "8 h", "999999999999d"} {
		tags, err := buildTags(LaunchConfig{Name: "t", TTL: bad}, "123456789012", "arn:aws:iam::1:user/u", "")
		if err == nil {
			t.Errorf("--ttl %q: buildTags returned no error (spawn:ttl=%q, spawn:ttl-deadline=%q)",
				bad, findTagValue(tags, "spawn:ttl"), findTagValue(tags, "spawn:ttl-deadline"))
			continue
		}
		if tags != nil {
			t.Errorf("--ttl %q: buildTags returned tags alongside its error", bad)
		}
	}
}

// spored ignores a duration tag it cannot parse, which for spawn:idle-timeout
// means no idle timeout at all. Day units are normalized so every reader agrees.
func TestBuildTags_LifecycleDurations(t *testing.T) {
	tags := mustBuildTags(t, LaunchConfig{
		Name: "t", IdleTimeout: "1d", CompletionDelay: "30s", OnComplete: "terminate",
		PreStop: "sync", PreStopTimeout: "5m",
	}, "123456789012", "arn:aws:iam::1:user/u", "")
	if v := findTagValue(tags, "spawn:idle-timeout"); v != "24h0m0s" {
		t.Errorf("spawn:idle-timeout = %q, want 24h0m0s", v)
	}
	if v := findTagValue(tags, "spawn:pre-stop-timeout"); v != "5m" {
		t.Errorf("spawn:pre-stop-timeout = %q, want it unchanged", v)
	}
	for _, cfg := range []LaunchConfig{
		{Name: "t", IdleTimeout: "1w"},
		{Name: "t", CompletionDelay: "soon"},
		{Name: "t", PreStop: "sync", PreStopTimeout: "5"},
		{Name: "t", CompletionWebhookURL: "https://example.invalid/hook", WebhookTimeout: "2"},
	} {
		if _, err := buildTags(cfg, "123456789012", "arn:aws:iam::1:user/u", ""); err == nil {
			t.Errorf("buildTags(%+v) accepted an unparseable duration", cfg)
		}
	}
}

func TestValidateDurations(t *testing.T) {
	ok := []LaunchConfig{
		{},
		{TTL: "1d"}, {TTL: "2d12h"}, {TTL: "8h"},
		{IdleTimeout: "30m"}, {IdleTimeout: "1d"},
		{FSxTTL: "7d"}, {PreStopTimeout: "90s"}, {CompletionDelay: "0"}, {WebhookTimeout: "2s"},
	}
	for _, c := range ok {
		if err := c.ValidateDurations(); err != nil {
			t.Errorf("%+v: %v", c, err)
		}
	}
	bad := map[string]LaunchConfig{
		"TTL":              {TTL: "1w"},
		"zero TTL":         {TTL: "0"},
		"idle timeout":     {IdleTimeout: "forever"},
		"zero idle":        {IdleTimeout: "0s"},
		"FSx TTL":          {FSxTTL: "7days"},
		"pre-stop timeout": {PreStopTimeout: "5"},
		"completion delay": {CompletionDelay: "-1s"},
		"webhook timeout":  {WebhookTimeout: "2 s"},
	}
	for name, c := range bad {
		err := c.ValidateDurations()
		if err == nil {
			t.Errorf("%s: %+v accepted", name, c)
			continue
		}
		if !strings.Contains(err.Error(), "invalid") {
			t.Errorf("%s: error %q does not say what is invalid", name, err)
		}
	}
}

// Launch must reject an unparseable TTL before its first AWS call. A zero
// Client has no AWS config at all, so reaching any AWS call would fail with a
// different error (or panic); getting the validation error proves the check
// runs first.
func TestLaunchRejectsAnUnparseableTTLBeforeAnyAWSCall(t *testing.T) {
	var c Client
	_, err := c.Launch(t.Context(), LaunchConfig{Name: "t", TTL: "1w"})
	if err == nil || !strings.Contains(err.Error(), `invalid TTL "1w"`) {
		t.Fatalf("Launch err = %v, want the TTL validation error", err)
	}
}
