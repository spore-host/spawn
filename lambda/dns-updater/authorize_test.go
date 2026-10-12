package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

// Gates for caller authorization (#835, GHSA-7pm4-cqjg-vh2q).
//
// The property is not "the registry is read" but "an account the registry does not
// vouch for cannot write to the zone, and every uncertain answer refuses". So the
// lookup is substituted and what authorizeCaller DOES with each possible answer is
// asserted — including the answer that looks like success (an empty set).

// stubRegistry replaces the live Scan for the duration of a test.
func stubRegistry(t *testing.T, ids []string, err error) {
	t.Helper()
	prev := scanAccountRegistry
	scanAccountRegistry = func(context.Context) (map[string]struct{}, error) {
		if err != nil {
			return nil, err
		}
		out := map[string]struct{}{}
		for _, id := range ids {
			out[id] = struct{}{}
		}
		return out, nil
	}
	resetAllowlistCache()
	t.Cleanup(func() {
		scanAccountRegistry = prev
		resetAllowlistCache()
	})
}

func TestAuthorizeCallerRefusesAnAccountTheRegistryDoesNotVouchFor(t *testing.T) {
	stubRegistry(t, []string{"435415984226", "966362334030"}, nil)

	if ok, reason := authorizeCaller(context.Background(), "435415984226"); !ok {
		t.Errorf("a registered account was refused: %s", reason)
	}

	// The whole point of the issue: a stranger with a free AWS account passes SigV4
	// and must still be refused.
	ok, reason := authorizeCaller(context.Background(), "111122223333")
	if ok {
		t.Error("an account absent from the registry was authorized — SigV4 proves identity, " +
			"not entitlement, and that is the #835 defect exactly")
	}
	if reason == "" {
		t.Error("refused with no reason; a refusal that does not say which account or why is " +
			"not actionable from a log")
	}
}

// Every uncertain answer must refuse, including the one shaped like success.
func TestAuthorizeCallerFailsClosed(t *testing.T) {
	t.Run("lookup error", func(t *testing.T) {
		stubRegistry(t, nil, errors.New("ProvisionedThroughputExceededException"))
		if ok, _ := authorizeCaller(context.Background(), "435415984226"); ok {
			t.Error("an unreadable registry authorized the caller; an unknown answer must refuse")
		}
	})

	// CLAUDE.md rule 4: a discovery that finds nothing has broken, not proved the
	// set empty. Reading zero rows as "deny everyone" is safe; the failure mode
	// being gated is a future refactor reading it as "no restrictions configured".
	t.Run("empty discovery", func(t *testing.T) {
		stubRegistry(t, nil, nil)
		if ok, _ := authorizeCaller(context.Background(), "435415984226"); ok {
			t.Error("an empty registry authorized the caller — an empty allowlist is a broken " +
				"lookup, not a permissive one")
		}
	})

	t.Run("caller account not an account ID", func(t *testing.T) {
		stubRegistry(t, []string{"435415984226"}, nil)
		for _, bad := range []string{"", "43541598422", "43541598422x", "4354159842267"} {
			if ok, _ := authorizeCaller(context.Background(), bad); ok {
				t.Errorf("authorized a caller account of %q", bad)
			}
		}
	})
}

// The static allowlist exists so enforcement is shippable at all: the account
// owning four live records in the zone is not in the registry.
func TestStaticAllowlistEntitles(t *testing.T) {
	stubRegistry(t, []string{"435415984226"}, nil)
	t.Setenv("DNS_ALLOWED_ACCOUNTS", " 942542972736 ,,111122223333")
	resetAllowlistCache()

	for _, id := range []string{"942542972736", "111122223333", "435415984226"} {
		if ok, reason := authorizeCaller(context.Background(), id); !ok {
			t.Errorf("account %s was refused: %s", id, reason)
		}
	}
	if ok, _ := authorizeCaller(context.Background(), "000000000000"); ok {
		t.Error("an account in neither the registry nor the allowlist was authorized")
	}
}

// A malformed allowlist entry must be REPORTED, not quietly dropped.
//
// It cannot authorize anything on its own — a caller account is validated as 12
// digits before the set is consulted — so the only harm it does is silently fail
// to entitle the account the operator meant. The request it breaks is denied
// minutes later with a message about the registry, pointing nowhere near the
// typo. Being told at resolution time is the whole mechanism, so that is what is
// asserted here rather than the contents of the set.
func TestAMalformedAllowlistEntryIsReportedNotDropped(t *testing.T) {
	t.Setenv("DNS_ALLOWED_ACCOUNTS", "435415984226,43541598422,not-an-account, ")
	accepted, rejected := extraAllowedAccounts()

	if _, ok := accepted["435415984226"]; !ok || len(accepted) != 1 {
		t.Errorf("accepted = %v, want exactly the one well-formed id", accepted)
	}
	want := map[string]bool{"43541598422": true, "not-an-account": true}
	if len(rejected) != len(want) {
		t.Fatalf("rejected = %v, want the two malformed entries reported so an operator can "+
			"see the typo; an unreported skip is an entitlement that silently never happened",
			rejected)
	}
	for _, r := range rejected {
		if !want[r] {
			t.Errorf("unexpected rejected entry %q", r)
		}
	}
	// An empty element between commas is not a typo and must not be reported, or
	// a trailing comma produces noise on every cold start.
	for _, r := range rejected {
		if strings.TrimSpace(r) == "" {
			t.Error("an empty element was reported as malformed")
		}
	}
}

// The fallback is bounded: an unreadable registry may be survived by an explicit
// allowlist, never by the absence of one.
func TestUnreadableRegistryOnlyFallsBackToAnExplicitAllowlist(t *testing.T) {
	t.Run("with an allowlist, the listed account still works", func(t *testing.T) {
		stubRegistry(t, nil, errors.New("AccessDeniedException"))
		t.Setenv("DNS_ALLOWED_ACCOUNTS", "435415984226")
		resetAllowlistCache()
		if ok, reason := authorizeCaller(context.Background(), "435415984226"); !ok {
			t.Errorf("explicitly allowlisted account refused while the registry was down: %s", reason)
		}
		if ok, _ := authorizeCaller(context.Background(), "111122223333"); ok {
			t.Error("the registry being down widened the allowlist to an unlisted account")
		}
	})

	t.Run("without an allowlist, nobody is authorized", func(t *testing.T) {
		stubRegistry(t, nil, errors.New("AccessDeniedException"))
		t.Setenv("DNS_ALLOWED_ACCOUNTS", "")
		resetAllowlistCache()
		if ok, _ := authorizeCaller(context.Background(), "435415984226"); ok {
			t.Error("an unreadable registry with no static allowlist authorized a caller")
		}
	})
}

// A failed lookup must not be cached, or one throttled Scan becomes five minutes
// of outage for every caller.
func TestAFailedLookupIsNotCached(t *testing.T) {
	prev := scanAccountRegistry
	t.Cleanup(func() { scanAccountRegistry = prev; resetAllowlistCache() })
	t.Setenv("DNS_ALLOWED_ACCOUNTS", "")
	resetAllowlistCache()

	calls := 0
	scanAccountRegistry = func(context.Context) (map[string]struct{}, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("transient")
		}
		return map[string]struct{}{"435415984226": {}}, nil
	}

	if ok, _ := authorizeCaller(context.Background(), "435415984226"); ok {
		t.Fatal("the first (failing) lookup authorized the caller")
	}
	if ok, reason := authorizeCaller(context.Background(), "435415984226"); !ok {
		t.Errorf("the retry was refused, so the failure was cached: %s", reason)
	}
	if calls != 2 {
		t.Errorf("registry consulted %d times, want 2 — a failure was served from cache", calls)
	}
}

// Enforcement must be a flip, and the default must be the deployable one.
//
// Not style: update-function-code leaves environment variables untouched and
// --environment replaces the whole map, so an unset variable is the normal outcome
// of a deploy. If the default were enforce, redeploying this function would take
// the four live hosts in the unregistered account off the air.
func TestAuthzModeDefaultsToAudit(t *testing.T) {
	t.Setenv("DNS_AUTHZ_MODE", "")
	if authzEnforced() {
		t.Error("enforcement is on with DNS_AUTHZ_MODE unset")
	}
	for _, v := range []string{"enforce", "Enforce", " ENFORCE "} {
		t.Setenv("DNS_AUTHZ_MODE", v)
		if !authzEnforced() {
			t.Errorf("DNS_AUTHZ_MODE=%q did not enable enforcement", v)
		}
	}
	// Anything else is audit — notably not "true"/"1", which would be a plausible
	// guess by someone setting it from a deploy script and would silently not
	// enforce. Keep the two names the only accepted spellings and say so.
	for _, v := range []string{"true", "1", "on", "strict", "audit"} {
		t.Setenv("DNS_AUTHZ_MODE", v)
		if authzEnforced() {
			t.Errorf("DNS_AUTHZ_MODE=%q enabled enforcement; only %q should",
				v, authzModeEnforce)
		}
	}
}

// The decision must be made BEFORE the zone is resolved or anything is written.
//
// Driven with an unknown domain on purpose, so the audit-mode case stops at a 400
// instead of reaching Route53 — a unit test must not be one env var away from
// UPSERTing into the real spore.host zone. The 400 is itself the evidence of
// ordering: it is produced after the authorization check, so seeing it proves the
// request passed authorization rather than skipped it.
func TestHandlerEnforcesAndAuditsTheCallerDecision(t *testing.T) {
	stubRegistry(t, []string{"435415984226"}, nil)
	t.Setenv("DNS_ALLOWED_ACCOUNTS", "")
	resetAllowlistCache()

	body, _ := json.Marshal(DNSUpdateRequest{
		RecordName: "x", Action: "UPSERT", IPAddress: "1.2.3.4",
		Domain: "unknown-domain.invalid",
	})
	req := func(account string) events.LambdaFunctionURLRequest {
		return events.LambdaFunctionURLRequest{
			Body: string(body),
			RequestContext: events.LambdaFunctionURLRequestContext{
				Authorizer: &events.LambdaFunctionURLRequestContextAuthorizerDescription{
					IAM: &events.LambdaFunctionURLRequestContextAuthorizerIAMDescription{
						AccountID: account,
					},
				},
			},
		}
	}

	t.Run("enforce refuses the unentitled caller", func(t *testing.T) {
		t.Setenv("DNS_AUTHZ_MODE", authzModeEnforce)
		resp, err := handler(context.Background(), req("111122223333"))
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 403 {
			t.Errorf("status = %d, want 403; an unentitled account reached the zone-resolution "+
				"step, so authorization is either absent or runs too late", resp.StatusCode)
		}
		if strings.Contains(resp.Body, registryTableDefault) {
			t.Error("the 403 body names the registry table; an unentitled caller should not " +
				"learn how entitlement is decided")
		}
	})

	t.Run("enforce admits the entitled caller", func(t *testing.T) {
		t.Setenv("DNS_AUTHZ_MODE", authzModeEnforce)
		resp, err := handler(context.Background(), req("435415984226"))
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode == 403 {
			t.Error("an entitled account was refused 403")
		}
	})

	t.Run("audit allows the unentitled caller through", func(t *testing.T) {
		t.Setenv("DNS_AUTHZ_MODE", "")
		resp, err := handler(context.Background(), req("111122223333"))
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode == 403 {
			t.Error("audit mode returned 403 — it is supposed to log the refusal and continue, " +
				"so that enforcement can be turned on knowingly rather than by deploying")
		}
		if resp.StatusCode != 400 {
			t.Errorf("status = %d, want 400 (unknown domain). Anything else means the request "+
				"did not reach zone resolution and this case is not testing what it claims",
				resp.StatusCode)
		}
	})
}
