package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	r53types "github.com/aws/aws-sdk-go-v2/service/route53/types"

	"github.com/spore-host/spawn/pkg/dns"
)

// Fixtures reuse expiry_test.go's accounts: unmanagedID (4zlw3a1t) is not covered,
// managedID (1f1koq13) is. TestExpiryFixtureLabelsAreCorrect proves the labels.
var leaseNow = time.Date(2026, 10, 12, 21, 0, 0, 0, time.UTC)

func aSet(name, ip string) r53types.ResourceRecordSet {
	return r53types.ResourceRecordSet{
		Name: aws.String(name + "."), Type: r53types.RRTypeA, TTL: aws.Int64(60),
		ResourceRecords: []r53types.ResourceRecord{{Value: aws.String(ip)}},
	}
}

func leaseSet(record string, renewed time.Time) r53types.ResourceRecordSet {
	return r53types.ResourceRecordSet{
		Name: aws.String(dns.LeaseName(record) + "."), Type: r53types.RRTypeTxt, TTL: aws.Int64(60),
		ResourceRecords: []r53types.ResourceRecord{{Value: aws.String(dns.FormatLease(renewed))}},
	}
}

func rawTXT(name, value string) r53types.ResourceRecordSet {
	return r53types.ResourceRecordSet{
		Name: aws.String(name + "."), Type: r53types.RRTypeTxt, TTL: aws.Int64(60),
		ResourceRecords: []r53types.ResourceRecord{{Value: aws.String(value)}},
	}
}

var (
	staleRec   = "dead." + unmanagedSub
	freshRec   = "live." + unmanagedSub
	coveredRec = "hung." + managedSub
	noLeaseRec = "oldspored." + unmanagedSub
	orphanRec  = "gone." + unmanagedSub
)

func leaseZone() []r53types.ResourceRecordSet {
	stale := leaseNow.Add(-dns.LeaseMaxAge - time.Hour)
	return []r53types.ResourceRecordSet{
		{Name: aws.String("spore.host."), Type: r53types.RRTypeSoa},
		aSet(staleRec, "192.0.2.1"), leaseSet(staleRec, stale),
		aSet(freshRec, "192.0.2.2"), leaseSet(freshRec, leaseNow.Add(-30*time.Minute)),
		aSet(coveredRec, "192.0.2.3"), leaseSet(coveredRec, stale),
		aSet(noLeaseRec, "192.0.2.4"), // a pre-lease spored: no lease at all
		leaseSet(orphanRec, stale),    // a lease whose A-record is gone
	}
}

func verdictsByRecord(fs []leaseFinding) map[string]leaseVerdict {
	m := map[string]leaseVerdict{}
	for _, f := range fs {
		m[f.record] = f.verdict
	}
	return m
}

// The decision that authorizes a deletion.
func TestClassifyLeases(t *testing.T) {
	managed := map[string]bool{managedID: true}
	got := verdictsByRecord(classifyLeases(leaseZone(), "spore.host", managed, leaseNow, dns.LeaseMaxAge))

	want := map[string]leaseVerdict{
		staleRec:   leaseExpire,
		freshRec:   leaseKeep,
		coveredRec: leaseCovered, // stale, but the instance sweep owns covered accounts
		orphanRec:  leaseOrphan,
	}
	for rec, v := range want {
		if got[rec] != v {
			t.Errorf("%s: verdict %v, want %v", rec, got[rec], v)
		}
	}
	// A record with no lease is not a finding at all: it is never expired by lease.
	if _, ok := got[noLeaseRec]; ok {
		t.Errorf("%s has no lease but produced a finding — every pre-lease spored's record "+
			"would be at risk", noLeaseRec)
	}
}

// Only a stale lease in an UNCOVERED account may expire a record. A covered account
// with a stale lease is most likely a hung spored on a live instance.
func TestCoveredAccountsAreNeverExpiredByLease(t *testing.T) {
	managed := map[string]bool{managedID: true}
	for _, f := range classifyLeases(leaseZone(), "spore.host", managed, leaseNow, dns.LeaseMaxAge) {
		if f.account == managedID && authorizeLeaseAction(f.verdict, true) {
			t.Errorf("%s in covered account %s would be acted on; the DescribeInstances sweep is "+
				"authoritative there, and a stale lease most likely means a hung spored on a "+
				"live box", f.record, f.account)
		}
	}
}

func TestUnparseableLeaseIsNeverActedOn(t *testing.T) {
	rec := "garbled." + unmanagedSub
	zone := []r53types.ResourceRecordSet{
		aSet(rec, "192.0.2.9"),
		rawTXT(dns.LeaseName(rec), `"last tuesday"`),
	}
	fs := classifyLeases(zone, "spore.host", map[string]bool{managedID: true}, leaseNow, dns.LeaseMaxAge)
	if len(fs) != 1 || fs[0].verdict != leaseUnparseable {
		t.Fatalf("got %+v, want one unparseable finding", fs)
	}
	if authorizeLeaseAction(fs[0].verdict, true) {
		t.Error("an unparseable lease was authorized for action; what cannot be read cannot be judged stale")
	}
}

// TXT records that merely resemble leases are not ours to touch.
func TestNonLeaseTXTRecordsAreIgnored(t *testing.T) {
	zone := []r53types.ResourceRecordSet{
		rawTXT("_spore-owner.scott-s-test-account.spore.host", `"942542972736"`),
		rawTXT("spore.host", `"v=spf1 include:_spf.purelymail.com ~all"`),
		rawTXT("_spore-lease.box.not-base36-at-all.spore.host", dns.FormatLease(leaseNow.Add(-72*time.Hour))),
	}
	if fs := classifyLeases(zone, "spore.host", map[string]bool{managedID: true}, leaseNow, dns.LeaseMaxAge); len(fs) != 0 {
		t.Errorf("non-lease or foreign TXT records produced findings: %+v", fs)
	}
}

func TestNothingIsActedOnUnlessArmed(t *testing.T) {
	for _, v := range []leaseVerdict{leaseKeep, leaseExpire, leaseOrphan, leaseUnparseable, leaseCovered} {
		if authorizeLeaseAction(v, false) {
			t.Errorf("verdict %v authorized with REAPER_DNS_LEASE_EXPIRE off", v)
		}
	}
	for v, want := range map[leaseVerdict]bool{
		leaseKeep: false, leaseExpire: true, leaseOrphan: true, leaseUnparseable: false, leaseCovered: false,
	} {
		if got := authorizeLeaseAction(v, true); got != want {
			t.Errorf("armed, verdict %v: authorized=%v, want %v", v, got, want)
		}
	}
}

func leaseReaper(f route53API, armed, dryRun bool) *reaper {
	return &reaper{
		route53Client: f, dnsZoneID: "Z1", dnsDomain: "spore.host",
		accounts:     []account{{accountID: managedID, label: "managed"}},
		expireLeases: armed, dryRun: dryRun,
	}
}

// End to end through the real walk: what gets DELETED, which is the property.
func TestReportLeasesDeletesOnlyWhatItShould(t *testing.T) {
	t.Run("disarmed deletes nothing", func(t *testing.T) {
		f := &fakeRoute53{sets: leaseZone()}
		var sum Summary
		leaseReaper(f, false, false).reportLeases(context.Background(), &sum, leaseNow)
		if len(f.deleted) != 0 {
			t.Errorf("deleted %v with lease expiry off", f.deleted)
		}
		if sum.DNSLeasesStale != 1 || sum.DNSLeasesOrphaned != 1 {
			t.Errorf("stale=%d orphaned=%d, want 1 and 1 — the report is how an operator sees "+
				"what arming would do", sum.DNSLeasesStale, sum.DNSLeasesOrphaned)
		}
	})

	t.Run("dry-run deletes nothing", func(t *testing.T) {
		f := &fakeRoute53{sets: leaseZone()}
		var sum Summary
		leaseReaper(f, true, true).reportLeases(context.Background(), &sum, leaseNow)
		if len(f.deleted) != 0 {
			t.Errorf("deleted %v in dry-run", f.deleted)
		}
	})

	t.Run("armed deletes exactly the stale uncovered record and the orphaned lease", func(t *testing.T) {
		f := &fakeRoute53{sets: leaseZone()}
		var sum Summary
		leaseReaper(f, true, false).reportLeases(context.Background(), &sum, leaseNow)
		want := map[string]bool{
			"A " + staleRec:                   true,
			"TXT " + dns.LeaseName(staleRec):  true,
			"TXT " + dns.LeaseName(orphanRec): true,
		}
		got := map[string]bool{}
		for _, d := range f.deleted {
			got[d] = true
			if !want[d] {
				t.Errorf("deleted %s, which it must not", d)
			}
		}
		for d := range want {
			if !got[d] {
				t.Errorf("did not delete %s", d)
			}
		}
		if sum.DNSLeasesExpired != 1 {
			t.Errorf("DNSLeasesExpired = %d, want 1", sum.DNSLeasesExpired)
		}
	})
}

// A renewal that lands between the zone listing and the delete must win.
type renewingRoute53 struct {
	*fakeRoute53
	renewed string // lease name whose targeted re-read returns a fresh value
}

func (r *renewingRoute53) ListResourceRecordSets(ctx context.Context, in *route53.ListResourceRecordSetsInput, o ...func(*route53.Options)) (*route53.ListResourceRecordSetsOutput, error) {
	if in.MaxItems != nil && in.StartRecordType == r53types.RRTypeTxt &&
		strings.TrimSuffix(aws.ToString(in.StartRecordName), ".") == r.renewed {
		return &route53.ListResourceRecordSetsOutput{ResourceRecordSets: []r53types.ResourceRecordSet{
			rawTXT(r.renewed, dns.FormatLease(leaseNow.Add(-time.Minute))),
		}}, nil
	}
	return r.fakeRoute53.ListResourceRecordSets(ctx, in, o...)
}

func TestARenewalDuringTheRunWins(t *testing.T) {
	inner := &fakeRoute53{sets: leaseZone()}
	f := &renewingRoute53{fakeRoute53: inner, renewed: dns.LeaseName(staleRec)}
	var sum Summary
	leaseReaper(f, true, false).reportLeases(context.Background(), &sum, leaseNow)
	for _, d := range inner.deleted {
		if strings.HasSuffix(d, staleRec) {
			t.Errorf("deleted %s although its owner renewed it after the zone was listed", d)
		}
	}
	if sum.DNSLeasesExpired != 0 {
		t.Errorf("DNSLeasesExpired = %d, want 0", sum.DNSLeasesExpired)
	}
}

// With no resolved account IDs every account looks uncovered, so a covered
// account's hung spored would be expired. The walk must refuse to run.
func TestNoResolvedAccountsMeansNoLeaseWalk(t *testing.T) {
	f := &fakeRoute53{sets: leaseZone()}
	r := leaseReaper(f, true, false)
	r.accounts = []account{{label: "unresolved"}} // accountID empty
	var sum Summary
	r.reportLeases(context.Background(), &sum, leaseNow)
	if len(f.deleted) != 0 {
		t.Errorf("deleted %v with no resolved account IDs — every account looked uncovered", f.deleted)
	}
}
