package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/aws/aws-sdk-go-v2/service/route53/types"
)

// DNS leases — the updater's half. See pkg/dns/lease.go for the design and why it
// exists; in one line, a record whose owner stops renewing it can be expired from
// the zone alone, with no credentials in the owning account.
//
// The updater is the only writer of a lease. That is the security property: the
// lease name is derived from fqdn, and fqdn is anchored to the SigV4-verified
// caller account (#173), so a caller can renew only its own records.
//
// These two constants MIRROR pkg/dns. This module does not import the root module,
// so the values are copied rather than shared, and
// TestDNSLeaseContractMatchesTheUpdater (root module) fails if they drift. Drift is
// not cosmetic: a reaper reading a different name or format would see no lease, or
// an unparseable one — and the first would expire nothing while the second must
// refuse to act, so the failure is safe, but it would silently disable the feature.
const (
	leasePrefix = "_spore-lease"
	leaseLayout = time.RFC3339
)

// leaseName is pkg/dns.LeaseName.
func leaseName(fqdn string) string {
	return leasePrefix + "." + strings.TrimSuffix(fqdn, ".")
}

// leaseValue is pkg/dns.FormatLease: the renewal time, quoted as Route53 requires.
func leaseValue(t time.Time) string {
	return `"` + t.UTC().Format(leaseLayout) + `"`
}

// leaseTTL is the TXT record's DNS TTL. Nothing resolves leases at the edge — they
// are read by the reaper via the Route53 API — so the TTL only bounds caching for a
// curious resolver. Matched to the A-record's.
const leaseTTL = defaultTTL

// leaseChange is the UPSERT that renews the lease for fqdn. Returned as a Change
// rather than applied, so the caller can put it in the SAME ChangeBatch as the
// A-record: Route53 applies a batch atomically, so a record and its lease are
// written together or not at all, and there is no window in which a fresh A-record
// carries an old lease.
func leaseChange(fqdn string, now time.Time) types.Change {
	return types.Change{
		Action: types.ChangeActionUpsert,
		ResourceRecordSet: &types.ResourceRecordSet{
			Name:            aws.String(leaseName(fqdn)),
			Type:            types.RRTypeTxt,
			TTL:             aws.Int64(leaseTTL),
			ResourceRecords: []types.ResourceRecord{{Value: aws.String(leaseValue(now))}},
		},
	}
}

// existingLease returns the current lease record set for fqdn, or nil if there is
// none. Route53 DELETE needs the exact record set, so removal has to read it first.
func existingLease(ctx context.Context, fqdn, zoneID string) (*types.ResourceRecordSet, error) {
	name := leaseName(fqdn)
	out, err := route53Client.ListResourceRecordSets(ctx, &route53.ListResourceRecordSetsInput{
		HostedZoneId:    aws.String(zoneID),
		StartRecordName: aws.String(name),
		StartRecordType: types.RRTypeTxt,
		MaxItems:        aws.Int32(1),
	})
	if err != nil {
		return nil, fmt.Errorf("look up lease %s: %w", name, err)
	}
	for i := range out.ResourceRecordSets {
		rs := out.ResourceRecordSets[i]
		if strings.TrimSuffix(aws.ToString(rs.Name), ".") == name && rs.Type == types.RRTypeTxt {
			return &rs, nil
		}
	}
	return nil, nil
}

// removeLease deletes the lease for fqdn if there is one.
//
// Best-effort, and its callers treat a failure as non-fatal. The cost of a lease
// left behind is bounded and self-correcting: the reaper deletes a lease whose
// A-record is gone, and a stale lease on a record that WAS re-registered without
// one is the only case that could expire a live record — which is exactly the case
// clearLeaseIfUnrenewed exists to prevent, and why it is called on every
// lease-less UPSERT.
func removeLease(ctx context.Context, fqdn, zoneID string) error {
	rs, err := existingLease(ctx, fqdn, zoneID)
	if err != nil || rs == nil {
		return err
	}
	_, err = route53Client.ChangeResourceRecordSets(ctx, &route53.ChangeResourceRecordSetsInput{
		HostedZoneId: aws.String(zoneID),
		ChangeBatch: &types.ChangeBatch{
			Comment: aws.String("spore.host DNS lease removed with its record"),
			Changes: []types.Change{{Action: types.ChangeActionDelete, ResourceRecordSet: rs}},
		},
	})
	return err
}

// clearLeaseIfUnrenewed removes a lease when a record is re-registered by a client
// that does not renew.
//
// The record's state must reflect its LATEST writer. Suppose a renewing spored
// registers `box`, then the instance is replaced by one running an older spored that
// registers the same name once and never again. Without this, the old lease would
// age out and the reaper would expire a record whose instance is alive — the one
// outcome the lease design must never produce. Removing it returns the record to
// today's behaviour: no lease, never expired by lease.
func clearLeaseIfUnrenewed(ctx context.Context, fqdn, zoneID string, renewing bool) {
	if renewing {
		return
	}
	if err := removeLease(ctx, fqdn, zoneID); err != nil {
		fmt.Printf("warning: could not clear the lease on %s after a non-renewing registration: %v\n", fqdn, err)
	}
}
