package main

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/aws/aws-sdk-go-v2/service/route53/types"
)

// Record ownership: which instance may change or delete a record.
//
// The canonical record is namespaced by the SigV4-verified caller account
// (#173), so a caller can write only under its own account's subdomain. Within
// that subdomain record_name is the caller's choice, so before this any
// principal in the account (any spore's role, read from IMDS by any local
// process) could repoint or delete any other instance's record, and serve the
// account's wildcard certificate behind it.
//
// The owner is an instance, identified the way IAM identifies it: an EC2
// instance-profile session is arn:aws:sts::<account>:assumed-role/<role>/<instance id>,
// and EC2 sets that session name. It counts only for spawn's own instance roles
// (spored-instance-role and spawn-instance-*), which trust ec2.amazonaws.com
// alone, so no principal can assume one under a session name of its choosing.
//
// The mapping is held server-side, in the zone, as a TXT record beside the
// record it describes: _spore-instance.<fqdn> = "<instance id>". The updater is
// its only writer, as for the lease. The rules:
//
//   - An instance may UPSERT a record it owns, or one nobody owns (no A
//     record, or only an orphaned marker). Claiming writes the marker with
//     Route53 CREATE in the same batch as the A record, so two instances
//     claiming one name at once cannot both win: the second batch fails whole.
//   - It may take over another instance's record only when the owner has
//     visibly stopped renewing it: the record's lease is older than
//     leaseMaxAge. A non-renewing owner (an older spored) is never displaced.
//   - A record from before ownership existed (an A record, no marker) belongs
//     to whoever sits at the address it names: an instance may claim or delete
//     it only when the request comes from that address (the Function URL's
//     source IP). Current spored renews hourly, so every live record it owns
//     is claimed within the hour after this deploys.
//   - DELETE is the owner's only, by the same rules.
//   - A caller that is not an instance session (`spawn dns register` and
//     `delete` from a workstation, or an instance under a role spawn did not
//     create) may change only records that carry no marker, and never writes
//     one: it cannot prove which instance it acts for.
//
// Enforced unless DNS_AUTHZ_MODE=audit, as the account check is: in audit a
// refusal is logged and the request goes ahead. Deleters of the marker: the
// DELETE path here, the TTL reaper's DNS teardown and its lease expiry
// (lambda/ttl-reaper), each beside the A record it removes.

// ownerPrefix is the marker's leading label. Underscore-prefixed so it can
// never collide with a record name (^[a-z0-9-]+$). It MIRRORS
// pkg/dns.OwnerPrefix; TestDNSLeaseContractMatchesTheUpdater (root module)
// fails if they drift.
const ownerPrefix = "_spore-instance"

// leaseMaxAge MIRRORS pkg/dns.LeaseMaxAge: a lease older than this says its
// owner has stopped renewing.
const leaseMaxAge = 6 * time.Hour

// ownerName is the marker TXT name for fqdn.
func ownerName(fqdn string) string {
	return ownerPrefix + "." + strings.TrimSuffix(fqdn, ".")
}

// instanceSessionRe is an EC2 instance-profile session ARN.
var instanceSessionRe = regexp.MustCompile(`^arn:aws[a-z-]*:sts::([0-9]{12}):assumed-role/([A-Za-z0-9+=,.@_-]+)/(i-[0-9a-f]{8,17})$`)

// callerInstance returns the instance a verified caller ARN is a session of,
// or "" when it is not an instance session of one of spawn's instance roles
// in account.
func callerInstance(userARN, account string) string {
	m := instanceSessionRe.FindStringSubmatch(strings.TrimSpace(userARN))
	if m == nil || m[1] != account {
		return ""
	}
	role := m[2]
	if role != "spored-instance-role" && !strings.HasPrefix(role, "spawn-instance-") {
		return ""
	}
	return m[3]
}

// recordState is what the zone says about one record's ownership.
type recordState struct {
	aValue    string                   // the A record's address, "" when there is none
	marker    *types.ResourceRecordSet // the ownership marker, nil when there is none
	owner     string                   // the marker's instance id
	leaseTime time.Time                // the lease's renewal time, zero when none or unreadable
}

// lookupRecordSet returns the record set name/type, or nil when there is none.
func lookupRecordSet(ctx context.Context, zoneID, name string, rrType types.RRType) (*types.ResourceRecordSet, error) {
	out, err := route53Client.ListResourceRecordSets(ctx, &route53.ListResourceRecordSetsInput{
		HostedZoneId:    aws.String(zoneID),
		StartRecordName: aws.String(name),
		StartRecordType: rrType,
		MaxItems:        aws.Int32(1),
	})
	if err != nil {
		return nil, fmt.Errorf("look up %s %s: %w", rrType, name, err)
	}
	for i := range out.ResourceRecordSets {
		rs := out.ResourceRecordSets[i]
		if strings.EqualFold(strings.TrimSuffix(aws.ToString(rs.Name), "."), name) && rs.Type == rrType {
			return &rs, nil
		}
	}
	return nil, nil
}

func readRecordState(ctx context.Context, zoneID, fqdn string) (recordState, error) {
	var st recordState
	a, err := lookupRecordSet(ctx, zoneID, fqdn, types.RRTypeA)
	if err != nil {
		return st, err
	}
	if a != nil && len(a.ResourceRecords) > 0 {
		st.aValue = aws.ToString(a.ResourceRecords[0].Value)
	}
	if st.marker, err = lookupRecordSet(ctx, zoneID, ownerName(fqdn), types.RRTypeTxt); err != nil {
		return st, err
	}
	if st.marker != nil && len(st.marker.ResourceRecords) > 0 {
		st.owner = strings.Trim(aws.ToString(st.marker.ResourceRecords[0].Value), `"`)
	}
	lease, err := existingLease(ctx, fqdn, zoneID)
	if err != nil {
		return st, err
	}
	if lease != nil && len(lease.ResourceRecords) > 0 {
		if t, perr := time.Parse(leaseLayout, strings.Trim(aws.ToString(lease.ResourceRecords[0].Value), `"`)); perr == nil {
			st.leaseTime = t
		}
	}
	return st, nil
}

// ownershipDecision is what authorizeOwnership decided.
type ownershipDecision struct {
	allow  bool
	reason string // why not, for the log
	// markerChanges go in the same Route53 batch as the record change: CREATE
	// a new claim, UPSERT a refresh, DELETE+CREATE a takeover, DELETE with a
	// delete. Empty for a caller that is not an instance.
	markerChanges []types.Change
}

func markerSet(fqdn, instance string) *types.ResourceRecordSet {
	return &types.ResourceRecordSet{
		Name:            aws.String(ownerName(fqdn)),
		Type:            types.RRTypeTxt,
		TTL:             aws.Int64(defaultTTL),
		ResourceRecords: []types.ResourceRecord{{Value: aws.String(`"` + instance + `"`)}},
	}
}

// authorizeOwnership applies the rules at the top of this file. instance is
// the caller's instance id ("" for a caller that is not an instance session),
// sourceIP the request's source address. Pure, so the rules are table-tested.
func authorizeOwnership(action, fqdn, instance, sourceIP string, st recordState, now time.Time) ownershipDecision {
	leaseStale := !st.leaseTime.IsZero() && now.Sub(st.leaseTime) > leaseMaxAge
	atAddress := st.aValue != "" && sourceIP != "" && st.aValue == sourceIP

	if instance == "" {
		if st.marker != nil && st.aValue != "" {
			return ownershipDecision{reason: fmt.Sprintf("%s is owned by instance %s, and the caller is not an instance session that can prove it acts for it", fqdn, st.owner)}
		}
		return ownershipDecision{allow: true}
	}

	switch action {
	case "DELETE":
		switch {
		case st.marker != nil && st.owner == instance:
			return ownershipDecision{allow: true, markerChanges: []types.Change{{Action: types.ChangeActionDelete, ResourceRecordSet: st.marker}}}
		case st.marker != nil && st.aValue != "":
			return ownershipDecision{reason: fmt.Sprintf("%s is owned by instance %s, not %s", fqdn, st.owner, instance)}
		case st.marker != nil: // orphaned marker, no record: nothing of anyone's to delete
			return ownershipDecision{allow: true, markerChanges: []types.Change{{Action: types.ChangeActionDelete, ResourceRecordSet: st.marker}}}
		case st.aValue == "" || atAddress:
			return ownershipDecision{allow: true}
		default:
			return ownershipDecision{reason: fmt.Sprintf("%s predates record ownership and points at %s, not at the caller's address %q", fqdn, st.aValue, sourceIP)}
		}
	default: // UPSERT
		claim := []types.Change{{Action: types.ChangeActionCreate, ResourceRecordSet: markerSet(fqdn, instance)}}
		takeover := func() []types.Change {
			return append([]types.Change{{Action: types.ChangeActionDelete, ResourceRecordSet: st.marker}}, claim...)
		}
		switch {
		case st.marker != nil && st.owner == instance:
			return ownershipDecision{allow: true, markerChanges: []types.Change{{Action: types.ChangeActionUpsert, ResourceRecordSet: markerSet(fqdn, instance)}}}
		case st.marker != nil && (st.aValue == "" || leaseStale):
			return ownershipDecision{allow: true, markerChanges: takeover()}
		case st.marker != nil:
			return ownershipDecision{reason: fmt.Sprintf("%s is owned by instance %s, which is still renewing it (or does not renew); %s may not take it over", fqdn, st.owner, instance)}
		case st.aValue == "" || atAddress || leaseStale:
			return ownershipDecision{allow: true, markerChanges: claim}
		default:
			return ownershipDecision{reason: fmt.Sprintf("%s predates record ownership and points at %s, not at the caller's address %q", fqdn, st.aValue, sourceIP)}
		}
	}
}

// auditTransfer is the marker change for a request audit mode lets through
// although authorizeOwnership refused it: the record now belongs to the
// caller, so the marker says so. Nothing for a caller that is not an
// instance, and nothing for a delete of a record with no marker.
func auditTransfer(action, fqdn, instance string, st recordState) []types.Change {
	var out []types.Change
	if st.marker != nil {
		out = append(out, types.Change{Action: types.ChangeActionDelete, ResourceRecordSet: st.marker})
	}
	if action == "DELETE" || instance == "" {
		if instance == "" {
			return nil
		}
		return out
	}
	return append(out, types.Change{Action: types.ChangeActionCreate, ResourceRecordSet: markerSet(fqdn, instance)})
}
