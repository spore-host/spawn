package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	r53types "github.com/aws/aws-sdk-go-v2/service/route53/types"
	"github.com/spore-host/spawn/pkg/dns"
)

// #1065: the reaper's DNS work covered exactly one zone, so a spore registered in
// a custom zone (#891) had no backstop — a record its spored never deleted (a
// crash, an out-of-band terminate) stayed, pointing at a released IP.

const customDomain = "prismcloud.example"

// zonedRoute53 serves several zones, routing every call by HostedZoneId, so a test
// can see WHICH zone each deletion went to.
type zonedRoute53 map[string]*fakeRoute53

func (z zonedRoute53) zone(id *string) (*fakeRoute53, error) {
	f, ok := z[aws.ToString(id)]
	if !ok {
		return nil, errors.New("NoSuchHostedZone " + aws.ToString(id))
	}
	return f, nil
}

func (z zonedRoute53) ListResourceRecordSets(ctx context.Context, in *route53.ListResourceRecordSetsInput, o ...func(*route53.Options)) (*route53.ListResourceRecordSetsOutput, error) {
	f, err := z.zone(in.HostedZoneId)
	if err != nil {
		return nil, err
	}
	return f.ListResourceRecordSets(ctx, in, o...)
}

func (z zonedRoute53) ChangeResourceRecordSets(ctx context.Context, in *route53.ChangeResourceRecordSetsInput, o ...func(*route53.Options)) (*route53.ChangeResourceRecordSetsOutput, error) {
	f, err := z.zone(in.HostedZoneId)
	if err != nil {
		return nil, err
	}
	return f.ChangeResourceRecordSets(ctx, in, o...)
}

func twoZoneReaper(r53 route53API) *reaper {
	return &reaper{
		route53Client: r53, dnsZoneID: "ZPRIMARY", dnsDomain: "spore.host",
		dnsZones: []dnsZone{{domain: "spore.host", zoneID: "ZPRIMARY"}, {domain: customDomain, zoneID: "ZCUSTOM"}},
		accounts: []account{{accountID: managedID, label: "managed"}},
	}
}

// Teardown deletes in the zone the instance registered in.
func TestTeardownDeletesInTheInstancesOwnZone(t *testing.T) {
	primary := &fakeRoute53{sets: []r53types.ResourceRecordSet{aSet("web.abc.spore.host", "192.0.2.1")}}
	custom := &fakeRoute53{sets: []r53types.ResourceRecordSet{aSet("web.abc."+customDomain, "192.0.2.2")}}
	r := twoZoneReaper(zonedRoute53{"ZPRIMARY": primary, "ZCUSTOM": custom})
	ctx := context.Background()

	r.deleteDNS(ctx, candidate{id: "i-custom", dnsName: "web", accountBase36: "abc", dnsDomain: customDomain})
	if len(custom.deleted) != 1 || custom.deleted[0] != "A web.abc."+customDomain {
		t.Errorf("custom-zone spore: deleted %v in the custom zone, want its A record", custom.deleted)
	}
	if len(primary.deleted) != 0 {
		t.Errorf("custom-zone spore: deleted %v in the PRIMARY zone", primary.deleted)
	}

	// No spawn:dns-domain: every pre-#891 spore — the primary zone, as before.
	r.deleteDNS(ctx, candidate{id: "i-plain", dnsName: "web", accountBase36: "abc"})
	if len(primary.deleted) != 1 || primary.deleted[0] != "A web.abc.spore.host" {
		t.Errorf("plain spore: deleted %v in the primary zone, want its A record", primary.deleted)
	}

	// A zone this reaper does not cover: nothing is touched anywhere.
	before := len(primary.deleted) + len(custom.deleted)
	r.deleteDNS(ctx, candidate{id: "i-other", dnsName: "web", accountBase36: "abc", dnsDomain: "elsewhere.example"})
	if len(primary.deleted)+len(custom.deleted) != before {
		t.Errorf("an uncovered zone's spore caused deletions: primary=%v custom=%v", primary.deleted, custom.deleted)
	}
}

// The zone-wide walks run in every covered zone: a stale lease in the custom zone
// is expired there, exactly as one in the primary zone is.
func TestZoneWalksCoverEveryZone(t *testing.T) {
	inCustom := func(sets []r53types.ResourceRecordSet) []r53types.ResourceRecordSet {
		out := make([]r53types.ResourceRecordSet, len(sets))
		for i, rs := range sets {
			rs.Name = aws.String(strings.Replace(aws.ToString(rs.Name), "spore.host", customDomain, 1))
			if rs.Type == r53types.RRTypeSoa {
				rs.Name = aws.String(customDomain + ".")
			}
			out[i] = rs
		}
		return out
	}
	primary := &fakeRoute53{sets: leaseZone()}
	custom := &fakeRoute53{sets: inCustom(leaseZone())}
	r := twoZoneReaper(zonedRoute53{"ZPRIMARY": primary, "ZCUSTOM": custom})
	r.expireLeases = true

	var sum Summary
	r.reportEveryZone(context.Background(), &sum, leaseNow)

	customStale := strings.Replace(staleRec, "spore.host", customDomain, 1)
	has := func(f *fakeRoute53, want string) bool {
		for _, d := range f.deleted {
			if d == want {
				return true
			}
		}
		return false
	}
	if !has(custom, "A "+customStale) || !has(custom, "TXT "+dns.LeaseName(customStale)) {
		t.Errorf("the stale lease in the custom zone was not expired there; custom deleted %v", custom.deleted)
	}
	if !has(primary, "A "+staleRec) {
		t.Errorf("the primary zone's stale lease was not expired; primary deleted %v", primary.deleted)
	}
	for _, d := range primary.deleted {
		if strings.Contains(d, customDomain) {
			t.Errorf("a custom-zone record was deleted through the PRIMARY zone: %s", d)
		}
	}
	if sum.DNSLeasesExpired != 2 {
		t.Errorf("DNSLeasesExpired = %d, want 2 (one per zone)", sum.DNSLeasesExpired)
	}
}

// A reaper built from the primary fields alone (every deployment before #1065)
// still covers its one zone.
func TestPrimaryOnlyReaperStillCoversItsZone(t *testing.T) {
	r := &reaper{dnsZoneID: "ZPRIMARY", dnsDomain: "spore.host"}
	if z := r.zones(); len(z) != 1 || z[0].zoneID != "ZPRIMARY" {
		t.Errorf("zones() = %v, want the primary", z)
	}
	if _, ok := r.zoneFor(""); !ok {
		t.Error("zoneFor(\"\") found nothing; pre-#891 spores would lose their teardown")
	}
	if z := (&reaper{}).zones(); z != nil {
		t.Errorf("an unconfigured reaper covers %v, want nothing", z)
	}
}

type fakeZoneGetter map[string]r53types.HostedZone

func (f fakeZoneGetter) GetHostedZone(_ context.Context, in *route53.GetHostedZoneInput, _ ...func(*route53.Options)) (*route53.GetHostedZoneOutput, error) {
	hz, ok := f[aws.ToString(in.Id)]
	if !ok {
		return nil, errors.New("AccessDenied")
	}
	return &route53.GetHostedZoneOutput{HostedZone: &hz}, nil
}

// Only zone IDs are configured; each domain is read from the zone itself, so the
// IAM grant (built from the same IDs) and the reaper cannot disagree.
func TestResolveExtraZones(t *testing.T) {
	g := fakeZoneGetter{
		"ZCUSTOM":  {Name: aws.String(customDomain + ".")},
		"ZPRIVATE": {Name: aws.String("internal.example."), Config: &r53types.HostedZoneConfig{PrivateZone: true}},
		"ZDUP":     {Name: aws.String("spore.host.")},
	}
	primary := dnsZone{domain: "spore.host", zoneID: "ZPRIMARY"}
	zones, errs := resolveExtraZones(context.Background(), g, " /hostedzone/ZCUSTOM, ZPRIVATE,ZDUP,ZDENIED,,ZPRIMARY", primary)

	if len(zones) != 1 || zones[0] != (dnsZone{domain: customDomain, zoneID: "ZCUSTOM"}) {
		t.Errorf("zones = %v, want only {%s ZCUSTOM}", zones, customDomain)
	}
	joined := ""
	for _, e := range errs {
		joined += e.Error() + "\n"
	}
	for _, want := range []string{"ZPRIVATE is private", "ZDUP (spore.host) is already covered", "ZDENIED", "ZPRIMARY"} {
		if !strings.Contains(joined, want) {
			t.Errorf("errors do not report %q:\n%s", want, joined)
		}
	}
	if z, errs := resolveExtraZones(context.Background(), g, "", primary); len(z) != 0 || len(errs) != 0 {
		t.Errorf("empty config = (%v, %v), want nothing", z, errs)
	}
}

// Through run() itself, so the call sites are what is tested, not a helper they
// happen to call: one account with no live instances, an orphaned A-record of
// that account in EACH zone, and the lease fixture in the custom zone. run must
// sweep both zones and expire the custom zone's stale lease.
func TestRunCoversEveryZone(t *testing.T) {
	managedLabel := strings.TrimSuffix(managedSub, ".spore.host") // base36 of managedID
	orphan := func(domain string) r53types.ResourceRecordSet {
		return aSet("ghost."+managedLabel+"."+domain, "192.0.2.99")
	}
	customLeases := []r53types.ResourceRecordSet{}
	for _, rs := range leaseZone() {
		rs.Name = aws.String(strings.Replace(aws.ToString(rs.Name), "spore.host", customDomain, 1))
		customLeases = append(customLeases, rs)
	}
	primary := &fakeRoute53{sets: []r53types.ResourceRecordSet{{Name: aws.String("spore.host."), Type: r53types.RRTypeSoa}, orphan("spore.host")}}
	custom := &fakeRoute53{sets: append(customLeases, orphan(customDomain))}

	r := twoZoneReaper(zonedRoute53{"ZPRIMARY": primary, "ZCUSTOM": custom})
	r.accounts = []account{{label: "managed", accountID: managedID, ec2For: func(string) ec2API { return emptyEC2{} }}}
	r.regions = []string{"us-east-1"}
	r.maxAge = defaultMaxAge
	r.sweepDNS, r.expireLeases = true, true

	captureLogs(func() { _, _ = r.run(context.Background()) })

	has := func(f *fakeRoute53, want string) bool {
		for _, d := range f.deleted {
			if d == want {
				return true
			}
		}
		return false
	}
	if !has(primary, "A ghost."+managedLabel+".spore.host") {
		t.Errorf("the sweep did not reach the primary zone; primary deleted %v", primary.deleted)
	}
	if !has(custom, "A ghost."+managedLabel+"."+customDomain) {
		t.Errorf("the sweep did not reach the custom zone; custom deleted %v", custom.deleted)
	}
	customStale := strings.Replace(staleRec, "spore.host", customDomain, 1)
	if !has(custom, "TXT "+dns.LeaseName(customStale)) {
		t.Errorf("lease expiry did not reach the custom zone; custom deleted %v", custom.deleted)
	}
}

// The startup wiring: REAPER_DNS_EXTRA_ZONE_IDS becomes covered zones, after the
// primary, and an unresolvable one is reported without disabling the rest.
func TestConfigureDNSZones(t *testing.T) {
	r := &reaper{dnsZoneID: "ZPRIMARY", dnsDomain: "spore.host"}
	out := captureLogs(func() {
		r.configureDNSZones(context.Background(), fakeZoneGetter{"ZCUSTOM": {Name: aws.String(customDomain + ".")}}, "ZCUSTOM,ZDENIED")
	})
	want := []dnsZone{{domain: "spore.host", zoneID: "ZPRIMARY"}, {domain: customDomain, zoneID: "ZCUSTOM"}}
	if len(r.dnsZones) != 2 || r.dnsZones[0] != want[0] || r.dnsZones[1] != want[1] {
		t.Errorf("dnsZones = %v, want %v", r.dnsZones, want)
	}
	if !strings.Contains(out, "ZDENIED") || !strings.Contains(out, "NOT covered") {
		t.Errorf("an unresolvable zone was not reported:\n%s", out)
	}
	if !strings.Contains(out, "DNS zones covered: spore.host, "+customDomain) {
		t.Errorf("the covered set is not logged:\n%s", out)
	}
}
