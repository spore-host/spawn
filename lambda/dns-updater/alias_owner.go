package main

import (
	"context"
	"fmt"
	"math/big"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/aws/aws-sdk-go-v2/service/route53/types"
)

// Alias-subdomain ownership (#820, GHSA-q9qq-223w-mqx6).
//
// The canonical record is namespaced by the SigV4-verified caller account, so a
// caller can only write under its own base36 subdomain. The friendly alias (#121)
// was built from req.AccountName — a string out of the request BODY, validated
// only as a DNS label — so any caller could UPSERT or DELETE
// {record}.{any-other-tenant-slug}.{domain}. The comment two lines above it in
// handler() claimed the opposite ("a caller can only write records under its own
// account's subdomain"), which was true of the line before and not of the alias.
//
// Resolving the owner server-side is the fix, and the hard part is that there is
// nowhere obvious to resolve it FROM:
//
//   - The friendly name comes from account:GetAccountInformation, which for a
//     MEMBER account requires org-management or delegated-admin credentials. The
//     management account of this org is one spore.host must never obtain access
//     to, so that door is closed by a project constraint, not by effort.
//   - The natural registry, the spore-portal-accounts DynamoDB table, is written
//     by the portal rather than by spawn and currently holds zero items. A lookup
//     against it would authorize nothing and break the one tenant using aliases.
//
// So ownership is derived from the zone's own state, which already encodes it: an
// existing alias CNAME points at {record}.{base36}.{domain}, and that base36 label
// decodes back to the owning account ID. A slug with records pointing at account X
// belongs to X. A slug with no records at all is unclaimed, and the first caller
// to use it takes it — recorded explicitly as a TXT marker so that ownership
// survives the last alias CNAME being deleted, which is otherwise exactly when a
// slug would silently become available to a squatter.

// ownerMarkerPrefix is the label carrying the explicit ownership claim. Underscore
// so it cannot collide with a record name, which handler() restricts to
// ^[a-z0-9-]+$.
const ownerMarkerPrefix = "_spore-owner"

// aliasTargetRe matches the canonical target an alias CNAME points at, capturing
// the base36 account label: {record}.{base36}.{domain}
var aliasTargetRe = regexp.MustCompile(`^[a-z0-9-]+\.([0-9a-z]+)\.(.+?)\.?$`)

// decodeAccountBase36 is the inverse of encodeAccountID. It returns "" when the
// label is not a plausible encoded account ID, so a stray record cannot be read as
// an ownership claim.
func decodeAccountBase36(label string) string {
	if label == "" || len(label) > 13 {
		return ""
	}
	n, ok := new(big.Int).SetString(label, 36)
	if !ok || n.Sign() <= 0 {
		return ""
	}
	s := n.String()
	// An AWS account ID is exactly 12 digits. Requiring that is what stops an
	// ordinary subdomain label (say "web" → 40876) being mistaken for an owner.
	if len(s) != 12 {
		return ""
	}
	// Round-trip: the label must be the canonical encoding, so "0web" or "WEB"
	// cannot claim the same account as "web".
	if encodeAccountID(s) != label {
		return ""
	}
	return s
}

// ownerMarkerName is the TXT record holding the explicit claim for a slug.
func ownerMarkerName(slug, dom string) string {
	return fmt.Sprintf("%s.%s.%s", ownerMarkerPrefix, slug, dom)
}

// normaliseName strips the trailing dot Route53 returns and lowercases, so names
// from the API compare equal to the ones built here.
func normaliseName(n string) string {
	return strings.ToLower(strings.TrimSuffix(n, "."))
}

// unescapeDNSName undoes Route53's octal escaping of non-hostname characters —
// notably `\052` for `*`. Without this, a wildcard record under a slug would not
// be recognised as belonging to that slug.
func unescapeDNSName(n string) string {
	return strings.ReplaceAll(n, `\052`, "*")
}

// aliasSubdomainOwner reports which account owns {slug}.{dom}, or "" if the slug
// is unclaimed. The explicit TXT marker wins; otherwise ownership is inferred from
// where the slug's existing alias CNAMEs point.
//
// It lists the whole zone rather than seeking with StartRecordName. Route53 sorts
// by label from the right, so the records under a slug ARE contiguous and seeking
// would be cheaper — but getting the ordering subtly wrong would skip a record and
// report a claimed slug as unclaimed, which is the one error this function must
// not make. DNS updates happen on instance launch and teardown, so the extra calls
// are affordable in a way a false "unclaimed" is not.
func aliasSubdomainOwner(ctx context.Context, slug, dom, zoneID string) (string, error) {
	suffix := "." + normaliseName(slug+"."+dom)
	marker := normaliseName(ownerMarkerName(slug, dom))

	var inferred string
	seen := 0
	paginator := route53.NewListResourceRecordSetsPaginator(route53Client, &route53.ListResourceRecordSetsInput{
		HostedZoneId: aws.String(zoneID),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			// Fail closed: an unreadable zone must not be read as "unclaimed".
			return "", fmt.Errorf("list records to resolve alias owner: %w", err)
		}
		seen += len(page.ResourceRecordSets)
		for _, rr := range page.ResourceRecordSets {
			name := normaliseName(unescapeDNSName(aws.ToString(rr.Name)))

			if rr.Type == types.RRTypeTxt && name == marker {
				if owner := ownerFromTXT(rr); owner != "" {
					// Explicit claim is authoritative; no need to look further.
					return owner, nil
				}
				continue
			}

			if rr.Type != types.RRTypeCname || !strings.HasSuffix(name, suffix) {
				continue
			}
			for _, v := range rr.ResourceRecords {
				m := aliasTargetRe.FindStringSubmatch(normaliseName(aws.ToString(v.Value)))
				if m == nil {
					continue
				}
				if owner := decodeAccountBase36(m[1]); owner != "" && inferred == "" {
					inferred = owner
				}
			}
		}
	}

	// A real hosted zone always contains at least its apex SOA and NS records,
	// neither of which can be deleted. So listing a zone and seeing NOTHING does
	// not mean "no records under this slug" — it means the listing did not do what
	// was asked: the wrong zone ID, a stubbed client, a pagination bug. Treating
	// that as "unclaimed" would authorize the write, which is the fail-open this
	// whole function exists to remove, reappearing on the success path rather than
	// the error path. A gate written to assert the refusal is what found it.
	if seen == 0 {
		return "", fmt.Errorf("zone %s returned no records at all, not even SOA/NS — refusing to "+
			"infer that %s.%s is unclaimed", zoneID, slug, dom)
	}
	return inferred, nil
}

// ownerFromTXT extracts the account ID from the marker record's value. TXT values
// come back quoted.
func ownerFromTXT(rr types.ResourceRecordSet) string {
	for _, v := range rr.ResourceRecords {
		val := strings.Trim(strings.TrimSpace(aws.ToString(v.Value)), `"`)
		if len(val) == 12 {
			if _, ok := new(big.Int).SetString(val, 10); ok {
				return val
			}
		}
	}
	return ""
}

// claimAliasSubdomain writes the explicit ownership marker for a slug.
//
// Best-effort by design: the alias itself is already a non-fatal nicety beside the
// canonical record, so failing to record the claim must not fail the request. The
// consequence of a missed claim is that ownership falls back to being inferred from
// the alias CNAMEs, which is where it was resolved from anyway.
func claimAliasSubdomain(ctx context.Context, slug, dom, zoneID, accountID string) error {
	_, err := route53Client.ChangeResourceRecordSets(ctx, &route53.ChangeResourceRecordSetsInput{
		HostedZoneId: aws.String(zoneID),
		ChangeBatch: &types.ChangeBatch{
			Comment: aws.String("spore.host alias-subdomain ownership claim (#820)"),
			Changes: []types.Change{{
				Action: types.ChangeActionUpsert,
				ResourceRecordSet: &types.ResourceRecordSet{
					Name:            aws.String(ownerMarkerName(slug, dom)),
					Type:            types.RRTypeTxt,
					TTL:             aws.Int64(300),
					ResourceRecords: []types.ResourceRecord{{Value: aws.String(`"` + accountID + `"`)}},
				},
			}},
		},
	})
	return err
}

// authorizeAlias decides whether callerAccountID may write or delete the alias
// subdomain {slug}.{dom}, claiming it if unowned.
//
// Returns the reason when refusing, for the log line — a refusal that does not say
// whose slug it was is not actionable.
func authorizeAlias(ctx context.Context, slug, dom, zoneID, callerAccountID string) (bool, string) {
	owner, err := aliasSubdomainOwner(ctx, slug, dom, zoneID)
	if err != nil {
		return false, fmt.Sprintf("could not resolve the owner of %s.%s (%v) — refusing rather than "+
			"guessing", slug, dom, err)
	}
	if owner == "" {
		// Unclaimed: the caller takes it. Recording the claim is what stops the slug
		// becoming available again when its last alias CNAME is deleted.
		if cerr := claimAliasSubdomain(ctx, slug, dom, zoneID, callerAccountID); cerr != nil {
			fmt.Printf("warning: could not record the ownership claim for %s.%s: %v\n", slug, dom, cerr)
		}
		return true, ""
	}
	if owner != callerAccountID {
		return false, fmt.Sprintf("alias subdomain %s.%s belongs to account %s; caller is %s",
			slug, dom, owner, callerAccountID)
	}
	return true, ""
}
