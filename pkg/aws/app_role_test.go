package aws

import (
	"testing"

	"github.com/spore-host/spawn/pkg/dns"
	"github.com/spore-host/spawn/pkg/instancerole"
)

// legacyCertGrant is the certificate read every CLI up to schema 2 put on
// spored-instance-role, and one up to v0.161.0 still rewrites there.
const legacyCertGrant = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject"],"Resource":"arn:aws:s3:::spawn-certs-*/*"}]}`

func certKey(account string) string {
	return "arn:aws:s3:::spawn-certs-us-east-1/" + dns.EncodeAccountID(account) + "/key.pem"
}

// The account's wildcard TLS key is readable by the app role and by no default
// spore (instance-7): the shared role Denies it, so an older CLI's grant beside
// the new policy cannot bring it back.
func TestOnlyTheAppRoleReadsTheWildcardKey(t *testing.T) {
	c := &Client{}
	get := func(account string) scopeRequest {
		return instanceReq("s3:GetObject", certKey(account), nil, nil)
	}
	shared := bound(t, sporedDCVRolePolicy)
	for what, docs := range map[string][]string{
		"spored-instance-role":                           {shared},
		"spored-instance-role + a pre-#1558 CLI's grant": {shared, legacyCertGrant},
	} {
		if got := scopeEval(t, docs, get(testAccount)); got != explicitDeny {
			t.Errorf("%s reading the wildcard key: %v, want explicit deny", what, got)
		}
	}

	app, err := appRolePolicyDoc(testAccount)
	if err != nil {
		t.Fatal(err)
	}
	appRole := []string{bound(t, c.sporedBaselinePolicyDoc()), app}
	if got := scopeEval(t, appRole, get(testAccount)); got != allowed {
		t.Errorf("the app role reading its own account's key: %v, want allow", got)
	}
	if got := scopeEval(t, appRole, get("210987654321")); got == allowed {
		t.Error("the app role may read another account's key")
	}
	// The baseline the app role carries must not Deny what it grants.
	if got := scopeEval(t, []string{bound(t, c.sporedBaselinePolicyDoc())}, get(testAccount)); got == explicitDeny {
		t.Error("the spored baseline Denies the key read, so the app role, which carries it, cannot read its certificate")
	}

	if _, err := appRolePolicyDoc("not-an-account"); err == nil {
		t.Error("the app role's certificate read was scoped to a malformed account")
	}
	if !instancerole.IsCandidate(AppRoleName) {
		t.Errorf("%s is outside pkg/instancerole's sweep, so nothing deletes it", AppRoleName)
	}
}
