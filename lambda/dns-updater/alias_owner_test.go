package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	r53types "github.com/aws/aws-sdk-go-v2/service/route53/types"
	"github.com/scttfrdmn/substrate/emulator"
)

// Alias-subdomain tenancy (#820, GHSA-q9qq-223w-mqx6).
//
// Driven against the Substrate Route53 emulator rather than a hand mock, for the
// same reason TestCNAMERecord_UpsertDelete is: the thing most likely to be wrong
// here is how real record names and values come back — trailing dots, quoting on
// TXT values, octal escaping — and a fake that returns what the code expects
// cannot catch any of that.

const (
	ownerAcct  = "435415984226" // base36 5k0zfnmq
	otherAcct  = "966362334030"
	testDomain = "spore.host"
)

func aliasTestZone(t *testing.T) string {
	t.Helper()
	ts := emulator.StartTestServer(t)
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithBaseEndpoint(ts.URL),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider("test", "test", "test")),
	)
	if err != nil {
		t.Fatalf("build config: %v", err)
	}
	route53Client = route53.NewFromConfig(cfg)
	// Both test accounts are entitled. Without this the handler tests Scanned the
	// LIVE account registry through init's ambient-credential dynamoClient — on a
	// developer machine, a real DynamoDB read of the production table — and
	// passed in CI only because that Scan failed and fell back to the static
	// allowlist (#968).
	stubRegistry(t, []string{ownerAcct, otherAcct}, nil)

	zone, err := route53Client.CreateHostedZone(context.Background(), &route53.CreateHostedZoneInput{
		Name:            aws.String(testDomain),
		CallerReference: aws.String("alias-owner-test"),
	})
	if err != nil {
		t.Skipf("substrate CreateHostedZone unavailable: %v", err)
	}
	zoneID := aws.ToString(zone.HostedZone.Id)

	// Real Route53 creates apex SOA and NS records with the zone and will not let
	// you delete them, which is what makes "the listing returned nothing" a
	// reliable signal that the listing is broken. Substrate's CreateHostedZone
	// does not, so seed an apex record to keep the fixture faithful — otherwise
	// every test here exercises a zone shape that cannot exist in production.
	if _, err := route53Client.ChangeResourceRecordSets(context.Background(),
		&route53.ChangeResourceRecordSetsInput{
			HostedZoneId: aws.String(zoneID),
			ChangeBatch: &r53types.ChangeBatch{
				Changes: []r53types.Change{{
					Action: r53types.ChangeActionUpsert,
					ResourceRecordSet: &r53types.ResourceRecordSet{
						Name: aws.String(testDomain),
						Type: r53types.RRTypeTxt,
						TTL:  aws.Int64(300),
						ResourceRecords: []r53types.ResourceRecord{
							{Value: aws.String(`"apex placeholder for SOA/NS"`)},
						},
					},
				}},
			},
		}); err != nil {
		t.Skipf("substrate ChangeResourceRecordSets unavailable: %v", err)
	}
	return zoneID
}

// The base36 label in an existing alias target identifies the owner.
//
// This is the whole basis of the fix, so it is asserted against the encoder rather
// than against a hardcoded string: if encodeAccountID ever changes, this fails
// instead of silently authorizing the wrong account.
func TestDecodeAccountBase36RoundTripsAndRejectsOrdinaryLabels(t *testing.T) {
	for _, acct := range []string{ownerAcct, otherAcct, "123456789012"} {
		label := encodeAccountID(acct)
		if got := decodeAccountBase36(label); got != acct {
			t.Errorf("decodeAccountBase36(%q) = %q, want %q", label, got, acct)
		}
	}

	// Labels that must NOT be read as an ownership claim. Each of these would, if
	// accepted, let a record under an unrelated subdomain assert ownership.
	for _, label := range []string{
		"", "web", "prod", "api", "spore", "a", "0",
		"zzzzzzzzzzzzzz",                 // too long
		"0" + encodeAccountID(ownerAcct), // non-canonical: leading zero
		"5K0ZFNMQ",                       // non-canonical: uppercase
		"not-base36",                     // hyphen is not a base36 digit
	} {
		if got := decodeAccountBase36(label); got != "" {
			t.Errorf("decodeAccountBase36(%q) = %q, want \"\" — an ordinary label must not be "+
				"readable as an account ID", label, got)
		}
	}
}

// A slug whose existing alias points at account X belongs to X.
func TestAliasOwnerInferredFromAnExistingAliasTarget(t *testing.T) {
	zoneID := aliasTestZone(t)
	ctx := context.Background()

	alias := "job.mycelium-development." + testDomain
	target := "job." + encodeAccountID(ownerAcct) + "." + testDomain
	if _, err := upsertCNAMERecord(ctx, alias, target, zoneID); err != nil {
		t.Fatalf("seed alias: %v", err)
	}

	owner, err := aliasSubdomainOwner(ctx, "mycelium-development", testDomain, zoneID)
	if err != nil {
		t.Fatalf("aliasSubdomainOwner: %v", err)
	}
	if owner != ownerAcct {
		t.Fatalf("owner = %q, want %q (inferred from the alias target's base36 label)", owner, ownerAcct)
	}
}

// The owner may write and delete its own alias; nobody else may.
//
// This is the defect, stated directly: before the fix, authorizeAlias did not
// exist and req.AccountName was spliced into the record name unchecked.
func TestOnlyTheOwningAccountMayWriteAnAliasSubdomain(t *testing.T) {
	zoneID := aliasTestZone(t)
	ctx := context.Background()

	slug := "mycelium-development"
	alias := "job." + slug + "." + testDomain
	target := "job." + encodeAccountID(ownerAcct) + "." + testDomain
	if _, err := upsertCNAMERecord(ctx, alias, target, zoneID); err != nil {
		t.Fatalf("seed alias: %v", err)
	}

	if ok, reason := authorizeAlias(ctx, slug, testDomain, zoneID, ownerAcct); !ok {
		t.Errorf("the owning account was refused its own alias subdomain: %s", reason)
	}

	ok, reason := authorizeAlias(ctx, slug, testDomain, zoneID, otherAcct)
	if ok {
		t.Fatal("a non-owning account was authorized to write another tenant's alias subdomain — " +
			"this is the #820 defect: it could point the tenant's friendly name at its own " +
			"instance, or delete it")
	}
	// The refusal must name both parties, or an operator cannot act on the log line.
	for _, want := range []string{slug, ownerAcct, otherAcct} {
		if !contains(reason, want) {
			t.Errorf("refusal reason %q does not mention %q", reason, want)
		}
	}
}

// An unclaimed slug is taken by its first user, and the claim is recorded.
//
// Recording it matters: without the marker, deleting the last alias CNAME would
// make the slug look unclaimed again, which is precisely when a squatter would
// take it.
func TestFirstUserClaimsAnUnownedSlugAndTheClaimPersists(t *testing.T) {
	zoneID := aliasTestZone(t)
	ctx := context.Background()
	slug := "brand-new-tenant"

	owner, err := aliasSubdomainOwner(ctx, slug, testDomain, zoneID)
	if err != nil {
		t.Fatalf("aliasSubdomainOwner: %v", err)
	}
	if owner != "" {
		t.Fatalf("a slug with no records reported owner %q, want unclaimed", owner)
	}

	if ok, reason := authorizeAlias(ctx, slug, testDomain, zoneID, ownerAcct); !ok {
		t.Fatalf("first user was refused an unclaimed slug: %s", reason)
	}

	// The claim must now be readable, with NO alias CNAME in place — the case that
	// would otherwise regress to unclaimed.
	owner, err = aliasSubdomainOwner(ctx, slug, testDomain, zoneID)
	if err != nil {
		t.Fatalf("aliasSubdomainOwner after claim: %v", err)
	}
	if owner != ownerAcct {
		t.Fatalf("after claiming, owner = %q, want %q — the TXT marker was not recorded or not read",
			owner, ownerAcct)
	}

	// And a second account is now refused.
	if ok, _ := authorizeAlias(ctx, slug, testDomain, zoneID, otherAcct); ok {
		t.Error("a second account was authorized for a slug already claimed by another")
	}
}

// The explicit marker beats an inferred owner.
//
// Otherwise an attacker who got one CNAME in could contest a recorded claim.
func TestTheExplicitClaimOutranksAnInferredOwner(t *testing.T) {
	zoneID := aliasTestZone(t)
	ctx := context.Background()
	slug := "contested"

	// Record the claim for the rightful owner...
	if err := claimAliasSubdomain(ctx, slug, testDomain, zoneID, ownerAcct); err != nil {
		t.Fatalf("claimAliasSubdomain: %v", err)
	}
	// ...then plant a CNAME pointing at someone else's base36 subdomain.
	planted := "evil." + slug + "." + testDomain
	plantedTarget := "evil." + encodeAccountID(otherAcct) + "." + testDomain
	if _, err := upsertCNAMERecord(ctx, planted, plantedTarget, zoneID); err != nil {
		t.Fatalf("plant CNAME: %v", err)
	}

	owner, err := aliasSubdomainOwner(ctx, slug, testDomain, zoneID)
	if err != nil {
		t.Fatalf("aliasSubdomainOwner: %v", err)
	}
	if owner != ownerAcct {
		t.Errorf("owner = %q, want %q — a planted CNAME overrode the recorded claim", owner, ownerAcct)
	}
}

// An unreadable zone must refuse, not default to "unclaimed".
//
// Fail-open here would be worse than the original bug: it would be a check that
// disappears exactly when Route53 is degraded.
func TestAnUnresolvableOwnerRefusesRatherThanAllowing(t *testing.T) {
	ctx := context.Background()
	zoneID := aliasTestZone(t)

	// A zone ID that does not exist makes the list call fail.
	ok, reason := authorizeAlias(ctx, "whatever", testDomain, zoneID+"-nope", ownerAcct)
	if ok {
		t.Fatal("authorizeAlias allowed the write despite being unable to resolve the owner; " +
			"an unreadable zone must not read as unclaimed")
	}
	if !contains(reason, "refusing") {
		t.Errorf("refusal reason does not say it is refusing: %q", reason)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		(haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}

// A failing list call must refuse, not fall through to "unclaimed".
//
// Separate from TestAnUnresolvableOwnerRefusesRatherThanAllowing: that one is
// caught by the empty-listing guard on the SUCCESS path. This drives the error
// path, which substrate never produces on its own — and which a revert to
// `return "", nil` left silently uncovered until this test existed.
func TestAListErrorRefusesRatherThanAllowing(t *testing.T) {
	zoneID := aliasTestZone(t)

	// Re-point the package client at a closed port so the call genuinely fails.
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithBaseEndpoint("http://127.0.0.1:1"),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider("test", "test", "test")),
		awsconfig.WithRetryMaxAttempts(1),
	)
	if err != nil {
		t.Fatalf("build config: %v", err)
	}
	route53Client = route53.NewFromConfig(cfg)

	ok, reason := authorizeAlias(context.Background(), "mycelium-development", testDomain, zoneID, ownerAcct)
	if ok {
		t.Fatal("authorizeAlias allowed the write although the zone could not be listed at all; " +
			"a check that evaporates when Route53 is unreachable is worse than no check")
	}
	if !contains(reason, "could not resolve the owner") {
		t.Errorf("refusal does not name the cause: %q", reason)
	}
}

// The handler itself must authorize the alias, not merely be able to.
//
// The unit tests above all call authorizeAlias directly, so deleting the call
// from handler() leaves every one of them passing — which is exactly the shape of
// the original bug (the helper aliasDNSName validated the slug perfectly well; the
// caller just never checked ownership). Drive the handler.
func TestHandlerRefusesAnAliasUnderAnotherAccountsSlug(t *testing.T) {
	zoneID := aliasTestZone(t)
	ctx := context.Background()

	// Seed mycelium-development as belonging to ownerAcct.
	seeded := "job.mycelium-development." + testDomain
	target := "job." + encodeAccountID(ownerAcct) + "." + testDomain
	if _, err := upsertCNAMERecord(ctx, seeded, target, zoneID); err != nil {
		t.Fatalf("seed alias: %v", err)
	}

	// The handler resolves the zone from DOMAIN_ZONES, not from the request.
	domainZones = map[string]string{testDomain: zoneID}
	defaultDomain = testDomain

	resp, err := handler(ctx, dnsRequestAs(otherAcct, "evil", "198.51.100.7", "mycelium-development"))
	if err != nil {
		t.Fatalf("handler returned an error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("expected the canonical record to still succeed, got %d: %s", resp.StatusCode, resp.Body)
	}
	// The canonical record is the caller's own and must be written...
	if !contains(resp.Body, encodeAccountID(otherAcct)) {
		t.Errorf("canonical record was not namespaced to the caller: %s", resp.Body)
	}
	// ...but the alias must NOT be, and must not appear in the success message.
	if contains(resp.Body, "mycelium-development") {
		t.Errorf("the handler reported writing an alias under another account's slug: %s", resp.Body)
	}
	if cnameExists(t, zoneID, "evil.mycelium-development."+testDomain,
		"evil."+encodeAccountID(otherAcct)+"."+testDomain) {
		t.Error("the handler WROTE an alias under another account's slug — #820 is reopened")
	}
	// And the victim's existing alias must be untouched.
	if !cnameExists(t, zoneID, seeded, target) {
		t.Error("the victim's existing alias was clobbered")
	}
}

// The owner's own alias still works through the handler, so the fix is not just
// "refuse everything".
func TestHandlerWritesAnAliasForItsOwner(t *testing.T) {
	zoneID := aliasTestZone(t)
	ctx := context.Background()
	domainZones = map[string]string{testDomain: zoneID}
	defaultDomain = testDomain

	resp, err := handler(ctx, dnsRequestAs(ownerAcct, "job", "198.51.100.8", "mycelium-development"))
	if err != nil {
		t.Fatalf("handler returned an error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, resp.Body)
	}
	if !contains(resp.Body, "mycelium-development") {
		t.Errorf("the owner's alias was refused: %s", resp.Body)
	}
	if !cnameExists(t, zoneID, "job.mycelium-development."+testDomain,
		"job."+encodeAccountID(ownerAcct)+"."+testDomain) {
		t.Error("the owner's alias CNAME was not written")
	}
}

// An ip_address that is not an IPv4 literal must be rejected before anything is
// written (#835). It used to be passed through to Route53 to reject.
func TestHandlerRejectsANonIPv4Address(t *testing.T) {
	zoneID := aliasTestZone(t)
	domainZones = map[string]string{testDomain: zoneID}
	defaultDomain = testDomain

	for _, bad := range []string{"not-an-ip", "999.1.1.1", "2001:db8::1", "198.51.100.7; rm -rf /", " "} {
		resp, err := handler(context.Background(), dnsRequestAs(ownerAcct, "job", bad, ""))
		if err != nil {
			t.Fatalf("handler error for %q: %v", bad, err)
		}
		if resp.StatusCode != 400 {
			t.Errorf("ip_address %q was accepted (status %d); it must be rejected here rather than "+
				"relying on Route53 to reject it after authorization", bad, resp.StatusCode)
		}
	}
}

func dnsRequestAs(accountID, recordName, ip, accountName string) events.LambdaFunctionURLRequest {
	body := map[string]string{
		"record_name": recordName,
		"ip_address":  ip,
		"action":      "UPSERT",
		"domain":      testDomain,
	}
	if accountName != "" {
		body["account_name"] = accountName
	}
	raw, _ := json.Marshal(body)
	return events.LambdaFunctionURLRequest{
		Body: string(raw),
		RequestContext: events.LambdaFunctionURLRequestContext{
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{Method: "POST"},
			Authorizer: &events.LambdaFunctionURLRequestContextAuthorizerDescription{
				IAM: &events.LambdaFunctionURLRequestContextAuthorizerIAMDescription{
					AccountID: accountID,
					UserARN:   "arn:aws:sts::" + accountID + ":assumed-role/spored-instance-role/i-0123",
				},
			},
		},
	}
}
