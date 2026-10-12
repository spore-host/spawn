package dns

import (
	"fmt"
	"strings"
	"time"
)

// DNS leases: liveness asserted by the record's OWNER, so a record can be expired
// without anyone holding credentials in the account that owns it.
//
// Why this exists. Every other backstop for a stale A-record works by asking EC2
// whether the instance behind it is still alive: the reaper's DNS sweep (#438)
// calls DescribeInstances in the owning account, and DNS expiry (#466) relies on
// the portal prober assuming a role there. Both need credentials in that account.
// For an account spore.host must never hold credentials in — and for any governed
// org that will not grant an external principal access — neither can run, so a
// record left by an abruptly terminated instance never goes away. A record that
// outlives its instance is not merely clutter: it points at an IP AWS will hand to
// someone else, under a spore.host name.
//
// A lease inverts the question. spored re-registers on a timer, and the DNS updater
// stamps the time of each registration into a companion TXT record. A record whose
// lease has not been renewed for long enough is one whose owner has stopped saying
// it is alive, and that is decidable from the zone alone.
//
// The updater writes the lease, not spored, and only under the SigV4-verified
// caller's own namespace (#173), so a caller can renew only its own records and
// cannot forge or refresh anyone else's.
//
// The format below is a contract between three separately built components —
// spored (this package), lambda/dns-updater (its own module, which does not import
// this one) and lambda/ttl-reaper. TestDNSLeaseContractMatchesTheUpdater fails if
// the updater's copy drifts from this one.

// LeasePrefix is the leading label of a lease TXT record. Underscore-prefixed so it
// can never collide with a record name, which the updater restricts to
// ^[a-z0-9-]+$ — the same convention as the _spore-owner alias claim.
const LeasePrefix = "_spore-lease"

// LeaseRefreshInterval is how often spored renews its registration. Jittered by
// LeaseRefreshJitter so that a job array launched together does not renew in one
// burst: Route53 throttles ChangeResourceRecordSets at five requests per second
// per account, and a thousand spores renewing in the same second would trip it.
const (
	LeaseRefreshInterval = time.Hour
	LeaseRefreshJitter   = 10 * time.Minute
)

// LeaseMaxAge is how long a lease may go unrenewed before the record is treated as
// abandoned. Six missed renewals: generous enough that an outage of the updater, a
// Route53 throttle, or a spored restart does not expire a live record, and short
// enough that an abandoned record stops pointing at a recycled IP the same day.
//
// Deliberately much longer than LeaseRefreshInterval. The cost of being wrong is
// asymmetric but no longer unbounded: with renewal in place a wrongly expired
// record comes back on the next refresh, which is what the #466 expiry code could
// not offer when it noted that spored "registers DNS once, at boot".
const LeaseMaxAge = 6 * time.Hour

// LeaseName returns the TXT record name holding the lease for an A-record FQDN.
// The lease sits directly under the record it describes, so it shares the
// account's base36 subdomain and is grouped with it by every per-account walk.
func LeaseName(fqdn string) string {
	return LeasePrefix + "." + strings.TrimSuffix(fqdn, ".")
}

// LeasedRecord returns the A-record FQDN a lease TXT name describes, and false if
// the name is not a lease.
func LeasedRecord(txtName string) (string, bool) {
	name := strings.TrimSuffix(strings.ToLower(txtName), ".")
	rest, ok := strings.CutPrefix(name, LeasePrefix+".")
	if !ok || rest == "" {
		return "", false
	}
	return rest, true
}

// FormatLease renders a renewal time as the TXT value, quoted as Route53 requires.
func FormatLease(t time.Time) string {
	return `"` + t.UTC().Format(time.RFC3339) + `"`
}

// ParseLease reads a TXT value written by FormatLease. It accepts the value with or
// without its surrounding quotes, because Route53 returns them and callers differ in
// whether they have already stripped them.
func ParseLease(value string) (time.Time, error) {
	v := strings.Trim(strings.TrimSpace(value), `"`)
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("lease value %q is not RFC 3339: %w", value, err)
	}
	return t, nil
}

// LeaseState is what a lease says about its record.
type LeaseState int

const (
	// LeaseFresh: renewed recently enough. Leave the record alone.
	LeaseFresh LeaseState = iota
	// LeaseStale: not renewed for longer than LeaseMaxAge. The owner has stopped
	// asserting the record is alive.
	LeaseStale
)

// Evaluate decides a lease's state. A lease dated in the future is FRESH, not
// stale: clock skew between the updater and the reaper must never be able to
// expire a live record, and a future timestamp cannot mean "abandoned".
func Evaluate(renewed, now time.Time, maxAge time.Duration) LeaseState {
	if now.Sub(renewed) > maxAge {
		return LeaseStale
	}
	return LeaseFresh
}
