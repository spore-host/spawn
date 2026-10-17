package bot

import (
	"bytes"
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
)

// fakeBotIAM is an in-memory SpawnBotCrossAccount role.
type fakeBotIAM struct {
	exists  bool
	trust   string // trust policy as IAM returns it (URL-encoded)
	tags    []iamtypes.Tag
	created *iam.CreateRoleInput
	updated *iam.UpdateAssumeRolePolicyInput
	putPol  *iam.PutRolePolicyInput
}

const fakeRoleARN = "arn:aws:iam::111122223333:role/" + CrossAccountRoleName

func (f *fakeBotIAM) GetRole(_ context.Context, in *iam.GetRoleInput, _ ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
	if !f.exists {
		return nil, &iamtypes.NoSuchEntityException{Message: aws.String("no role")}
	}
	return &iam.GetRoleOutput{Role: &iamtypes.Role{
		RoleName: in.RoleName, Arn: aws.String(fakeRoleARN),
		AssumeRolePolicyDocument: aws.String(f.trust), Tags: f.tags,
	}}, nil
}

func (f *fakeBotIAM) CreateRole(_ context.Context, in *iam.CreateRoleInput, _ ...func(*iam.Options)) (*iam.CreateRoleOutput, error) {
	f.created = in
	return &iam.CreateRoleOutput{Role: &iamtypes.Role{Arn: aws.String(fakeRoleARN)}}, nil
}

func (f *fakeBotIAM) PutRolePolicy(_ context.Context, in *iam.PutRolePolicyInput, _ ...func(*iam.Options)) (*iam.PutRolePolicyOutput, error) {
	f.putPol = in
	return &iam.PutRolePolicyOutput{}, nil
}

func (f *fakeBotIAM) UpdateAssumeRolePolicy(_ context.Context, in *iam.UpdateAssumeRolePolicyInput, _ ...func(*iam.Options)) (*iam.UpdateAssumeRolePolicyOutput, error) {
	f.updated = in
	return &iam.UpdateAssumeRolePolicyOutput{}, nil
}

const (
	testPrincipal = "arn:aws:iam::966362334030:role/current-principal"
	oldPrincipal  = "arn:aws:iam::966362334030:role/old-principal-U2vZFZXgWBeM"
)

func encodedTrust(t *testing.T, principal, extID string) string {
	t.Helper()
	doc, err := botTrustPolicy(principal, extID)
	if err != nil {
		t.Fatal(err)
	}
	return iamEncode(doc)
}

// iamEncode URL-encodes a policy the way IAM's GetRole returns it (RFC 3986:
// a space is %20, not +).
func iamEncode(doc string) string { return strings.ReplaceAll(url.QueryEscape(doc), "+", "%20") }

var spawnCreatedTags = []iamtypes.Tag{{Key: aws.String(botPurposeTagKey), Value: aws.String(botPurposeTagValue)}}

func fixedID(id string) func() (string, error) { return func() (string, error) { return id, nil } }

// mustTrust parses doc and asserts it grants exactly principal under extID.
func mustTrust(t *testing.T, doc, principal, extID string) {
	t.Helper()
	tr, err := parseBotTrust(doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.principals) != 1 || tr.principals[0] != principal {
		t.Errorf("trust principals = %v, want [%s]", tr.principals, principal)
	}
	if len(tr.externalIDs) != 1 || tr.externalIDs[0] != extID {
		t.Errorf("trust ExternalIds = %v, want [%s]", tr.externalIDs, extID)
	}
}

func TestEnsureCrossAccountRole_CreatesWithPrincipalAndPerAccountExternalID(t *testing.T) {
	f := &fakeBotIAM{}
	arn, xid, err := EnsureCrossAccountRole(context.Background(), f, testPrincipal, false, fixedID("fresh"), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if arn != fakeRoleARN || xid != "fresh" {
		t.Errorf("got (%s, %s), want (%s, fresh)", arn, xid, fakeRoleARN)
	}
	if f.created == nil {
		t.Fatal("role not created")
	}
	mustTrust(t, aws.ToString(f.created.AssumeRolePolicyDocument), testPrincipal, "fresh")
	if !hasTag(f.created.Tags, botPurposeTagKey, botPurposeTagValue) {
		t.Errorf("created role lacks %s=%s, so --repair could never fix it later", botPurposeTagKey, botPurposeTagValue)
	}
	if f.putPol == nil {
		t.Error("permission policy not attached")
	}
}

func TestEnsureCrossAccountRole_ReusesAHealthyRoleAndItsExternalID(t *testing.T) {
	f := &fakeBotIAM{exists: true, trust: encodedTrust(t, testPrincipal, "kept"), tags: spawnCreatedTags}
	for _, repair := range []bool{false, true} {
		_, xid, err := EnsureCrossAccountRole(context.Background(), f, testPrincipal, repair, fixedID("unused"), &bytes.Buffer{})
		if err != nil {
			t.Fatalf("repair=%v: %v", repair, err)
		}
		if xid != "kept" {
			t.Errorf("repair=%v: ExternalId %q, want the role's own %q", repair, xid, "kept")
		}
	}
	if f.updated != nil || f.created != nil {
		t.Error("a healthy role was modified")
	}
}

func TestEnsureCrossAccountRole_StaleRole(t *testing.T) {
	tests := []struct {
		name       string
		principal  string
		extID      string
		tags       []iamtypes.Tag
		repair     bool
		wantErr    string // substring; "" = success
		wantExtID  string
		wantUpdate bool
	}{
		{name: "old principal, no repair", principal: oldPrincipal, extID: "spawn-bot", tags: spawnCreatedTags,
			wantErr: "--repair"},
		{name: "old principal, repair, shared id replaced", principal: oldPrincipal, extID: "spawn-bot", tags: spawnCreatedTags,
			repair: true, wantExtID: "fresh", wantUpdate: true},
		{name: "old principal, repair, per-account id kept", principal: oldPrincipal, extID: "mine", tags: spawnCreatedTags,
			repair: true, wantExtID: "mine", wantUpdate: true},
		{name: "right principal, shared id only, repair", principal: testPrincipal, extID: "spawn-bot", tags: spawnCreatedTags,
			repair: true, wantExtID: "fresh", wantUpdate: true},
		{name: "right principal, shared id only, no repair", principal: testPrincipal, extID: "prism-bot", tags: spawnCreatedTags,
			wantErr: "no per-account ExternalId"},
		{name: "not created by spawn, no repair", principal: oldPrincipal, extID: "x",
			wantErr: "not created by spawn"},
		{name: "not created by spawn, repair refused", principal: oldPrincipal, extID: "x",
			repair: true, wantErr: "refuses to modify"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeBotIAM{exists: true, trust: encodedTrust(t, tt.principal, tt.extID), tags: tt.tags}
			_, xid, err := EnsureCrossAccountRole(context.Background(), f, testPrincipal, tt.repair, fixedID("fresh"), &bytes.Buffer{})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tt.wantErr)
				}
				if f.updated != nil {
					t.Error("trust policy was rewritten on an error path")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if xid != tt.wantExtID {
				t.Errorf("ExternalId = %q, want %q", xid, tt.wantExtID)
			}
			if (f.updated != nil) != tt.wantUpdate {
				t.Fatalf("updated = %v, want %v", f.updated != nil, tt.wantUpdate)
			}
			mustTrust(t, aws.ToString(f.updated.PolicyDocument), testPrincipal, tt.wantExtID)
		})
	}
}

// The pre-#1046 trust document, as ensureCrossAccountRole used to write it
// (whitespace and all), must parse so --repair can recognise it.
func TestParseBotTrust_LegacyDocument(t *testing.T) {
	legacy := `{
		"Version": "2012-10-17",
		"Statement": [{
			"Effect": "Allow",
			"Principal": {"AWS": "` + oldPrincipal + `"},
			"Action": "sts:AssumeRole",
			"Condition": {"StringEquals": {"sts:ExternalId": "spawn-bot"}}
		}]
	}`
	for _, doc := range []string{legacy, iamEncode(legacy)} {
		tr, err := parseBotTrust(doc)
		if err != nil {
			t.Fatal(err)
		}
		if !tr.trusts(oldPrincipal) || tr.trusts(testPrincipal) {
			t.Errorf("principals = %v", tr.principals)
		}
		if tr.perAccountExternalID() != "" {
			t.Errorf("shared spawn-bot ExternalId counted as per-account: %v", tr.externalIDs)
		}
	}
}
