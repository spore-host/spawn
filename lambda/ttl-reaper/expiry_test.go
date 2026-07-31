package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	r53types "github.com/aws/aws-sdk-go-v2/service/route53/types"
	"github.com/spore-host/spawn/pkg/dns"
	"github.com/spore-host/spore-host/lambda/accountlifecycle"
)

var expiryNow = time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

// row builds a registry row in a given lifecycle state, with the timestamps the
// prober would have stamped alongside it.
func row(id, status string) accountlifecycle.Account {
	return accountlifecycle.Account{
		AccountID:       id,
		RoleArn:         "arn:aws:iam::" + id + ":role/spore-portal-onboard",
		Status:          status,
		StatusChangedAt: expiryNow.Add(-48 * time.Hour).Format(time.RFC3339),
		LastSeenAt:      expiryNow.Add(-1 * time.Hour).Format(time.RFC3339),
	}
}

func rowsOf(accts ...accountlifecycle.Account) map[string]accountlifecycle.Account {
	m := make(map[string]accountlifecycle.Account, len(accts))
	for _, a := range accts {
		m[a.AccountID] = a
	}
	return m
}

// TestClassifySubdomain covers the verdict for every lifecycle state an unmanaged
// subdomain's account can be in. The two that authorize a deletion are the whole
// point; the ones that refuse are what keeps a working spore reachable.
func TestClassifySubdomain(t *testing.T) {
	const id = "390967728545"

	cases := []struct {
		name     string
		rows     map[string]accountlifecycle.Account
		eligible bool
		wantIn   string // substring the operator-facing detail must contain
	}{
		{
			name:     "dormant is eligible — emptiness was PROVEN through a working role",
			rows:     rowsOf(row(id, accountlifecycle.StatusDormant)),
			eligible: true,
			wantIn:   "ELIGIBLE",
		},
		{
			name:     "offboarded is eligible — a human stated the intent",
			rows:     rowsOf(row(id, accountlifecycle.StatusOffboarded)),
			eligible: true,
			wantIn:   "ELIGIBLE",
		},
		{
			name:     "active must never be eligible — the account is in use",
			rows:     rowsOf(row(id, accountlifecycle.StatusActive)),
			eligible: false,
			wantIn:   "not eligible",
		},
		{
			name: "unreachable must never be eligible (#457 trap 2) and must say why",
			rows: rowsOf(row(id, accountlifecycle.StatusUnreachable)),
			// The state we would most LIKE to clean up, and the one where the role we
			// would have verified through is gone. A longer wait cannot fix it.
			eligible: false,
			wantIn:   "needs a human",
		},
		{
			name:     "a legacy row with no status reads as active, not as eligible",
			rows:     rowsOf(row(id, "")),
			eligible: false,
			wantIn:   "not eligible",
		},
		{
			name:     "an unknown future status defaults to refusing",
			rows:     rowsOf(row(id, "quiescent-pending-review")),
			eligible: false,
			wantIn:   "not eligible",
		},
		{
			name:     "absent from the registry is a verdict of NONE, not a verdict of yes",
			rows:     rowsOf(row("111111111111", accountlifecycle.StatusDormant)),
			eligible: false,
			wantIn:   "not in the portal registry",
		},
		{
			name:     "a nil registry map (unreadable Scan) makes nothing eligible",
			rows:     nil,
			eligible: false,
			wantIn:   "not in the portal registry",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := classifySubdomain(id, tc.rows, expiryNow)
			if v.eligible != tc.eligible {
				t.Errorf("eligible = %t, want %t (detail: %s)", v.eligible, tc.eligible, v.detail)
			}
			if !strings.Contains(v.detail, tc.wantIn) {
				t.Errorf("detail = %q, want it to contain %q — the reason is what an operator acts on", v.detail, tc.wantIn)
			}
		})
	}
}

// TestAuthorizeExpiry is the full truth table for the gate that turns a verdict into
// a deletion. Exactly ONE row may delete; every other combination must not, because
// spored registers DNS once at boot and a wrongly deleted record never self-heals.
func TestAuthorizeExpiry(t *testing.T) {
	eligible := expiryVerdict{eligible: true, status: accountlifecycle.StatusDormant}
	ineligible := expiryVerdict{status: accountlifecycle.StatusActive}

	cases := []struct {
		v          expiryVerdict
		registryOK bool
		expireDNS  bool
		want       expiryAction
		why        string
	}{
		{eligible, true, true, actionExpire, "the only combination that may delete"},
		{eligible, true, false, actionReport, "eligible but the flag is off — the default, and it must delete nothing"},
		{eligible, false, true, actionReport, "an unreadable registry is not a verdict, however eligible the stale row looked"},
		{eligible, false, false, actionReport, "neither precondition holds"},
		{ineligible, true, true, actionReport, "the flag does not override the verdict — this is the live-account case"},
		{ineligible, true, false, actionReport, "ineligible and off"},
		{ineligible, false, true, actionReport, "ineligible with no registry"},
		{ineligible, false, false, actionReport, "nothing holds"},
	}

	for _, tc := range cases {
		got := authorizeExpiry(tc.v, tc.registryOK, tc.expireDNS)
		if got != tc.want {
			t.Errorf("authorizeExpiry(eligible=%t, registryOK=%t, expireDNS=%t) = %v, want %v — %s",
				tc.v.eligible, tc.registryOK, tc.expireDNS, got, tc.want, tc.why)
		}
	}
}

// fakeRegistry drives the reaper's read of the portal registry.
type fakeRegistry struct {
	rows []accountlifecycle.Account
	err  error
}

func (f *fakeRegistry) ListAccounts(ctx context.Context) ([]accountlifecycle.Account, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

// fakeRoute53 serves a fixed zone and records every deletion. The recorded deletes
// are the sharpest assertion in this file: most of these tests assert that the slice
// is EMPTY.
type fakeRoute53 struct {
	sets    []r53types.ResourceRecordSet
	deleted []string // "TYPE name", in the order sent
	listErr error
}

func (f *fakeRoute53) ListResourceRecordSets(ctx context.Context, in *route53.ListResourceRecordSetsInput, _ ...func(*route53.Options)) (*route53.ListResourceRecordSetsOutput, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	// deleteRecord reads back the exact record set it is about to delete (Route53
	// requires the current rdata), and asks for one item by name+type.
	if in.MaxItems != nil && aws.ToInt32(in.MaxItems) == 1 && in.StartRecordName != nil {
		want := strings.TrimSuffix(aws.ToString(in.StartRecordName), ".")
		for _, rs := range f.sets {
			if strings.TrimSuffix(aws.ToString(rs.Name), ".") == want && rs.Type == in.StartRecordType {
				return &route53.ListResourceRecordSetsOutput{ResourceRecordSets: []r53types.ResourceRecordSet{rs}}, nil
			}
		}
		return &route53.ListResourceRecordSetsOutput{}, nil
	}
	return &route53.ListResourceRecordSetsOutput{ResourceRecordSets: f.sets}, nil
}

func (f *fakeRoute53) ChangeResourceRecordSets(ctx context.Context, in *route53.ChangeResourceRecordSetsInput, _ ...func(*route53.Options)) (*route53.ChangeResourceRecordSetsOutput, error) {
	for _, c := range in.ChangeBatch.Changes {
		if c.Action == r53types.ChangeActionDelete {
			f.deleted = append(f.deleted, string(c.ResourceRecordSet.Type)+" "+
				strings.TrimSuffix(aws.ToString(c.ResourceRecordSet.Name), "."))
		}
	}
	return &route53.ChangeResourceRecordSetsOutput{}, nil
}

// The unmanaged account under test, and a managed one so the diff has something to
// exclude (with no managed IDs the report skips entirely).
const (
	unmanagedID  = "390967728545"
	unmanagedSub = "4zlw3a1t.spore.host"
	managedID    = "111111111111"
	managedSub   = "1f1koq13.spore.host"
)

// The base36 labels above are hardcoded so the fixture reads like a real zone. Assert
// they actually decode as intended: getting one wrong silently turns a "managed,
// must-not-touch" record into a second unmanaged subdomain, which weakens every test
// below without failing any of them.
func TestExpiryFixtureLabelsAreCorrect(t *testing.T) {
	for id, sub := range map[string]string{unmanagedID: unmanagedSub, managedID: managedSub} {
		label := strings.TrimSuffix(sub, ".spore.host")
		got, ok := dns.DecodeAccountID(label)
		if !ok || got != id {
			t.Errorf("label %q decodes to (%q, %t), want %q — fixture drift", label, got, ok, id)
		}
	}
}

func a(name, ip string) r53types.ResourceRecordSet {
	return r53types.ResourceRecordSet{
		Name:            aws.String(name),
		Type:            r53types.RRTypeA,
		ResourceRecords: []r53types.ResourceRecord{{Value: aws.String(ip)}},
	}
}

// zone is the fixture zone: two A-records and one CNAME under the unmanaged
// subdomain, plus a record under a managed account that must never be touched.
func zone() []r53types.ResourceRecordSet {
	return []r53types.ResourceRecordSet{
		a("spore.host.", "203.0.113.1"), // apex
		a("box1."+unmanagedSub+".", "54.1.2.3"),
		a("box2."+unmanagedSub+".", "54.1.2.4"),
		{
			Name:            aws.String("friendly." + unmanagedSub + "."),
			Type:            r53types.RRTypeCname,
			ResourceRecords: []r53types.ResourceRecord{{Value: aws.String("box1." + unmanagedSub)}},
		},
		a("box."+managedSub+".", "54.9.9.9"), // a MANAGED account's record
	}
}

// newExpiryReaper wires a reaper over the fakes with the zone configured and the
// sweep on, which is the precondition for the unmanaged walk running at all.
func newExpiryReaper(r53 route53API, reg registryAPI, expireDNS bool) *reaper {
	return &reaper{
		accounts:      []account{{label: "managed", accountID: managedID}},
		route53Client: r53,
		dnsZoneID:     "Z123",
		dnsDomain:     "spore.host",
		sweepDNS:      true,
		registry:      reg,
		expireDNS:     expireDNS,
	}
}

// The positive case: dormant + the flag + not dry-run deletes exactly the
// subdomain's A-records — no CNAME, no other account's record.
func TestReportUnmanagedSubdomains_DormantAndFlagDeletesARecordsOnly(t *testing.T) {
	r53 := &fakeRoute53{sets: zone()}
	reg := &fakeRegistry{rows: []accountlifecycle.Account{row(unmanagedID, accountlifecycle.StatusDormant)}}
	rp := newExpiryReaper(r53, reg, true)

	var sum Summary
	rp.reportUnmanagedSubdomains(context.Background(), &sum, expiryNow)

	want := []string{"A box1." + unmanagedSub, "A box2." + unmanagedSub}
	if len(r53.deleted) != len(want) {
		t.Fatalf("deleted %v, want exactly %v", r53.deleted, want)
	}
	for i := range want {
		if r53.deleted[i] != want[i] {
			t.Errorf("deleted[%d] = %q, want %q", i, r53.deleted[i], want[i])
		}
	}
	// Spelled out separately because these are the two ways this could go wrong
	// destructively rather than merely incorrectly.
	for _, got := range r53.deleted {
		if strings.HasPrefix(got, "CNAME") {
			t.Errorf("deleted %q: the #121 CNAME carries no IP, so it is not part of the hazard", got)
		}
		if strings.Contains(got, "1f1koq13") {
			t.Errorf("deleted %q: that subdomain belongs to a MANAGED account", got)
		}
	}
	if sum.DNSExpiryEligible != 1 || sum.DNSExpiredRecords != 2 {
		t.Errorf("summary = %+v, want 1 eligible / 2 expired records", sum)
	}
}

// Eligible, but the flag is off: the default posture. Report everything, delete
// nothing. If this fails, an upgrade silently starts deleting records.
func TestReportUnmanagedSubdomains_EligibleButFlagOffDeletesNothing(t *testing.T) {
	r53 := &fakeRoute53{sets: zone()}
	reg := &fakeRegistry{rows: []accountlifecycle.Account{row(unmanagedID, accountlifecycle.StatusDormant)}}
	rp := newExpiryReaper(r53, reg, false)

	var sum Summary
	rp.reportUnmanagedSubdomains(context.Background(), &sum, expiryNow)

	if len(r53.deleted) != 0 {
		t.Errorf("deleted %v with REAPER_DNS_EXPIRE off", r53.deleted)
	}
	// The verdict must still be computed and reported — that is how an operator
	// decides whether enabling the flag is safe.
	if sum.DNSExpiryEligible != 1 {
		t.Errorf("summary = %+v, want the eligible verdict still counted", sum)
	}
	if sum.DNSExpiredRecords != 0 {
		t.Errorf("summary = %+v, want 0 expired records", sum)
	}
}

// dry-run decides and reports, and sends nothing to Route53.
func TestReportUnmanagedSubdomains_DryRunDeletesNothing(t *testing.T) {
	r53 := &fakeRoute53{sets: zone()}
	reg := &fakeRegistry{rows: []accountlifecycle.Account{row(unmanagedID, accountlifecycle.StatusDormant)}}
	rp := newExpiryReaper(r53, reg, true)
	rp.dryRun = true

	var sum Summary
	rp.reportUnmanagedSubdomains(context.Background(), &sum, expiryNow)

	if len(r53.deleted) != 0 {
		t.Errorf("deleted %v in dry-run", r53.deleted)
	}
	if sum.DNSExpiredRecords != 2 {
		t.Errorf("summary = %+v, want the 2 would-delete records counted", sum)
	}
}

// A failed registry Scan must delete nothing and must be visible as an error. The
// dangerous failure mode is treating an empty read as "no accounts are eligible" —
// which is safe here only by accident, so it is asserted directly.
func TestReportUnmanagedSubdomains_RegistryErrorDeletesNothing(t *testing.T) {
	r53 := &fakeRoute53{sets: zone()}
	reg := &fakeRegistry{err: errors.New("dynamodb unavailable")}
	rp := newExpiryReaper(r53, reg, true)

	var sum Summary
	rp.reportUnmanagedSubdomains(context.Background(), &sum, expiryNow)

	if len(r53.deleted) != 0 {
		t.Errorf("deleted %v with an unreadable registry", r53.deleted)
	}
	if sum.Errors != 1 {
		t.Errorf("errors = %d, want 1 — a registry unreadable for weeks must not look like a clean run", sum.Errors)
	}
	if sum.DNSUnmanagedSubdomains != 1 {
		t.Errorf("summary = %+v, want the subdomain still reported", sum)
	}
}

// An account absent from the registry deletes nothing: the prober has no opinion
// about it, so the original #458 ambiguity stands in full.
func TestReportUnmanagedSubdomains_AbsentFromRegistryDeletesNothing(t *testing.T) {
	r53 := &fakeRoute53{sets: zone()}
	// A DIFFERENT account is dormant. Nothing about that authorizes this subdomain.
	reg := &fakeRegistry{rows: []accountlifecycle.Account{row("222222222222", accountlifecycle.StatusDormant)}}
	rp := newExpiryReaper(r53, reg, true)

	var sum Summary
	rp.reportUnmanagedSubdomains(context.Background(), &sum, expiryNow)

	if len(r53.deleted) != 0 {
		t.Errorf("deleted %v for an account with no registry row", r53.deleted)
	}
	if sum.DNSExpiryIneligible != 1 {
		t.Errorf("summary = %+v, want the subdomain counted ineligible", sum)
	}
}

// unreachable deletes nothing, with the flag on and everything else in place. This
// is #457 trap 2: the role we would verify through is gone, so emptiness can no
// longer be proven — and "probably uninstalled" is not proof.
func TestReportUnmanagedSubdomains_UnreachableDeletesNothing(t *testing.T) {
	r53 := &fakeRoute53{sets: zone()}
	reg := &fakeRegistry{rows: []accountlifecycle.Account{row(unmanagedID, accountlifecycle.StatusUnreachable)}}
	rp := newExpiryReaper(r53, reg, true)

	var sum Summary
	rp.reportUnmanagedSubdomains(context.Background(), &sum, expiryNow)

	if len(r53.deleted) != 0 {
		t.Errorf("deleted %v for an unreachable account — emptiness is unprovable there", r53.deleted)
	}
	if sum.DNSExpiryIneligible != 1 {
		t.Errorf("summary = %+v, want 1 ineligible", sum)
	}
}

// An ACTIVE account's records must survive with the flag on. This is the case where
// getting it wrong costs a running researcher their hostname until the box reboots.
func TestReportUnmanagedSubdomains_ActiveAccountDeletesNothing(t *testing.T) {
	r53 := &fakeRoute53{sets: zone()}
	reg := &fakeRegistry{rows: []accountlifecycle.Account{row(unmanagedID, accountlifecycle.StatusActive)}}
	rp := newExpiryReaper(r53, reg, true)

	var sum Summary
	rp.reportUnmanagedSubdomains(context.Background(), &sum, expiryNow)

	if len(r53.deleted) != 0 {
		t.Errorf("deleted %v for an ACTIVE account", r53.deleted)
	}
}

// A nil registry (no zone-independent configuration path wired it) behaves like an
// unreadable one: report, delete nothing, and do NOT count an error — nothing failed.
func TestReportUnmanagedSubdomains_NilRegistryDeletesNothing(t *testing.T) {
	r53 := &fakeRoute53{sets: zone()}
	rp := newExpiryReaper(r53, nil, true)

	var sum Summary
	rp.reportUnmanagedSubdomains(context.Background(), &sum, expiryNow)

	if len(r53.deleted) != 0 {
		t.Errorf("deleted %v with no registry configured", r53.deleted)
	}
	if sum.Errors != 0 {
		t.Errorf("errors = %d, want 0 — an unconfigured registry is not a failure", sum.Errors)
	}
}

// A registry that reads back zero rows deletes nothing. It is indistinguishable
// from a truncated Scan, and an empty registry cannot authorize anything anyway.
func TestReportUnmanagedSubdomains_EmptyRegistryDeletesNothing(t *testing.T) {
	r53 := &fakeRoute53{sets: zone()}
	rp := newExpiryReaper(r53, &fakeRegistry{}, true)

	var sum Summary
	rp.reportUnmanagedSubdomains(context.Background(), &sum, expiryNow)

	if len(r53.deleted) != 0 {
		t.Errorf("deleted %v against an empty registry", r53.deleted)
	}
}

// A zone-listing failure aborts before any verdict, so nothing is deleted and the
// error surfaces. Without this, a partial zone read could be acted on.
func TestReportUnmanagedSubdomains_ZoneListErrorDeletesNothing(t *testing.T) {
	r53 := &fakeRoute53{sets: zone(), listErr: errors.New("throttled")}
	reg := &fakeRegistry{rows: []accountlifecycle.Account{row(unmanagedID, accountlifecycle.StatusDormant)}}
	rp := newExpiryReaper(r53, reg, true)

	var sum Summary
	rp.reportUnmanagedSubdomains(context.Background(), &sum, expiryNow)

	if len(r53.deleted) != 0 {
		t.Errorf("deleted %v after a failed zone list", r53.deleted)
	}
	if sum.Errors != 1 {
		t.Errorf("errors = %d, want 1", sum.Errors)
	}
}

// captureLogs runs fn with the standard logger redirected, and returns what it wrote.
// In report and dry-run modes the log IS the deliverable — nothing else leaves the
// Lambda — so its wording is behaviour, not decoration.
func captureLogs(fn func()) string {
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	}()
	fn()
	return buf.String()
}

// Dry-run must say WOULD, never "deleted". deleteRecord is itself dry-run aware, so
// the reaper is safe either way — but a log claiming a deletion that never happened
// is how an operator concludes the feature works when it has not yet done anything,
// or that it destroyed records when it has not.
func TestReportUnmanagedSubdomains_DryRunLogSaysWouldNotDeleted(t *testing.T) {
	r53 := &fakeRoute53{sets: zone()}
	reg := &fakeRegistry{rows: []accountlifecycle.Account{row(unmanagedID, accountlifecycle.StatusDormant)}}
	rp := newExpiryReaper(r53, reg, true)
	rp.dryRun = true

	var sum Summary
	out := captureLogs(func() {
		rp.reportUnmanagedSubdomains(context.Background(), &sum, expiryNow)
	})

	if !strings.Contains(out, "WOULD delete A-record box1."+unmanagedSub) {
		t.Errorf("dry-run log must say WOULD delete; got:\n%s", out)
	}
	if strings.Contains(out, "DNS expiry: deleted") {
		t.Errorf("dry-run log claims a deletion that did not happen; got:\n%s", out)
	}
}

// An unusable registry must SAY so. Without this line an operator who enabled
// REAPER_DNS_EXPIRE sees ordinary #457 report output and concludes there is nothing
// eligible, when the truth is that no verdict was available at all.
func TestReportUnmanagedSubdomains_UnusableRegistryIsAnnounced(t *testing.T) {
	for _, tc := range []struct {
		name string
		reg  registryAPI
	}{
		{"empty registry", &fakeRegistry{}},
		{"unreadable registry", &fakeRegistry{err: errors.New("dynamodb unavailable")}},
		{"no registry configured", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r53 := &fakeRoute53{sets: zone()}
			rp := newExpiryReaper(r53, tc.reg, true)

			var sum Summary
			out := captureLogs(func() {
				rp.reportUnmanagedSubdomains(context.Background(), &sum, expiryNow)
			})

			if !strings.Contains(out, "no registry verdicts available") {
				t.Errorf("want the missing-verdicts advisory; got:\n%s", out)
			}
			if len(r53.deleted) != 0 {
				t.Errorf("deleted %v with no usable registry", r53.deleted)
			}
		})
	}
}
