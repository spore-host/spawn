package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	r53types "github.com/aws/aws-sdk-go-v2/service/route53/types"
	"github.com/scttfrdmn/substrate/emulator"
)

// DNS lease behaviour, driven through the real write functions against the
// Substrate Route53 emulator — not by matching source text, because the property is
// what ends up in the zone.
//
// Zone creation FAILS rather than skips. The CNAME test beside this one skips when
// the emulator cannot create a zone, which turns an emulator regression into
// silently absent coverage; for the lease that would mean the one test proving a
// live record cannot be expired by a stale lease stops running without anyone
// noticing.
func leaseTestZone(t *testing.T) string {
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
	zone, err := route53Client.CreateHostedZone(context.Background(), &route53.CreateHostedZoneInput{
		Name:            aws.String("spore.host"),
		CallerReference: aws.String("dns-lease-test-" + t.Name()),
	})
	if err != nil {
		t.Fatalf("substrate CreateHostedZone: %v", err)
	}
	return aws.ToString(zone.HostedZone.Id)
}

// leaseRecord returns the lease TXT value for fqdn, or "" if there is none.
func leaseRecord(t *testing.T, zoneID, fqdn string) string {
	t.Helper()
	rs, err := existingLease(context.Background(), fqdn, zoneID)
	if err != nil {
		t.Fatalf("existingLease: %v", err)
	}
	if rs == nil || len(rs.ResourceRecords) == 0 {
		return ""
	}
	return aws.ToString(rs.ResourceRecords[0].Value)
}

func aRecordExists(t *testing.T, zoneID, fqdn string) bool {
	t.Helper()
	out, err := route53Client.ListResourceRecordSets(context.Background(), &route53.ListResourceRecordSetsInput{
		HostedZoneId: aws.String(zoneID),
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, rs := range out.ResourceRecordSets {
		if strings.TrimSuffix(aws.ToString(rs.Name), ".") == fqdn && rs.Type == r53types.RRTypeA {
			return true
		}
	}
	return false
}

const leaseFQDN = "loop-l12-esc.c0zxr0ao.spore.host"

// A renewing client gets a lease, dated now, beside its record.
func TestRenewingRegistrationWritesALease(t *testing.T) {
	zoneID := leaseTestZone(t)
	before := time.Now().UTC().Add(-time.Second)

	if _, err := upsertDNSRecord(context.Background(), leaseFQDN, "192.0.2.10", zoneID, true); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if !aRecordExists(t, zoneID, leaseFQDN) {
		t.Fatal("A-record not written")
	}
	v := leaseRecord(t, zoneID, leaseFQDN)
	if v == "" {
		t.Fatal("a registration with lease=true wrote no lease, so the record can never be " +
			"expired once its owner stops renewing — the gap this exists to close")
	}
	got, err := time.Parse(time.RFC3339, strings.Trim(v, `"`))
	if err != nil {
		t.Fatalf("lease value %q is not RFC 3339 — the reaper would be unable to read it", v)
	}
	if got.Before(before) || got.After(time.Now().UTC().Add(time.Second)) {
		t.Errorf("lease dated %s, want the time of this registration", got)
	}
}

// Renewal moves the lease forward.
func TestReRegistrationRenewsTheLease(t *testing.T) {
	zoneID := leaseTestZone(t)
	if _, err := upsertDNSRecord(context.Background(), leaseFQDN, "192.0.2.10", zoneID, true); err != nil {
		t.Fatal(err)
	}
	first := leaseRecord(t, zoneID, leaseFQDN)
	time.Sleep(1100 * time.Millisecond) // RFC 3339 is second-granular
	if _, err := upsertDNSRecord(context.Background(), leaseFQDN, "192.0.2.10", zoneID, true); err != nil {
		t.Fatal(err)
	}
	if second := leaseRecord(t, zoneID, leaseFQDN); second == first {
		t.Errorf("re-registration left the lease at %s — renewal is not renewing", first)
	}
}

// THE property: a record re-registered by a client that does not renew must lose
// its old lease, or the reaper expires a record whose instance is alive.
func TestNonRenewingRegistrationClearsAnInheritedLease(t *testing.T) {
	zoneID := leaseTestZone(t)
	if _, err := upsertDNSRecord(context.Background(), leaseFQDN, "192.0.2.10", zoneID, true); err != nil {
		t.Fatal(err)
	}
	if leaseRecord(t, zoneID, leaseFQDN) == "" {
		t.Fatal("setup: no lease written")
	}

	// Same name, now claimed by an older spored that registers once at boot.
	if _, err := upsertDNSRecord(context.Background(), leaseFQDN, "192.0.2.20", zoneID, false); err != nil {
		t.Fatal(err)
	}
	if v := leaseRecord(t, zoneID, leaseFQDN); v != "" {
		t.Errorf("a non-renewing registration left the previous owner's lease (%s) in place. "+
			"It will never be renewed, so the reaper would expire this record while its "+
			"instance is alive.", v)
	}
	if !aRecordExists(t, zoneID, leaseFQDN) {
		t.Error("the A-record itself was lost")
	}
}

// Every client that exists today sends no lease field. Its behaviour must be
// exactly what it was: a record, and nothing else.
func TestOldClientsGetNoLease(t *testing.T) {
	zoneID := leaseTestZone(t)
	if _, err := upsertDNSRecord(context.Background(), leaseFQDN, "192.0.2.10", zoneID, false); err != nil {
		t.Fatal(err)
	}
	if v := leaseRecord(t, zoneID, leaseFQDN); v != "" {
		t.Errorf("a client that did not ask for a lease got one (%s) — it will never renew it", v)
	}
}

// Deleting a record removes its lease.
func TestDeleteRemovesTheLease(t *testing.T) {
	zoneID := leaseTestZone(t)
	if _, err := upsertDNSRecord(context.Background(), leaseFQDN, "192.0.2.10", zoneID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := deleteDNSRecord(context.Background(), leaseFQDN, zoneID); err != nil {
		t.Fatal(err)
	}
	if err := removeLease(context.Background(), leaseFQDN, zoneID); err != nil {
		t.Fatal(err)
	}
	if v := leaseRecord(t, zoneID, leaseFQDN); v != "" {
		t.Errorf("lease %s survived its record's deletion", v)
	}
	// And removing a lease that is not there is a no-op.
	if err := removeLease(context.Background(), leaseFQDN, zoneID); err != nil {
		t.Errorf("removing an absent lease errored: %v", err)
	}
}

// The wire contract, through the real handler: a request body is what an old or new
// spored actually sends. The function-level tests above cannot catch a wrong json
// tag or a call site that hardcodes the flag, because they never parse a request.
func TestHandlerMapsTheLeaseFieldFromTheWire(t *testing.T) {
	zoneID := leaseTestZone(t)
	prevZones, prevDefault := domainZones, defaultDomain
	t.Cleanup(func() { domainZones, defaultDomain = prevZones, prevDefault })
	domainZones = map[string]string{"spore.host": zoneID}
	defaultDomain = "spore.host"
	stubRegistry(t, []string{"942542972736"}, nil)

	// The account whose base36 is c0zxr0ao, so the FQDN matches leaseFQDN.
	const acct = "942542972736"
	fqdn := getFullDNSName("loop-l12-esc", acct, "spore.host")
	if fqdn != leaseFQDN {
		t.Fatalf("fixture: FQDN %s != %s", fqdn, leaseFQDN)
	}

	call := func(body string) {
		t.Helper()
		resp, err := handler(context.Background(), events.LambdaFunctionURLRequest{
			Body: body,
			RequestContext: events.LambdaFunctionURLRequestContext{
				Authorizer: &events.LambdaFunctionURLRequestContextAuthorizerDescription{
					IAM: &events.LambdaFunctionURLRequestContextAuthorizerIAMDescription{AccountID: acct},
				},
			},
		})
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("handler: status %d err %v body %s", resp.StatusCode, err, resp.Body)
		}
	}

	// Exactly what every spored released before this field existed sends.
	call(`{"record_name":"loop-l12-esc","ip_address":"192.0.2.10","action":"UPSERT"}`)
	if v := leaseRecord(t, zoneID, leaseFQDN); v != "" {
		t.Errorf("a request with no lease field produced a lease (%s). Every existing spored "+
			"sends exactly this body and none of them renew, so each such record would be "+
			"expired under a live instance.", v)
	}

	call(`{"record_name":"loop-l12-esc","ip_address":"192.0.2.10","action":"UPSERT","lease":true}`)
	if v := leaseRecord(t, zoneID, leaseFQDN); v == "" {
		t.Error(`"lease":true on the wire produced no lease — the json tag or the call site is wrong`)
	}
}
