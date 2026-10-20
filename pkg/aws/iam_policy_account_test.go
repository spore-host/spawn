package aws

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/spore-host/spawn/pkg/testutil"
)

// #1069: the spored role policies carried ${aws:PrincipalAccount} in a Resource,
// which PutRolePolicy rejects ("MalformedPolicyDocument: The policy failed legacy
// parsing") — so every launch that set up the role failed on v0.129.0/v0.130.0.
// Substrate accepts the document, and so do IAM Access Analyzer and
// simulate-custom-policy, so the gate is on what is WRITTEN: run every writer
// against Substrate and read back what reached IAM.

func storedPolicy(t *testing.T, ic *iam.Client, role, name string) string {
	t.Helper()
	out, err := ic.GetRolePolicy(context.Background(), &iam.GetRolePolicyInput{RoleName: aws.String(role), PolicyName: aws.String(name)})
	if err != nil {
		t.Fatalf("GetRolePolicy %s/%s: %v", role, name, err)
	}
	doc := aws.ToString(out.PolicyDocument)
	if d, err := url.QueryUnescape(doc); err == nil { // IAM returns it URL-encoded
		doc = d
	}
	return doc
}

func assertBound(t *testing.T, what, doc, account string) {
	t.Helper()
	if strings.Contains(doc, "${aws:PrincipalAccount}") {
		t.Errorf("%s reached IAM with ${aws:PrincipalAccount}, which PutRolePolicy rejects", what)
	}
	if strings.Contains(doc, sporedAccountPlaceholder) {
		t.Errorf("%s reached IAM with the unbound placeholder %s", what, sporedAccountPlaceholder)
	}
	if !strings.Contains(doc, "arn:aws:dynamodb:*:"+account+":table/spawn-hybrid-registry") {
		t.Errorf("%s does not grant the registry table in account %s:\n%s", what, account, doc)
	}
}

func TestEverySporedPolicyIsWrittenBoundToTheAccount(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	c := NewClientFromConfig(env.AWSConfig)
	ic := env.IAMClient()
	acct, _, err := c.GetCallerIdentityInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// No IAM flags: the shared spored role (every ordinary launch, and Provision).
	if _, err := c.SetupSporedIAMRole(ctx); err != nil {
		t.Fatalf("SetupSporedIAMRole: %v", err)
	}
	assertBound(t, "spored-policy (SetupSporedIAMRole)", storedPolicy(t, ic, SporedRoleName, "spored-policy"), acct)

	// --iam-policy: the inline policy AND the baseline on the generated role.
	if _, err := c.CreateOrGetInstanceProfile(ctx, IAMRoleConfig{RoleName: "spawn-test-1069", Policies: []string{"s3:ReadOnly"}}); err != nil {
		t.Fatalf("CreateOrGetInstanceProfile: %v", err)
	}
	assertBound(t, "spawn-inline-policy (createIAMRole)", storedPolicy(t, ic, "spawn-test-1069", "spawn-inline-policy"), acct)
	assertBound(t, "spored-baseline-policy", storedPolicy(t, ic, "spawn-test-1069", "spored-baseline-policy"), acct)

	// The same role again, with a different policy: the update path.
	if _, err := c.CreateOrGetInstanceProfile(ctx, IAMRoleConfig{RoleName: "spawn-test-1069", Policies: []string{"s3:ReadWrite"}}); err != nil {
		t.Fatalf("CreateOrGetInstanceProfile (update): %v", err)
	}
	assertBound(t, "spawn-inline-policy (updateInlinePolicy)", storedPolicy(t, ic, "spawn-test-1069", "spawn-inline-policy"), acct)
}

func TestBindPolicyAccount(t *testing.T) {
	tmpl := `{"Resource":"arn:aws:dynamodb:*:` + sporedAccountPlaceholder + `:table/x"}`
	got, err := bindPolicyAccount(tmpl, "123456789012")
	if err != nil || got != `{"Resource":"arn:aws:dynamodb:*:123456789012:table/x"}` {
		t.Errorf("bind = (%q, %v)", got, err)
	}
	for _, bad := range []string{"", "12345", "*", "12345678901a"} {
		if _, err := bindPolicyAccount(tmpl, bad); err == nil {
			t.Errorf("bound the policy to account %q", bad)
		}
	}
	if _, err := bindPolicyAccount(`{"Resource":"arn:aws:dynamodb:*:${aws:PrincipalAccount}:table/x"}`, "123456789012"); err == nil {
		t.Error("a document with ${aws:PrincipalAccount} was accepted for writing")
	}
	// A document with nothing to bind passes through unchanged.
	if got, err := bindPolicyAccount(`{"a":1}`, ""); err != nil || got != `{"a":1}` {
		t.Errorf("no-placeholder doc = (%q, %v)", got, err)
	}
}

// #1158: the custom-updater grant is written as its own policy, which a real
// PutRolePolicy accepts (#1069 is why that is checked by writing it), and which
// leaves the baseline's spore.host grant alone.
func TestEnsureDNSInvokeGrantIsWrittenBesideTheBaseline(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	c := NewClientFromConfig(env.AWSConfig)
	ic := env.IAMClient()
	if _, err := c.SetupSporedIAMRole(ctx); err != nil {
		t.Fatalf("SetupSporedIAMRole: %v", err)
	}
	const fn = "arn:aws:lambda:eu-west-1:111122223333:function:spawn-dns-updater"
	if err := c.EnsureDNSInvokeGrant(ctx, SporedRoleName, fn); err != nil {
		t.Fatalf("EnsureDNSInvokeGrant: %v", err)
	}
	if doc := storedPolicy(t, ic, SporedRoleName, DNSInvokePolicyName(fn)); !strings.Contains(doc, `"Resource":"`+fn+`"`) {
		t.Errorf("custom grant = %s, want Resource %s", doc, fn)
	}
	if doc := storedPolicy(t, ic, SporedRoleName, "spored-policy"); strings.Contains(doc, fn) {
		t.Errorf("the custom function reached the shared baseline, where the next launch would overwrite it:\n%s", doc)
	}
	if DNSInvokePolicyName(fn) == DNSInvokePolicyName("arn:aws:lambda:eu-west-1:444455556666:function:spawn-dns-updater") {
		t.Error("two updaters share a policy name, so one grant would replace the other")
	}
}
