package main

// DNS lease expiry — the reaper's half (see pkg/dns/lease.go for the design).
//
// What this closes. The DNS sweep (#438) reconciles records against
// DescribeInstances, and #466's expiry relies on the portal prober assuming a role
// in the owning account. Both need credentials there. For an account the reaper
// does not cover — and in particular one spore.host must never hold credentials in
// — neither can run, so a record left by an abruptly terminated instance points at
// its IP forever, under a spore.host name, after AWS has handed that IP to someone
// else. A lease lets the zone answer instead: a record whose owner has stopped
// renewing it has been abandoned.
//
// Deliberately SCOPED to subdomains the reaper does not cover. For a covered
// account the DescribeInstances sweep is authoritative — it asks EC2 directly — and
// a stale lease there most likely means spored hung on a live instance, where
// deleting its name would be wrong. The lease is used only where it is the ONLY
// signal there is.
//
// Safety, in order:
//   - REPORT ALWAYS; act only with REAPER_DNS_LEASE_EXPIRE=true, which itself needs
//     the DNS sweep on. Arrives off.
//   - dry-run is honoured, by deleteRecord.
//   - an unparseable lease is reported and NEVER acted on.
//   - a record with no lease is never touched here: every spored released before
//     leases existed registers without one, and its records behave exactly as
//     before.
//   - the lease is RE-READ immediately before a deletion, so a renewal that lands
//     between this run's listing and its delete wins.
//   - A-records only, plus the lease itself. Alias CNAMEs are left, for the reason
//     expireSubdomain gives: a dangling CNAME resolves to nothing, a dangling
//     A-record resolves to a stranger's IP.

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	r53types "github.com/aws/aws-sdk-go-v2/service/route53/types"

	"github.com/spore-host/spawn/pkg/dns"
)

// leaseVerdict is what one lease says should happen.
type leaseVerdict int

const (
	leaseKeep        leaseVerdict = iota // fresh: leave it
	leaseExpire                          // stale, uncovered account: expire record + lease
	leaseOrphan                          // the A-record is gone: delete the lease
	leaseUnparseable                     // cannot read it: report, never act
	leaseCovered                         // a covered account: the instance sweep owns it
)

// leaseFinding is one lease and what to do about it.
type leaseFinding struct {
	record  string // A-record FQDN, no trailing dot
	lease   string // lease TXT FQDN, no trailing dot
	account string // decoded owner account
	renewed time.Time
	verdict leaseVerdict
	detail  string
}

// classifyLeases decides what every lease in the zone calls for. Pure: the decision
// that authorizes a deletion is table-tested without AWS, the same shape as
// orphanedRecords and classifySubdomain.
//
// managed is the set of account IDs the reaper covers. An empty map is NOT "nothing
// is covered" — it means account resolution failed, and the caller refuses to run
// rather than treat every account as uncovered.
func classifyLeases(sets []r53types.ResourceRecordSet, domain string, managed map[string]bool, now time.Time, maxAge time.Duration) []leaseFinding {
	suffix := "." + domain
	aRecords := map[string]bool{}
	for _, rs := range sets {
		if rs.Type == r53types.RRTypeA {
			aRecords[strings.ToLower(strings.TrimSuffix(aws.ToString(rs.Name), "."))] = true
		}
	}

	var out []leaseFinding
	for _, rs := range sets {
		if rs.Type != r53types.RRTypeTxt {
			continue
		}
		name := strings.TrimSuffix(aws.ToString(rs.Name), ".")
		record, ok := dns.LeasedRecord(name)
		if !ok || !strings.HasSuffix(record, suffix) {
			continue
		}
		// The owner is the account label directly before the domain. A lease whose
		// record does not sit under a decodable account subdomain is not one the
		// updater wrote, and is left alone.
		rel := strings.TrimSuffix(record, suffix)
		label := rel
		if i := strings.LastIndex(rel, "."); i >= 0 {
			label = rel[i+1:]
		}
		acct, ok := dns.DecodeAccountID(label)
		if !ok {
			continue
		}
		f := leaseFinding{record: record, lease: strings.ToLower(name), account: acct}

		switch {
		case !aRecords[record]:
			f.verdict, f.detail = leaseOrphan, "its A-record no longer exists"
		case len(rs.ResourceRecords) == 0:
			f.verdict, f.detail = leaseUnparseable, "lease record has no value"
		default:
			t, err := dns.ParseLease(aws.ToString(rs.ResourceRecords[0].Value))
			if err != nil {
				f.verdict, f.detail = leaseUnparseable, err.Error()
				break
			}
			f.renewed = t
			switch {
			case managed[acct]:
				f.verdict, f.detail = leaseCovered, "account is covered — the instance sweep is authoritative"
			case dns.Evaluate(t, now, maxAge) == dns.LeaseStale:
				f.verdict = leaseExpire
				f.detail = fmt.Sprintf("not renewed for %s (limit %s)", now.Sub(t).Round(time.Minute), maxAge)
			default:
				f.verdict = leaseKeep
			}
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].lease < out[j].lease })
	return out
}

// authorizeLeaseAction is the gate between a verdict and a deletion. Pure, and the
// only place that says yes.
func authorizeLeaseAction(v leaseVerdict, expireLeases bool) bool {
	if !expireLeases {
		return false
	}
	return v == leaseExpire || v == leaseOrphan
}

// reportLeases walks the zone's leases, reports every verdict, and — only when
// armed — acts on the ones authorizeLeaseAction allows.
func (r *reaper) reportLeases(ctx context.Context, sum *Summary, now time.Time) {
	if r.dnsZoneID == "" || r.dnsDomain == "" {
		return
	}
	managed := make(map[string]bool, len(r.accounts))
	for _, acct := range r.accounts {
		if acct.accountID != "" {
			managed[acct.accountID] = true
		}
	}
	// Without resolved account IDs every account would look uncovered, and a stale
	// lease on a COVERED account — a hung spored on a live box — would be expired.
	if len(managed) == 0 {
		log.Printf("DNS lease report: no resolved account IDs — skipping rather than treating every account as uncovered")
		return
	}

	sets, err := r.listZoneRecordSets(ctx)
	if err != nil {
		log.Printf("DNS lease report: list zone failed: %v", err)
		sum.Errors++
		return
	}

	for _, f := range classifyLeases(sets, r.dnsDomain, managed, now, dns.LeaseMaxAge) {
		switch f.verdict {
		case leaseKeep:
			sum.DNSLeasesFresh++
			continue
		case leaseCovered:
			sum.DNSLeasesCovered++
			continue
		case leaseUnparseable:
			sum.DNSLeasesUnparseable++
			log.Printf("DNS lease UNPARSEABLE %s (account %s): %s — never acted on", f.lease, f.account, f.detail)
			continue
		case leaseExpire:
			sum.DNSLeasesStale++
		case leaseOrphan:
			sum.DNSLeasesOrphaned++
		}

		if !authorizeLeaseAction(f.verdict, r.expireLeases) {
			log.Printf("DNS lease WOULD %s %s (account %s): %s; set REAPER_DNS_LEASE_EXPIRE=true to act",
				verb(f.verdict), f.record, f.account, f.detail)
			continue
		}
		r.actOnLease(ctx, f, sum, now)
	}
}

func verb(v leaseVerdict) string {
	if v == leaseOrphan {
		return "delete the orphaned lease for"
	}
	return "expire"
}

// actOnLease performs an authorized action.
func (r *reaper) actOnLease(ctx context.Context, f leaseFinding, sum *Summary, now time.Time) {
	if f.verdict == leaseExpire {
		// Re-read the lease. A renewal landing between this run's listing and this
		// delete must win: the owner has just said the record is alive.
		if renewed, ok := r.currentLease(ctx, f.lease); ok &&
			dns.Evaluate(renewed, now, dns.LeaseMaxAge) == dns.LeaseFresh {
			log.Printf("DNS lease: %s was renewed at %s since this run listed the zone — not expiring",
				f.record, renewed.Format(time.RFC3339))
			return
		}
		log.Printf("DNS lease: EXPIRING %s (account %s): %s", f.record, f.account, f.detail)
		r.deleteRecord(ctx, f.record, r53types.RRTypeA)
		sum.DNSLeasesExpired++
	}
	r.deleteRecord(ctx, f.lease, r53types.RRTypeTxt)
}

// currentLease re-reads one lease's renewal time.
func (r *reaper) currentLease(ctx context.Context, lease string) (time.Time, bool) {
	out, err := r.route53Client.ListResourceRecordSets(ctx, &route53.ListResourceRecordSetsInput{
		HostedZoneId:    aws.String(r.dnsZoneID),
		StartRecordName: aws.String(lease),
		StartRecordType: r53types.RRTypeTxt,
		MaxItems:        aws.Int32(1),
	})
	if err != nil {
		return time.Time{}, false
	}
	for _, rs := range out.ResourceRecordSets {
		if strings.EqualFold(strings.TrimSuffix(aws.ToString(rs.Name), "."), lease) &&
			rs.Type == r53types.RRTypeTxt && len(rs.ResourceRecords) > 0 {
			t, err := dns.ParseLease(aws.ToString(rs.ResourceRecords[0].Value))
			return t, err == nil
		}
	}
	return time.Time{}, false
}

// listZoneRecordSets returns every record set in the zone with its values,
// paginated. listZoneRecordNames returns names only; leases need the TXT values.
func (r *reaper) listZoneRecordSets(ctx context.Context) ([]r53types.ResourceRecordSet, error) {
	var out []r53types.ResourceRecordSet
	input := &route53.ListResourceRecordSetsInput{HostedZoneId: aws.String(r.dnsZoneID)}
	for {
		page, err := r.route53Client.ListResourceRecordSets(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("list records: %w", err)
		}
		out = append(out, page.ResourceRecordSets...)
		if !page.IsTruncated {
			break
		}
		input.StartRecordName = page.NextRecordName
		input.StartRecordType = page.NextRecordType
		input.StartRecordIdentifier = page.NextRecordIdentifier
	}
	// A real zone always has its apex SOA and NS, so an empty listing means the
	// listing did not do what was asked — not that there are no leases.
	if len(out) == 0 {
		return nil, fmt.Errorf("zone %s returned no records at all, not even SOA/NS", r.dnsZoneID)
	}
	return out, nil
}
