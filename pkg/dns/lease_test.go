package dns

import (
	"testing"
	"time"
)

func TestLeaseNameRoundTrips(t *testing.T) {
	for _, fqdn := range []string{
		"loop-l12-esc.c0zxr0ao.spore.host",
		"loop-l12-esc.c0zxr0ao.spore.host.", // Route53 returns names with the trailing dot
	} {
		name := LeaseName(fqdn)
		if name != "_spore-lease.loop-l12-esc.c0zxr0ao.spore.host" {
			t.Errorf("LeaseName(%q) = %q", fqdn, name)
		}
		got, ok := LeasedRecord(name + ".")
		if !ok || got != "loop-l12-esc.c0zxr0ao.spore.host" {
			t.Errorf("LeasedRecord(%q) = %q, %v", name, got, ok)
		}
	}
}

// Names that are not leases must not be read as one. The owner claim is the
// sibling most likely to be confused with it.
func TestLeasedRecordRejectsNonLeases(t *testing.T) {
	for _, name := range []string{
		"_spore-owner.scott-s-test-account.spore.host.",
		"loop-l12-esc.c0zxr0ao.spore.host.",
		"_spore-lease.",
		"_spore-leasex.foo.spore.host.",
	} {
		if rec, ok := LeasedRecord(name); ok {
			t.Errorf("%q was read as the lease of %q", name, rec)
		}
	}
}

func TestLeaseValueRoundTripsThroughRoute53Quoting(t *testing.T) {
	at := time.Date(2026, 10, 12, 21, 11, 3, 0, time.UTC)
	v := FormatLease(at)
	if v != `"2026-10-12T21:11:03Z"` {
		t.Errorf("FormatLease = %s", v)
	}
	for _, in := range []string{v, `2026-10-12T21:11:03Z`, ` "2026-10-12T21:11:03Z" `} {
		got, err := ParseLease(in)
		if err != nil || !got.Equal(at) {
			t.Errorf("ParseLease(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := ParseLease(`"yesterday"`); err == nil {
		t.Error("an unparseable lease parsed; the reaper must be able to tell it apart and refuse to act")
	}
}

func TestEvaluate(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		renewed time.Time
		want    LeaseState
	}{
		{"just renewed", now, LeaseFresh},
		{"one missed renewal", now.Add(-2 * time.Hour), LeaseFresh},
		{"exactly at the limit is still fresh", now.Add(-LeaseMaxAge), LeaseFresh},
		{"past the limit", now.Add(-LeaseMaxAge - time.Second), LeaseStale},
		{"days old", now.Add(-72 * time.Hour), LeaseStale},
		// Skew must never expire a live record.
		{"dated in the future", now.Add(3 * time.Hour), LeaseFresh},
	}
	for _, c := range cases {
		if got := Evaluate(c.renewed, now, LeaseMaxAge); got != c.want {
			t.Errorf("%s: Evaluate = %v, want %v", c.name, got, c.want)
		}
	}
}

// The renewal cadence must leave room for several missed renewals before expiry,
// or one updater outage expires every live record in the zone.
func TestLeaseMaxAgeToleratesMissedRenewals(t *testing.T) {
	worstGap := LeaseRefreshInterval + LeaseRefreshJitter
	if LeaseMaxAge < 4*worstGap {
		t.Errorf("LeaseMaxAge %s allows fewer than four missed renewals at a worst-case gap of %s",
			LeaseMaxAge, worstGap)
	}
}
