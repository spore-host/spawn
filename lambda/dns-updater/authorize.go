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
// WHICH registry. The entitled accounts are the portal's account registry,
// `spore-portal-accounts` (keyed on accountId), the same source dashboard-api
// checks every caller against (lambda/dashboard-api/entitlement.go), plus
// DNS_ALLOWED_ACCOUNTS. One GetItem by the verified account ID answers it,
// projecting only the key and status (#1611).
//
// It used to be a Scan of `spawn-user-accounts`, which is keyed on user_id
// and so cannot be read by account. That had two defects. The grant the Scan
// needed was never on the live role, so every registration logged the
// registry unreadable and fell back to DNS_ALLOWED_ACCOUNTS, and a newly
// onboarded account was refused under enforce. And spawn-user-accounts is
// written by dashboard-api on a caller's first request, so for as long as
// that Lambda wrote a row for any caller (before #1605) any caller could have
// entitled itself. The portal registry is written by the portal's onboarding.
//
// The live zone holds records for an account in neither registry (the four A
// records under c0zxr0ao.spore.host); that account is entitled with
// DNS_ALLOWED_ACCOUNTS, as before. DNS_AUTHZ_MODE=audit is the explicit way
// back.
//
// The registry's own fields are never used for ownership: ownership questions
// derive base36 from the verified account ID (encodeAccountID, alias_owner.go).

// registryTableDefault is the portal account registry. DNS_ACCOUNTS_TABLE
// overrides it.
const registryTableDefault = "spore-portal-accounts"

// Registry item attributes. These are the only two this function reads, and
// the IAM grant (scripts/deploy-custom-dns.sh, SpawnDNSAccountRegistryRead)
// pins dynamodb:Attributes to exactly them, so the two lists must agree.
// Projecting anything else is denied, which matters because the row also
// holds the account's onboarding role and ExternalId.
const (
	registryKeyAttr    = "accountId"
	registryStatusAttr = "status"
	statusOffboarded   = "offboarded"
)

// registryAttributes is what the GetItem projects, in the order the IAM
// grant lists them.
var registryAttributes = []string{registryKeyAttr, registryStatusAttr}

// entitledTTL and notEntitledTTL cache one account's answer for the life of a
// warm container. Entitlement changes when someone onboards, not per request,
// and a DNS write happens on instance launch and teardown, so a few minutes of
// staleness costs an onboarding user one retry. A refusal is cached for less.
const (
	entitledTTL    = 5 * time.Minute
	notEntitledTTL = time.Minute
)

// authzModeEnforce and authzModeAudit are the values of DNS_AUTHZ_MODE. Only
// audit lets a refused request through (logged); anything else, including
// unset or a typo, enforces.
const (
	authzModeEnforce = "enforce"
	authzModeAudit   = "audit"
)

var (
	dynamoClient *dynamodb.Client

	entitlementMu    sync.Mutex
	entitlementCache = map[string]entitlementEntry{}

	reportRejectedOnce sync.Once

	// lookupRegistryAccount is a variable so tests can substitute the lookup.
	// The behaviour being gated is what authorizeCaller DOES with each answer
	// (a row, no row, an offboarded row, an error), and none of those are
	// reachable through a real DynamoDB client in a unit test.
	lookupRegistryAccount = lookupRegistryAccountLive
)

type entitlementEntry struct {
	ok      bool
	expires time.Time
}

// authzEnforced reports whether a refusal should be fatal: always, unless
// DNS_AUTHZ_MODE is exactly "audit".
//
// This used to default to audit, for two reasons: --environment REPLACES a
// function's whole variable map, so a variable is easily lost and "unset" had
// to be safe to deploy; and the account owning four live records was not in the
// registry. The first argues the other way once the check guards record
// ownership as well as entitlement: a lost variable must not quietly reopen
// every account's records to every principal in it, so unset now enforces, and
// losing the variable fails closed. The second is handled by entitling that
// account in DNS_ALLOWED_ACCOUNTS before this deploys (the PR's deploy notes).
func authzEnforced() bool {
	return !strings.EqualFold(strings.TrimSpace(os.Getenv("DNS_AUTHZ_MODE")), authzModeAudit)
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

// registryTable is the registry the updater reads.
func registryTable() string {
	if t := os.Getenv("DNS_ACCOUNTS_TABLE"); t != "" {
		return t
	}
	return registryTableDefault
}

// registryGetItemInput is the one request the updater makes of the registry:
// a GetItem by the verified account ID, projecting registryAttributes.
// ConsistentRead is left off: an eventually consistent read is all the grant
// needs to allow, and onboarding is not a race worth a strongly consistent one.
func registryGetItemInput(table, accountID string) *dynamodb.GetItemInput {
	return &dynamodb.GetItemInput{
		TableName:            aws.String(table),
		Key:                  map[string]ddbtypes.AttributeValue{registryKeyAttr: &ddbtypes.AttributeValueMemberS{Value: accountID}},
		ProjectionExpression: aws.String("#k, #s"),
		// Both through names: status is a DynamoDB reserved word.
		ExpressionAttributeNames: map[string]string{"#k": registryKeyAttr, "#s": registryStatusAttr},
	}
}

// lookupRegistryAccountLive reads accountID's registry row: whether there is
// one, and its status.
func lookupRegistryAccountLive(ctx context.Context, accountID string) (bool, string, error) {
	if dynamoClient == nil {
		return false, "", fmt.Errorf("account registry client not initialised")
	}
	table := registryTable()
	out, err := dynamoClient.GetItem(ctx, registryGetItemInput(table, accountID))
	if err != nil {
		return false, "", fmt.Errorf("get %s from %s: %w", accountID, table, err)
	}
	if out.Item == nil {
		return false, "", nil
	}
	status := ""
	if v, ok := out.Item[registryStatusAttr].(*ddbtypes.AttributeValueMemberS); ok {
		status = v.Value
	}
	return true, status, nil
}

// resetAllowlistCache drops every cached answer. For tests.
func resetAllowlistCache() {
	entitlementMu.Lock()
	defer entitlementMu.Unlock()
	entitlementCache = map[string]entitlementEntry{}
}

// registryEntitles reports whether the registry entitles accountID, from cache
// when warm. A failed lookup is NOT cached: caching it would turn one
// throttled read into minutes of refusals.
func registryEntitles(ctx context.Context, accountID string) (bool, error) {
	entitlementMu.Lock()
	e, ok := entitlementCache[accountID]
	entitlementMu.Unlock()
	if ok && time.Now().Before(e.expires) {
		return e.ok, nil
	}
	found, status, err := lookupRegistryAccount(ctx, accountID)
	if err != nil {
		return false, err
	}
	entitled := found && status != statusOffboarded
	ttl := entitledTTL
	if !entitled {
		ttl = notEntitledTTL
	}
	entitlementMu.Lock()
	entitlementCache[accountID] = entitlementEntry{ok: entitled, expires: time.Now().Add(ttl)}
	entitlementMu.Unlock()
	return entitled, nil
}

// authorizeCaller reports whether the SigV4-verified account may register DNS at
// all, and why not when it may not.
//
// DNS_ALLOWED_ACCOUNTS answers first, with no DynamoDB call; then the
// registry. Fails closed on every uncertainty: an account that is not 12
// digits, a registry that cannot be read, no row, an offboarded row.
func authorizeCaller(ctx context.Context, accountID string) (bool, string) {
	if !isAccountID(accountID) {
		return false, fmt.Sprintf("verified caller account %q is not a 12-digit account ID", accountID)
	}
	static, rejected := extraAllowedAccounts()
	reportRejectedOnce.Do(func() {
		for _, bad := range rejected {
			fmt.Printf("DNS_ALLOWED_ACCOUNTS: ignoring %q — not a 12-digit AWS account ID, so the "+
				"account it was meant to entitle is NOT entitled\n", bad)
		}
	})
	if _, ok := static[accountID]; ok {
		return true, ""
	}
	entitled, err := registryEntitles(ctx, accountID)
	if err != nil {
		return false, fmt.Sprintf("could not read account %s from %s (%v) and it is not in "+
			"DNS_ALLOWED_ACCOUNTS — refusing rather than assuming entitlement", accountID, registryTable(), err)
	}
	if !entitled {
		return false, fmt.Sprintf("account %s is not onboarded in %s (no row, or offboarded) and is not in "+
			"DNS_ALLOWED_ACCOUNTS", accountID, registryTable())
	}
	return true, ""
}
