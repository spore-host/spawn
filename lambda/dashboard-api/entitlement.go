package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Caller ENTITLEMENT, as distinct from authentication.
//
// getUserFromRequest proves who a caller is: STS GetCallerIdentity on the
// credentials they sent, or API Gateway's IAM identity. That says nothing about
// whether they are a customer. Any AWS account, including a free one opened a
// minute ago, has credentials STS will vouch for, and every route behind
// authentication acts on the caller's behalf with this Lambda's own role: team
// creation and membership, the user-account cache, and POST
// /api/strata/resolve, which writes lockfiles into the shared strata registry.
//
// So every authenticated request must also come from an entitled account:
//
//   - one onboarded in the portal's account registry (DASHBOARD_ACCOUNTS_TABLE,
//     default spore-portal-accounts, keyed on accountId) and not offboarded; or
//   - one listed in DASHBOARD_ALLOWED_ACCOUNTS (comma-separated account IDs),
//     for accounts entitled without a registry row: the operator's own, and
//     CLI users from before the portal registry existed.
//
// spawn-user-accounts is deliberately NOT a source: this Lambda writes it
// itself on a caller's first request, so trusting it would let any caller
// entitle themselves.
//
// It fails closed on every uncertainty. An unreadable registry denies (503)
// unless the static list alone answers; an empty list plus an empty registry
// denies everyone. Unlike dns-updater's DNS_AUTHZ_MODE there is no audit mode:
// set DASHBOARD_ALLOWED_ACCOUNTS before deploying, or the dashboard refuses
// every caller.

// errNotEntitled: the caller's account is neither onboarded nor allow-listed.
var errNotEntitled = errors.New("account is not onboarded to spore.host")

// errEntitlementUnavailable: entitlement could not be decided.
var errEntitlementUnavailable = errors.New("could not verify the account's entitlement")

const (
	portalAccountsTableDefault = "spore-portal-accounts"
	portalStatusOffboarded     = "offboarded"
	entitledTTL                = 5 * time.Minute
	notEntitledTTL             = time.Minute
)

// portalAccountLookup reads an account's registry row: found, and its status.
// A variable so tests substitute it; nothing about the decision is reachable
// through a real DynamoDB client in a unit test.
var portalAccountLookup = lookupPortalAccountLive

var (
	entitlementMu    sync.Mutex
	entitlementCache = map[string]entitlementEntry{}
)

type entitlementEntry struct {
	ok      bool
	expires time.Time
}

func resetEntitlementCache() {
	entitlementMu.Lock()
	defer entitlementMu.Unlock()
	entitlementCache = map[string]entitlementEntry{}
}

// allowListedAccounts parses DASHBOARD_ALLOWED_ACCOUNTS, reporting entries it
// rejects (a typo would otherwise silently fail to entitle the account meant).
func allowListedAccounts() map[string]bool {
	out := map[string]bool{}
	for _, raw := range strings.Split(os.Getenv("DASHBOARD_ALLOWED_ACCOUNTS"), ",") {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if !isAccountID(id) {
			log.Printf("DASHBOARD_ALLOWED_ACCOUNTS: ignoring %q — not a 12-digit account ID, so the account it was meant to entitle is NOT entitled", id)
			continue
		}
		out[id] = true
	}
	return out
}

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

// accountOfScope is the account a scope key (the cliIamArn the routes filter
// on) belongs to: the key itself when it is an account ID (the portal path),
// else the account field of an ARN. "" when it is neither.
func accountOfScope(scope string) string {
	if isAccountID(scope) {
		return scope
	}
	parts := strings.Split(scope, ":")
	if len(parts) >= 6 && parts[0] == "arn" && isAccountID(parts[4]) {
		return parts[4]
	}
	return ""
}

// authorizeAccounts requires every named account to be entitled, and at least
// one to be named: a request whose account cannot be determined is refused.
func authorizeAccounts(ctx context.Context, cfg aws.Config, accounts ...string) error {
	seen := 0
	for _, a := range accounts {
		if a == "" {
			continue
		}
		seen++
		if err := authorizeAccount(ctx, cfg, a); err != nil {
			return err
		}
	}
	if seen == 0 {
		return fmt.Errorf("%w: the caller's account could not be determined", errNotEntitled)
	}
	return nil
}

// authorizeAccount reports whether one account is entitled.
func authorizeAccount(ctx context.Context, cfg aws.Config, accountID string) error {
	if !isAccountID(accountID) {
		return fmt.Errorf("%w: %q is not an account ID", errNotEntitled, accountID)
	}
	if allowListedAccounts()[accountID] {
		return nil
	}

	entitlementMu.Lock()
	e, ok := entitlementCache[accountID]
	entitlementMu.Unlock()
	if ok && time.Now().Before(e.expires) {
		if e.ok {
			return nil
		}
		return fmt.Errorf("%w (account %s)", errNotEntitled, accountID)
	}

	found, status, err := portalAccountLookup(ctx, cfg, accountID)
	if err != nil {
		// Not cached: one throttled read must not become minutes of refusals.
		log.Printf("entitlement: registry lookup for %s failed: %v", accountID, err)
		return fmt.Errorf("%w: %v", errEntitlementUnavailable, err)
	}
	entitled := found && status != portalStatusOffboarded
	ttl := entitledTTL
	if !entitled {
		ttl = notEntitledTTL
	}
	entitlementMu.Lock()
	entitlementCache[accountID] = entitlementEntry{ok: entitled, expires: time.Now().Add(ttl)}
	entitlementMu.Unlock()
	if !entitled {
		log.Printf("entitlement: account %s refused (registry row found=%v status=%q)", accountID, found, status)
		return fmt.Errorf("%w (account %s)", errNotEntitled, accountID)
	}
	return nil
}

// lookupPortalAccountLive reads the account's row from the portal registry,
// projecting only the key and status (the row also holds the onboarding role
// and ExternalId, which this function has no business reading).
func lookupPortalAccountLive(ctx context.Context, cfg aws.Config, accountID string) (bool, string, error) {
	table := os.Getenv("DASHBOARD_ACCOUNTS_TABLE")
	if table == "" {
		table = portalAccountsTableDefault
	}
	out, err := dynamodb.NewFromConfig(cfg).GetItem(ctx, &dynamodb.GetItemInput{
		TableName:                aws.String(table),
		Key:                      map[string]ddbtypes.AttributeValue{"accountId": &ddbtypes.AttributeValueMemberS{Value: accountID}},
		ProjectionExpression:     aws.String("accountId, #s"),
		ExpressionAttributeNames: map[string]string{"#s": "status"},
	})
	if err != nil {
		return false, "", fmt.Errorf("get %s from %s: %w", accountID, table, err)
	}
	if out.Item == nil {
		return false, "", nil
	}
	status := ""
	if v, ok := out.Item["status"].(*ddbtypes.AttributeValueMemberS); ok {
		status = v.Value
	}
	return true, status, nil
}
