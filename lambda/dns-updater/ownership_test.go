package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/route53/types"
)

func TestCallerInstance(t *testing.T) {
	const acct = "123456789012"
	cases := map[string]string{
		"arn:aws:sts::123456789012:assumed-role/spored-instance-role/i-0123456789abcdef0":        "i-0123456789abcdef0",
		"arn:aws:sts::123456789012:assumed-role/spawn-instance-ab12cd34/i-0abc12345":             "i-0abc12345",
		"arn:aws:sts::123456789012:assumed-role/spawn-instance-nested-1a2b3c4d/i-0abc12345":      "i-0abc12345",
		"arn:aws-us-gov:sts::123456789012:assumed-role/spored-instance-role/i-0abc12345":         "i-0abc12345",
		"arn:aws:sts::123456789012:assumed-role/AdminRole/i-0abc12345":                           "", // a role anyone may assume under any session name
		"arn:aws:sts::999999999999:assumed-role/spored-instance-role/i-0abc12345":                "", // another account
		"arn:aws:sts::123456789012:assumed-role/spored-instance-role/alice@example.com":          "",
		"arn:aws:iam::123456789012:user/alice":                                                   "",
		"":                                                                                       "",
		"arn:aws:sts::123456789012:assumed-role/spored-instance-role/i-0abc12345/extra":          "",
		"arn:aws:sts::123456789012:assumed-role/spored-instance-role/i-0ABC12345":                "",
		"arn:aws:sts::123456789012:assumed-role/spored-instance-role-not-really/i-0abc12345":     "",
		"arn:aws:sts::123456789012:assumed-role/xspawn-instance-ab12/i-0abc12345":                "",
		"arn:aws:sts::123456789012:federated-user/spored-instance-role/i-0abc12345":              "",
		"arn:aws:sts::123456789012:assumed-role/spored-instance-role/i-0abc12345\n":              "i-0abc12345", // trimmed
		"arn:aws:sts::123456789012:assumed-role/spawn-instance-x/i-0abc12345 ":                   "i-0abc12345",
		"arn:aws:sts::123456789012:assumed-role/spored-instance-role/i-":                         "",
		"arn:aws:sts::123456789012:assumed-role/spored-instance-role/i-0123456789abcdef01234567": "",
	}
	for arn, want := range cases {
		if got := callerInstance(arn, acct); got != want {
			t.Errorf("callerInstance(%q) = %q, want %q", arn, got, want)
		}
	}
}

func markerFor(instance string) *types.ResourceRecordSet { return markerSet(ownFQDN, instance) }

const ownFQDN = "web.abc123.spore.host"

// The rules in ownership.go, as a table. Every refusal here is the scenario the
// file exists for: another instance (or a principal that is not one) repointing
// or deleting a record it does not own.
func TestAuthorizeOwnership(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	fresh, stale := now.Add(-time.Hour), now.Add(-7*time.Hour)
	const me, other, myIP, theirIP = "i-0aaaaaaaa", "i-0bbbbbbbb", "192.0.2.1", "198.51.100.7"
	cases := []struct {
		name     string
		action   string
		instance string
		source   string
		st       recordState
		allow    bool
		marker   []types.ChangeAction // the marker changes, in order
	}{
		{"claim a free name", "UPSERT", me, myIP, recordState{}, true, []types.ChangeAction{types.ChangeActionCreate}},
		{"renew my record", "UPSERT", me, myIP, recordState{aValue: myIP, marker: markerFor(me), owner: me, leaseTime: fresh}, true, []types.ChangeAction{types.ChangeActionUpsert}},
		{"move my record to a new IP", "UPSERT", me, "203.0.113.9", recordState{aValue: myIP, marker: markerFor(me), owner: me}, true, []types.ChangeAction{types.ChangeActionUpsert}},
		{"repoint another instance's record", "UPSERT", me, myIP, recordState{aValue: theirIP, marker: markerFor(other), owner: other, leaseTime: fresh}, false, nil},
		{"repoint a non-renewing owner's record", "UPSERT", me, myIP, recordState{aValue: theirIP, marker: markerFor(other), owner: other}, false, nil},
		{"take over an abandoned record", "UPSERT", me, myIP, recordState{aValue: theirIP, marker: markerFor(other), owner: other, leaseTime: stale}, true, []types.ChangeAction{types.ChangeActionDelete, types.ChangeActionCreate}},
		{"claim over an orphaned marker", "UPSERT", me, myIP, recordState{marker: markerFor(other), owner: other}, true, []types.ChangeAction{types.ChangeActionDelete, types.ChangeActionCreate}},
		{"claim a legacy record from its address", "UPSERT", me, theirIP, recordState{aValue: theirIP}, true, []types.ChangeAction{types.ChangeActionCreate}},
		{"claim a legacy record from elsewhere", "UPSERT", me, myIP, recordState{aValue: theirIP}, false, nil},
		{"claim a legacy record with no source address", "UPSERT", me, "", recordState{aValue: theirIP}, false, nil},
		{"claim an abandoned legacy record", "UPSERT", me, myIP, recordState{aValue: theirIP, leaseTime: stale}, true, []types.ChangeAction{types.ChangeActionCreate}},
		{"delete my record", "DELETE", me, myIP, recordState{aValue: myIP, marker: markerFor(me), owner: me}, true, []types.ChangeAction{types.ChangeActionDelete}},
		{"delete another instance's record", "DELETE", me, myIP, recordState{aValue: theirIP, marker: markerFor(other), owner: other}, false, nil},
		{"delete another instance's abandoned record", "DELETE", me, myIP, recordState{aValue: theirIP, marker: markerFor(other), owner: other, leaseTime: stale}, false, nil},
		{"delete a legacy record from its address", "DELETE", me, theirIP, recordState{aValue: theirIP}, true, nil},
		{"delete a legacy record from elsewhere", "DELETE", me, myIP, recordState{aValue: theirIP}, false, nil},
		{"operator writes a free name", "UPSERT", "", myIP, recordState{}, true, nil},
		{"operator writes a legacy record", "UPSERT", "", myIP, recordState{aValue: theirIP}, true, nil},
		{"operator repoints an instance's record", "UPSERT", "", myIP, recordState{aValue: theirIP, marker: markerFor(other), owner: other}, false, nil},
		{"operator deletes an instance's record", "DELETE", "", myIP, recordState{aValue: theirIP, marker: markerFor(other), owner: other}, false, nil},
	}
	for _, c := range cases {
		d := authorizeOwnership(c.action, ownFQDN, c.instance, c.source, c.st, now)
		if d.allow != c.allow {
			t.Errorf("%s: allow=%v, want %v (%s)", c.name, d.allow, c.allow, d.reason)
			continue
		}
		if !d.allow && d.reason == "" {
			t.Errorf("%s: refused with no reason to log", c.name)
		}
		var got []types.ChangeAction
		for _, ch := range d.markerChanges {
			got = append(got, ch.Action)
			if name := aws.ToString(ch.ResourceRecordSet.Name); name != ownerName(ownFQDN) {
				t.Errorf("%s: marker change on %s, want %s", c.name, name, ownerName(ownFQDN))
			}
		}
		if len(got) != len(c.marker) {
			t.Errorf("%s: marker changes %v, want %v", c.name, got, c.marker)
			continue
		}
		for i := range got {
			if got[i] != c.marker[i] {
				t.Errorf("%s: marker changes %v, want %v", c.name, got, c.marker)
			}
		}
		for _, ch := range d.markerChanges {
			if ch.Action == types.ChangeActionCreate || ch.Action == types.ChangeActionUpsert {
				if v := aws.ToString(ch.ResourceRecordSet.ResourceRecords[0].Value); v != `"`+c.instance+`"` {
					t.Errorf("%s: the marker names %s, want the caller %s", c.name, v, c.instance)
				}
			}
		}
	}
}

// Through the real handler and the Route53 emulator: what ends up in the zone.
func TestHandlerRefusesAnotherInstancesRecord(t *testing.T) {
	zoneID := leaseTestZone(t)
	prevZones, prevDefault := domainZones, defaultDomain
	t.Cleanup(func() { domainZones, defaultDomain = prevZones, prevDefault })
	domainZones = map[string]string{"spore.host": zoneID}
	defaultDomain = "spore.host"
	const acct = "435415984226"
	stubRegistry(t, []string{acct}, nil)
	fqdn := getFullDNSName("victim-app", acct, "spore.host")

	call := func(mode, userARN, sourceIP, action, ip string) int {
		t.Helper()
		t.Setenv("DNS_AUTHZ_MODE", mode)
		body, _ := json.Marshal(DNSUpdateRequest{RecordName: "victim-app", IPAddress: ip, Action: action, Lease: true})
		resp, err := handler(context.Background(), events.LambdaFunctionURLRequest{
			Body: string(body),
			RequestContext: events.LambdaFunctionURLRequestContext{
				HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{SourceIP: sourceIP},
				Authorizer: &events.LambdaFunctionURLRequestContextAuthorizerDescription{
					IAM: &events.LambdaFunctionURLRequestContextAuthorizerIAMDescription{AccountID: acct, UserARN: userARN},
				},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode
	}
	session := func(id string) string {
		return "arn:aws:sts::" + acct + ":assumed-role/spored-instance-role/" + id
	}
	aValue := func() string {
		rs, err := lookupRecordSet(context.Background(), zoneID, fqdn, types.RRTypeA)
		if err != nil {
			t.Fatal(err)
		}
		if rs == nil {
			return ""
		}
		return aws.ToString(rs.ResourceRecords[0].Value)
	}
	owner := func() string {
		st, err := readRecordState(context.Background(), zoneID, fqdn)
		if err != nil {
			t.Fatal(err)
		}
		return st.owner
	}

	if got := call("", session("i-0aaaa1111"), "192.0.2.10", "UPSERT", "192.0.2.10"); got != 200 {
		t.Fatalf("the victim's own registration: status %d", got)
	}
	if owner() != "i-0aaaa1111" {
		t.Fatalf("the victim's registration recorded owner %q", owner())
	}
	if got := call("", session("i-0bbbb2222"), "198.51.100.7", "UPSERT", "198.51.100.7"); got != 403 {
		t.Errorf("another instance repointing the record: status %d, want 403", got)
	}
	if got := call("", session("i-0bbbb2222"), "198.51.100.7", "DELETE", "192.0.2.10"); got != 403 {
		t.Errorf("another instance deleting the record: status %d, want 403", got)
	}
	if got := call("", "arn:aws:iam::"+acct+":user/mallory", "198.51.100.7", "UPSERT", "198.51.100.7"); got != 403 {
		t.Errorf("a principal that is not an instance repointing the record: status %d, want 403", got)
	}
	if v := aValue(); v != "192.0.2.10" {
		t.Errorf("after refused requests the record points at %q, want the victim's 192.0.2.10", v)
	}
	if got := call("", session("i-0aaaa1111"), "192.0.2.11", "UPSERT", "192.0.2.11"); got != 200 || aValue() != "192.0.2.11" {
		t.Errorf("the owner moving its record: status %d, record %q", got, aValue())
	}

	// Audit lets a refused request through, and the marker follows the record.
	if got := call(authzModeAudit, session("i-0bbbb2222"), "198.51.100.7", "UPSERT", "198.51.100.7"); got != 200 {
		t.Errorf("audit mode refused: status %d", got)
	}
	if owner() != "i-0bbbb2222" {
		t.Errorf("after an audit-mode takeover the marker names %q, want the new writer", owner())
	}

	// The owner's delete takes the marker with it.
	if got := call("", session("i-0bbbb2222"), "198.51.100.7", "DELETE", "198.51.100.7"); got != 200 {
		t.Fatalf("the owner's delete: status %d", got)
	}
	if aValue() != "" || owner() != "" {
		t.Errorf("after the owner's delete: record %q, marker %q; want both gone", aValue(), owner())
	}
}
