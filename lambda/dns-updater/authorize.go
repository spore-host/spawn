package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Caller AUTHORIZATION, as distinct from authentication (#835, GHSA-7pm4-cqjg-vh2q).
//
// The Function URL is AuthType: AWS_IAM, so every request reaching handler() has
// passed SigV4 and carries a verified caller account. That answers "who is this",
// and #173 reasoned carefully about it — "unspoofable, and independent of anything
// in the request body", which is true. The other half was never written: the
// function's resource policy is the statement AWS auto-generates for a Function
// URL, `Principal: "*"` gated only on lambda:FunctionUrlAuthType, so *any*
// principal in *any* AWS account may invoke it. SigV4 says who the caller is;
// nothing said they were a customer.
//
// That matters more than tidiness because record_name is caller-chosen and the
// record lands in the real spore.host zone: a stranger with a free AWS account
// could publish login.<their-base36>.spore.host pointing anywhere — a phishing
// primitive hosted and paid for by spore.host.
//
// Two layers close it, and only one of them is code:
//
//  1. The resource policy, narrowed to the entitled accounts. That is the real
//     fix: it refuses at the URL, before any of this runs, and it is a live AWS
//     change rather than a deploy (see docs/dns-updater-authorization.md).
//  2. This file: the same decision inside the handler, so the function is not
//     relying solely on a policy nobody will notice being widened again, and so
//     the entitled set can be a registry rather than a literal list.
//
// WHICH registry, and the hazard in picking one. #835 proposed
// `spore-portal-accounts`; it holds zero items, so a lookup against it would
// authorize nobody. `spawn-user-accounts` is the populated one (3 items, carrying
// aws_account_id). But the live spore.host zone has four A records under
// c0zxr0ao.spore.host — keel-skx, keel-zen4, keel-zen5, loop-l12-esc — and
// c0zxr0ao decodes to an account that is NOT in that table. So the obvious
// implementation of this issue, shipped enforcing, would have broken DNS for four
// running hosts on their next registration. Hence DNS_AUTHZ_MODE below: this
// deploys in audit, the logs say who actually calls, and enforcement is a separate
// deliberate flip. Same staging the TTL reaper shipped with.
//
// Note also that the table's own account_base36 attribute is NOT reliably the
// encoding of its aws_account_id (one row stores "c8s8u", which decodes to
// 20565246), so ownership questions must derive base36 from the verified account
// ID — as encodeAccountID and alias_owner.go already do — and never read it from
// the registry.

// registryTableDefault is the populated account registry. Overridable so the
// portal's table can take over once it holds anything.
const registryTableDefault = "spawn-user-accounts"

// accountIDAttr is the only attribute this function reads. Named here because the
// IAM grant restricts the Scan to it via the dynamodb:Attributes condition key, so
// the two have to agree: projecting anything else is denied, not just impolite.
const accountIDAttr = "aws_account_id"

// allowlistTTL caches the resolved set for the life of a warm container. Account
// entitlement changes when someone onboards, not per request, and a DNS write
// happens on instance launch and teardown — so a few minutes of staleness costs an
// onboarding user one retry, while re-scanning per request would put a DynamoDB
// call in the path of every launch.
const allowlistTTL = 5 * time.Minute

// authzModeEnforce is the value of DNS_AUTHZ_MODE that makes a refusal a 403.
const authzModeEnforce = "enforce"

var (
	dynamoClient *dynamodb.Client

	allowlistMu     sync.Mutex
	allowlistCache  map[string]struct{}
	allowlistExpiry time.Time

	// scanAccountRegistry is a variable so tests can substitute the lookup. The
	// behaviour being gated is what authorizeCaller DOES with each answer —
	// success, failure, and empty — and none of those are reachable through a real
	// DynamoDB client in a unit test.
	scanAccountRegistry = scanAccountRegistryLive
)

// authzEnforced reports whether a refusal should be fatal.
//
// Deliberately defaulting to audit, which means a fresh deploy does NOT yet close
// the hole. Two reasons, both specific:
//
//   - update-function-code does not touch environment variables and
//     --environment REPLACES the whole map, so "the deploy sets it" is exactly how
//     a variable goes missing. An unset mode must therefore be the SAFE-to-deploy
//     one, not the strict one.
//   - the account owning four live records is not in the registry (see above).
//     Defaulting to enforce would take those hosts off the air to fix a
//     vulnerability nobody is exploiting.
//
// This is the one place in this file where failing OPEN is the right default, and
// it is only safe because the edge fix — the resource policy — does not depend on
// it.
func authzEnforced() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("DNS_AUTHZ_MODE")), authzModeEnforce)
}

// extraAllowedAccounts reads the static allowlist from the environment.
//
// This exists so an account can be entitled without a registry write — bootstrap,
// break-glass, and the "running hosts in an unregistered account" case that would
// otherwise make enforcement unshippable.
//
// It returns the entries it REJECTED as well as the ones it accepted, which is the
// load-bearing half. A malformed entry cannot itself authorize anything — a caller
// account is validated as 12 digits before the set is consulted, so junk in the set
// is unreachable. What it can do is silently fail to entitle the account the
// operator meant, and a request denied five minutes later says nothing about the
// typo in the variable. So the skip has to be reported, and the report is what the
// gate asserts. (The first version of that gate checked the set contents instead
// and passed a revert that deleted the validation entirely — CLAUDE.md rule 6:
// that was a correlate of the mechanism, not the mechanism.)
func extraAllowedAccounts() (map[string]struct{}, []string) {
	out := map[string]struct{}{}
	var rejected []string
	for _, raw := range strings.Split(os.Getenv("DNS_ALLOWED_ACCOUNTS"), ",") {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if !isAccountID(id) {
			rejected = append(rejected, id)
			continue
		}
		out[id] = struct{}{}
	}
	return out, rejected
}

// isAccountID reports whether s is exactly 12 decimal digits.
func isAccountID(s string) bool {
	if len(s) != 12 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// scanAccountRegistryLive reads the entitled account IDs out of the registry table.
//
// A Scan, not a Query: the table is keyed on user_id with GSIs on cli_iam_arn and
// email, and this function knows only an account ID — none of those. The table
// holds one item per onboarded identity, so it is small by nature (3 today), and
// the result is cached per container. ProjectionExpression keeps the read to the
// one attribute, which also keeps it inside what the IAM grant permits.
//
// The same account appears more than once (one row per identity_type), so the
// result is a set.
func scanAccountRegistryLive(ctx context.Context) (map[string]struct{}, error) {
	if dynamoClient == nil {
		return nil, fmt.Errorf("account registry client not initialised")
	}
	table := os.Getenv("DNS_ACCOUNTS_TABLE")
	if table == "" {
		table = registryTableDefault
	}

	out := map[string]struct{}{}
	var start map[string]ddbtypes.AttributeValue
	for {
		page, err := dynamoClient.Scan(ctx, &dynamodb.ScanInput{
			TableName:            aws.String(table),
			ProjectionExpression: aws.String(accountIDAttr),
			// Select is set EXPLICITLY, not left for DynamoDB to infer from the
			// presence of ProjectionExpression.
			//
			// The IAM grant pins dynamodb:Select to SPECIFIC_ATTRIBUTES, because
			// that is the only thing stopping ForAllValues:StringEquals on
			// dynamodb:Attributes being satisfied vacuously by a request naming no
			// attributes at all. A condition key that the request does not carry
			// evaluates as absent and the condition fails — so inferring Select
			// server-side would leave the grant denying its own only caller, and
			// it would fail in production and nowhere else. Simulated: allowed with
			// this pair, implicitDeny with Select=ALL_ATTRIBUTES, with a second
			// projected attribute, and with no context at all.
			Select:            ddbtypes.SelectSpecificAttributes,
			ExclusiveStartKey: start,
		})
		if err != nil {
			return nil, fmt.Errorf("scan %s: %w", table, err)
		}
		for _, item := range page.Items {
			av, ok := item[accountIDAttr]
			if !ok {
				continue
			}
			s, ok := av.(*ddbtypes.AttributeValueMemberS)
			if !ok || !isAccountID(s.Value) {
				continue
			}
			out[s.Value] = struct{}{}
		}
		if len(page.LastEvaluatedKey) == 0 {
			break
		}
		start = page.LastEvaluatedKey
	}
	return out, nil
}

// allowedAccounts returns the entitled set, from cache when warm.
//
// A failed lookup is NOT cached: caching it would turn one throttled Scan into
// five minutes of refusals for every caller.
func allowedAccounts(ctx context.Context) (map[string]struct{}, error) {
	allowlistMu.Lock()
	defer allowlistMu.Unlock()

	if allowlistCache != nil && time.Now().Before(allowlistExpiry) {
		return allowlistCache, nil
	}

	merged, rejected := extraAllowedAccounts()
	for _, bad := range rejected {
		fmt.Printf("DNS_ALLOWED_ACCOUNTS: ignoring %q — not a 12-digit AWS account ID, so the "+
			"account it was meant to entitle is NOT entitled\n", bad)
	}
	registry, err := scanAccountRegistry(ctx)
	if err != nil {
		// Only tolerable when the static allowlist alone can answer the question.
		// Otherwise the caller decides what to do with the error; it is not this
		// function's place to decide that an unreadable registry means "allow".
		if len(merged) == 0 {
			return nil, err
		}
		fmt.Printf("warning: account registry unreadable (%v); falling back to DNS_ALLOWED_ACCOUNTS (%d entries)\n",
			err, len(merged))
		return merged, nil
	}
	for id := range registry {
		merged[id] = struct{}{}
	}

	allowlistCache, allowlistExpiry = merged, time.Now().Add(allowlistTTL)
	return merged, nil
}

// resetAllowlistCache drops the cached set. For tests.
func resetAllowlistCache() {
	allowlistMu.Lock()
	defer allowlistMu.Unlock()
	allowlistCache, allowlistExpiry = nil, time.Time{}
}

// authorizeCaller reports whether the SigV4-verified account may register DNS at
// all, and why not when it may not.
//
// Fails closed on every uncertainty, including the one that looks like success: a
// lookup that returns zero accounts has broken rather than proved that nobody is
// entitled (CLAUDE.md rule 4 — a discovery finding nothing is a failure). The
// registry is known to hold items; an empty answer means the table name is wrong,
// or it was emptied, or the projection stopped matching. Reading that as "deny
// everyone" is correct but reading it as "allow everyone" would be the same
// fail-open this file exists to remove.
func authorizeCaller(ctx context.Context, accountID string) (bool, string) {
	if !isAccountID(accountID) {
		return false, fmt.Sprintf("verified caller account %q is not a 12-digit account ID", accountID)
	}
	allowed, err := allowedAccounts(ctx)
	if err != nil {
		return false, fmt.Sprintf("could not resolve the entitled accounts (%v) — refusing rather "+
			"than assuming entitlement", err)
	}
	if len(allowed) == 0 {
		return false, "the entitled-account lookup returned nothing at all; the registry is known " +
			"to hold items, so this is a broken lookup rather than an empty allowlist"
	}
	if _, ok := allowed[accountID]; !ok {
		return false, fmt.Sprintf("account %s is not registered in %s and is not in "+
			"DNS_ALLOWED_ACCOUNTS", accountID, registryTableDefault)
	}
	return true, ""
}
