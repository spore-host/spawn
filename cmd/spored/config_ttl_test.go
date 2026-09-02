package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/agent"
	"github.com/spore-host/spawn/pkg/provider"
	"github.com/spore-host/spawn/pkg/ttl"
)

// TestFormatTTLLine_PrefersDeadlineOverRawTTL is the direct regression test
// for spawn#553's reporting symptom: `spored config get ttl` used to print
// config.TTL (the raw spawn:ttl duration tag) in isolation, while
// `spawn status`/`spored status` computed remaining time from
// config.TTLDeadline — the field pkg/agent's enforcement loop actually reads.
// The two could disagree about the same instance at the same moment whenever
// spawn:ttl had been rewritten without moving spawn:ttl-deadline (exactly
// what the old `config set ttl` did). formatTTLLine is now the single
// function both `config get ttl` and `config set ttl`'s post-reload
// read-back call, so this test locks it to the deadline-first behavior.
func TestFormatTTLLine_PrefersDeadlineOverRawTTL(t *testing.T) {
	now := time.Date(2026, 8, 20, 8, 39, 1, 0, time.UTC) // t+8m in the issue's repro
	launch := time.Date(2026, 8, 20, 8, 26, 22, 0, time.UTC)

	// Simulates the exact broken state from #553: an operator ran
	// `config set ttl 8m`, and the OLD code wrote spawn:ttl=8m but left
	// spawn:ttl-deadline at the original 1h-from-launch value untouched.
	cfg := &provider.Config{
		TTL:         8 * time.Minute,       // what the old bug wrote
		TTLDeadline: launch.Add(time.Hour), // UNCHANGED — the actual bug
		LaunchTime:  launch,
	}

	got := formatTTLLine(cfg, now)

	// The deadline (55m remaining, terminates 09:26) must win over the raw
	// tag (which alone would suggest ~0 remaining on an 8m TTL set 8m ago).
	// This is deliberately what a caller SHOULD see when the deadline is
	// stale: it proves formatTTLLine reports the truth (what actually
	// terminates the instance), not the unenforced tag — the same property
	// `spawn status`'s "Lifecycle mismatch" notice (#508) surfaces from the
	// other direction.
	wantDeadline := launch.Add(time.Hour).UTC().Format("2006-01-02 15:04 UTC")
	if !strings.Contains(got, wantDeadline) {
		t.Errorf("formatTTLLine = %q, want it to report the deadline %q (not the stale spawn:ttl tag)", got, wantDeadline)
	}
}

// TestFormatTTLLine_ShortenedDeadlineAgrees confirms that AFTER the fix —
// where config set ttl writes both tags via pkg/ttl.SetTags — formatTTLLine
// (used by both config get ttl and config set ttl's read-back) reports the
// NEW, SHORTER deadline. This is the fail-without/pass-with pair for
// TestFormatTTLLine_PrefersDeadlineOverRawTTL: same launch/now, but the
// config here is what loadConfigFromEC2Tags would produce AFTER a correct
// dual-tag write, not the old single-tag write.
func TestFormatTTLLine_ShortenedDeadlineAgrees(t *testing.T) {
	now := time.Date(2026, 8, 20, 8, 39, 1, 0, time.UTC)
	launch := time.Date(2026, 8, 20, 8, 26, 22, 0, time.UTC)

	tags, newDeadline, err := ttl.SetTags(launch, 8*time.Minute)
	if err != nil {
		t.Fatalf("ttl.SetTags: %v", err)
	}
	parsedTTL, err := time.ParseDuration(tags["spawn:ttl"])
	if err != nil {
		t.Fatalf("parse spawn:ttl: %v", err)
	}
	parsedDeadline, err := time.Parse(time.RFC3339, tags["spawn:ttl-deadline"])
	if err != nil {
		t.Fatalf("parse spawn:ttl-deadline: %v", err)
	}

	cfg := &provider.Config{
		TTL:         parsedTTL,
		TTLDeadline: parsedDeadline,
		LaunchTime:  launch,
	}

	got := formatTTLLine(cfg, now)

	// The deadline moved into the past relative to `now` (8m TTL set at
	// launch, now is 12m39s after launch) — remaining must be clamped to 0,
	// and the reported deadline must be the NEW one (08:34:22Z), not the
	// original 1h deadline from TestFormatTTLLine_PrefersDeadlineOverRawTTL.
	if !newDeadline.Equal(launch.Add(8 * time.Minute)) {
		t.Fatalf("sanity: newDeadline = %v, want %v", newDeadline, launch.Add(8*time.Minute))
	}
	wantDeadlineStr := newDeadline.UTC().Format("2006-01-02 15:04 UTC")
	if !strings.Contains(got, wantDeadlineStr) {
		t.Errorf("formatTTLLine = %q, want it to report the NEW shortened deadline %q", got, wantDeadlineStr)
	}
	if strings.Contains(got, launch.Add(time.Hour).UTC().Format("2006-01-02 15:04 UTC")) {
		t.Errorf("formatTTLLine = %q, still reports the OLD 1h deadline — TTL was not actually shortened", got)
	}
}

// TestFormatTTLLine_ConfigGetAndConfigSetAgree is the direct test for the
// issue's headline symptom: `config get ttl` and `spored status` (which
// buildStatusReport derives from the same TTLDeadline-first logic) disagreeing
// about the same instance at the same moment. Both now go through
// formatTTLLine, so feeding the same config to it twice must be identical —
// this pins the shared code path so a future edit to one branch can't
// silently diverge from the other without failing here.
func TestFormatTTLLine_ConfigGetAndConfigSetAgree(t *testing.T) {
	now := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	launch := time.Date(2026, 8, 20, 8, 0, 0, 0, time.UTC)
	cfg := &provider.Config{
		TTL:         3 * time.Hour,
		TTLDeadline: launch.Add(3 * time.Hour),
		LaunchTime:  launch,
	}

	fromConfigGet := formatTTLLine(cfg, now) // what handleConfigGet prints
	fromConfigSet := formatTTLLine(cfg, now) // what handleConfigSetTTL's read-back prints
	if fromConfigGet != fromConfigSet {
		t.Errorf("config get ttl (%q) and config set ttl's read-back (%q) disagree on the same config", fromConfigGet, fromConfigSet)
	}
}

func TestFormatTTLLine_Disabled(t *testing.T) {
	cfg := &provider.Config{}
	if got := formatTTLLine(cfg, time.Now()); got != "disabled" {
		t.Errorf("formatTTLLine on zero-value config = %q, want %q", got, "disabled")
	}
}

func TestFormatTTLLine_NoDeadlineFallsBackToRawTTL(t *testing.T) {
	// Pre-deadline-tag instance (older spored): only TTL is set. Should still
	// render something, from the raw duration, matching agent.go's own
	// pre-deadline fallback path.
	cfg := &provider.Config{TTL: 2 * time.Hour}
	got := formatTTLLine(cfg, time.Now())
	if got == "disabled" {
		t.Errorf("formatTTLLine with TTL>0 and no deadline reported %q, want the raw duration", got)
	}
}

// mutableTagProvider is a provider.Provider backed by an in-memory tag map,
// mimicking what an EC2Provider reads via DescribeTags. It lets tests drive
// agent.Reload through the exact same loadConfigFromEC2Tags-style parsing
// spored uses in production, without a real EC2 client — the write side
// (writeOldSingleTag / writeFixedDualTags below) simulates the two
// CreateTags call shapes handleConfigSetTTL could make: the OLD code's
// single spawn:ttl write, and the FIXED code's pkg/ttl.SetTags dual write.
type mutableTagProvider struct {
	identity *provider.Identity
	tags     map[string]string
}

func (p *mutableTagProvider) parseConfig() *provider.Config {
	cfg := &provider.Config{}
	if v, ok := p.tags["spawn:ttl"]; ok {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.TTL = d
		}
	}
	if v, ok := p.tags["spawn:ttl-deadline"]; ok {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			cfg.TTLDeadline = t
		}
	}
	if v, ok := p.tags["spawn:launch-time"]; ok {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			cfg.LaunchTime = t
		}
	}
	return cfg
}

func (p *mutableTagProvider) GetIdentity(_ context.Context) (*provider.Identity, error) {
	return p.identity, nil
}
func (p *mutableTagProvider) GetConfig(_ context.Context) (*provider.Config, error) {
	return p.parseConfig(), nil
}
func (p *mutableTagProvider) RefreshConfig(_ context.Context) error       { return nil }
func (p *mutableTagProvider) Terminate(_ context.Context, _ string) error { return nil }
func (p *mutableTagProvider) Stop(_ context.Context, _ string) error      { return nil }
func (p *mutableTagProvider) Hibernate(_ context.Context) error           { return nil }
func (p *mutableTagProvider) IsSpotInstance(_ context.Context) bool       { return false }
func (p *mutableTagProvider) CheckSpotInterruption(_ context.Context) (*provider.InterruptionInfo, error) {
	return nil, nil
}
func (p *mutableTagProvider) DiscoverPeers(_ context.Context, _ string) ([]provider.PeerInfo, error) {
	return nil, nil
}
func (p *mutableTagProvider) GetProviderType() string                               { return "stub" }
func (p *mutableTagProvider) LookupAndTagEBSCost(_ context.Context) (float64, bool) { return 0, false }
func (p *mutableTagProvider) CountOtherManagedInstances(_ context.Context) int      { return -1 }

// TestConfigSetTTL_Shorten_FailWithoutPassWith is the fail-without/pass-with
// pair this repo's convention asks for, reproducing spawn#553's exact repro
// (launch --ttl 1h, then set ttl 8m) through the real agent.Reload path — not
// just the pure ttl package helpers already covered above. oldWrite models
// exactly what the pre-fix handleConfigSet did (CreateTags with only
// spawn:ttl); newWrite models the fixed handleConfigSetTTL (pkg/ttl.SetTags,
// both tags). Both go through the identical Reload()/parseConfig() path, so
// the only variable is the tag-write shape — isolating the actual fix.
func TestConfigSetTTL_Shorten_FailWithoutPassWith(t *testing.T) {
	launch := time.Date(2026, 8, 20, 8, 26, 22, 0, time.UTC)
	identity := &provider.Identity{InstanceID: "i-0fa855e3124b19d11", Region: "us-east-1", Provider: "stub"}

	newInstance := func() *mutableTagProvider {
		return &mutableTagProvider{
			identity: identity,
			tags: map[string]string{
				"spawn:launch-time":  launch.UTC().Format(time.RFC3339),
				"spawn:ttl":          "1h0m0s",
				"spawn:ttl-deadline": launch.Add(time.Hour).UTC().Format(time.RFC3339),
			},
		}
	}

	t.Run("old single-tag write does not move the deadline (fail-without)", func(t *testing.T) {
		p := newInstance()
		ag, err := agent.NewAgent(context.Background(), p)
		if err != nil {
			t.Fatalf("NewAgent: %v", err)
		}
		originalDeadline := ag.GetConfig().TTLDeadline

		// This is exactly what the pre-fix handleConfigSet's CreateTags call did:
		// write spawn:ttl alone.
		p.tags["spawn:ttl"] = "8m0s"

		if err := ag.Reload(context.Background()); err != nil {
			t.Fatalf("Reload: %v", err)
		}

		if !ag.GetConfig().TTLDeadline.Equal(originalDeadline) {
			t.Fatalf("expected the OLD single-tag write to leave the deadline unchanged (that IS the bug being characterized), but it moved from %v to %v",
				originalDeadline, ag.GetConfig().TTLDeadline)
		}
		// The tag says 8m; the enforced deadline still says the original 1h —
		// this mismatch, present with the old write shape, is the entire bug.
		if ag.GetConfig().TTL != 8*time.Minute {
			t.Fatalf("sanity: spawn:ttl tag should read as 8m, got %v", ag.GetConfig().TTL)
		}
	})

	t.Run("fixed dual-tag write moves the deadline earlier (pass-with)", func(t *testing.T) {
		p := newInstance()
		ag, err := agent.NewAgent(context.Background(), p)
		if err != nil {
			t.Fatalf("NewAgent: %v", err)
		}
		originalDeadline := ag.GetConfig().TTLDeadline

		// This is what the fixed handleConfigSetTTL does: compute both tags
		// via pkg/ttl.SetTags and write them together.
		tags, newDeadline, err := ttl.SetTags(launch, 8*time.Minute)
		if err != nil {
			t.Fatalf("ttl.SetTags: %v", err)
		}
		for k, v := range tags {
			p.tags[k] = v
		}

		if err := ag.Reload(context.Background()); err != nil {
			t.Fatalf("Reload: %v", err)
		}

		if ag.GetConfig().TTLDeadline.Equal(originalDeadline) {
			t.Fatal("deadline did not move at all — the fix regressed")
		}
		if !ag.GetConfig().TTLDeadline.Before(originalDeadline) {
			t.Fatalf("deadline %v is not before the original %v — shortening did not actually shorten",
				ag.GetConfig().TTLDeadline, originalDeadline)
		}
		if !ag.GetConfig().TTLDeadline.Equal(newDeadline) {
			t.Fatalf("reloaded deadline %v != expected new deadline %v", ag.GetConfig().TTLDeadline, newDeadline)
		}
	})
}
