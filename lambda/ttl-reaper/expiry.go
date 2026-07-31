package main

// DNS expiry for unmanaged subdomains (#466, the last open piece of #457).
//
// The unmanaged-subdomain report (#458) deletes nothing, and its comment says why:
// an unmanaged subdomain is ambiguous between "the customer uninstalled and left
// records" and "an active account nobody added to REAPER_ROLE_ARNS", and we cannot
// tell them apart *precisely because we lack the credentials* — DescribeInstances is
// how emptiness is proven.
//
// The portal's account-prober (spore-host/spore-host#492) has those credentials. It
// assumes each registry account's spore-portal-onboard role — a different role from
// the reaper's spawn-ttl-reaper-ec2, and one the registry actually knows about — and
// proves emptiness across every region. So for any account IN THE REGISTRY the
// ambiguity is now resolvable, and accountlifecycle.DNSExpiryEligible is the verdict.
//
// What has NOT changed is the cost of being wrong. spored registers DNS once, at boot
// (pkg/agent/agent.go:234) — there is no periodic re-registration, so a wrongly
// deleted A-record never self-heals. The spore keeps running and is merely
// unreachable by name until it reboots. Hence: the verdict is always REPORTED, and
// acting on it needs REAPER_DNS_EXPIRE on top of the sweep's own opt-in.

import (
	"fmt"
	"time"

	"github.com/spore-host/spore-host/lambda/accountlifecycle"
)

// expiryVerdict is what the registry says about one unmanaged subdomain's account.
// Separate from the eligibility bool because the REASON is the useful half: an
// operator reading the log needs to know whether we proved dormancy, proved nothing,
// or have never heard of this account.
type expiryVerdict struct {
	eligible bool
	// status is the registry status, or "" when the account has no row at all.
	status string
	// detail is the human-facing explanation appended to the report line.
	detail string
}

// classifySubdomain decides whether one unmanaged subdomain's records may be
// expired. Pure — same shape as orphanedRecords/sweepableRecord, so the decision
// that authorizes a deletion is table-tested without AWS.
//
// rows is the registry keyed by account ID. A nil/empty map means we could not read
// the registry, which is NOT the same as "no accounts are eligible" — see the caller,
// which refuses to act at all in that case.
func classifySubdomain(accountID string, rows map[string]accountlifecycle.Account, now time.Time) expiryVerdict {
	acct, ok := rows[accountID]
	if !ok {
		// The informative negative. This account never onboarded through the portal,
		// so the prober has no opinion about it and the original #458 ambiguity
		// stands in full. Reporting "not in the registry" is strictly better than the
		// old bare "no credentials to reconcile", because it tells the operator that
		// no amount of waiting will produce a verdict.
		return expiryVerdict{
			detail: "not in the portal registry (never onboarded, or onboarded before the registry existed) " +
				"— no verdict is possible; add its role to REAPER_ROLE_ARNS to sweep it, or offboard it deliberately",
		}
	}

	status := acct.AccountStatus()
	if !accountlifecycle.DNSExpiryEligible(&acct) {
		// Deliberately NOT re-implemented here: DNSExpiryEligible is the single gate,
		// its unknown-status default is false, and its guards are mutation-verified in
		// the accountlifecycle module. A second copy of the rule would drift.
		detail := fmt.Sprintf("registry status %q — not eligible for expiry", status)
		if status == accountlifecycle.StatusUnreachable {
			// Worth spelling out: this is the state we would most LIKE to clean up and
			// the one where verification has become impossible, because the deleted
			// role is what we would have verified through (#457 trap 2).
			detail += " (the role is gone, so emptiness can no longer be proven — this needs a human, not a longer wait)"
		}
		return expiryVerdict{status: status, detail: detail}
	}

	detail := fmt.Sprintf("registry status %q — ELIGIBLE for expiry", status)
	if since := acct.StatusChangedAt; since != "" {
		detail += fmt.Sprintf(" (since %s", since)
		if seen := acct.LastSeenAt; seen != "" {
			if t, err := time.Parse(time.RFC3339, seen); err == nil {
				detail += fmt.Sprintf(", last probed %s ago", now.Sub(t).Round(time.Minute))
			}
		}
		detail += ")"
	}
	return expiryVerdict{eligible: true, status: status, detail: detail}
}

// expiryAction is what the reaper does about one unmanaged subdomain.
type expiryAction int

const (
	// actionReport is the #458 behaviour: log and delete nothing.
	actionReport expiryAction = iota
	// actionExpire deletes the subdomain's A-records.
	actionExpire
)

// authorizeExpiry is the single gate that turns a verdict into a deletion, kept pure
// and separate from classifySubdomain so the full truth table — every combination of
// verdict and configuration that could delete a live spore's DNS — is testable in one
// place with no AWS.
//
// registryOK is deliberately an input rather than folded into the verdict: an
// unreadable registry produces the same "not in the registry" verdict as a genuinely
// unregistered account, and only one of those two is a reason to doubt the whole run.
// Both refuse, but conflating them would hide the difference from the log.
func authorizeExpiry(v expiryVerdict, registryOK, expireDNS bool) expiryAction {
	if !registryOK || !v.eligible || !expireDNS {
		return actionReport
	}
	return actionExpire
}
