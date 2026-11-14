package main

import (
	"bytes"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const invDir = "../../infra/iam/github-oidc"

func committed(t *testing.T) (Inventory, SigningKey) {
	t.Helper()
	inv, sk, err := load(invDir)
	if err != nil {
		t.Fatal(err)
	}
	return inv, sk
}

func TestCommittedInventoryPassesCheck(t *testing.T) {
	inv, sk := committed(t)
	if ps := Check(inv, sk); len(ps) > 0 {
		t.Fatalf("committed inventory: %v", ps)
	}
	var out bytes.Buffer
	if err := run([]string{"check", "--dir", invDir}, &out); err != nil {
		t.Fatalf("check: %v\n%s", err, out.String())
	}
}

const releaseRole = "GitHubActions-spore-host-release"

func withTrust(t *testing.T, inv Inventory, role, trust string) Inventory {
	t.Helper()
	out := Inventory{}
	for _, r := range inv.Roles {
		if r.Role == role {
			r.Trust = json.RawMessage(trust)
		}
		out.Roles = append(out.Roles, r)
	}
	return out
}

func trustDoc(cond string) string {
	return `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
		`"Principal":{"Federated":"arn:aws:iam::966362334030:oidc-provider/token.actions.githubusercontent.com"},` +
		`"Action":"sts:AssumeRoleWithWebIdentity","Condition":` + cond + `}]}`
}

// Each trust a reviewer might be tempted to write, and why it is refused.
func TestCheckRefusesBroadTrusts(t *testing.T) {
	inv, sk := committed(t)
	cases := map[string]string{
		"any ref of the repo": `{"StringEquals":{"token.actions.githubusercontent.com:aud":"sts.amazonaws.com"},"StringLike":{"token.actions.githubusercontent.com:sub":"repo:spore-host/spawn:*"}}`,
		"any branch":          `{"StringEquals":{"token.actions.githubusercontent.com:aud":"sts.amazonaws.com"},"StringLike":{"token.actions.githubusercontent.com:sub":"repo:spore-host/spawn:ref:refs/heads/*"}}`,
		"any repo in the org": `{"StringEquals":{"token.actions.githubusercontent.com:aud":"sts.amazonaws.com"},"StringLike":{"token.actions.githubusercontent.com:sub":"repo:spore-host/*:ref:refs/tags/v*"}}`,
		"aud only":            `{"StringEquals":{"token.actions.githubusercontent.com:aud":"sts.amazonaws.com"}}`,
		"no aud":              `{"StringLike":{"token.actions.githubusercontent.com:sub":"repo:spore-host/spawn:ref:refs/tags/v*"}}`,
		"pattern under StringEquals": `{"StringEquals":{"token.actions.githubusercontent.com:aud":"sts.amazonaws.com",` +
			`"token.actions.githubusercontent.com:sub":"repo:spore-host/spawn:pull_request"}}`,
	}
	for name, cond := range cases {
		t.Run(name, func(t *testing.T) {
			if ps := Check(withTrust(t, inv, releaseRole, trustDoc(cond)), sk); len(ps) == 0 {
				t.Errorf("Check passed a trust with %s", name)
			}
		})
	}
	t.Run("another account's provider", func(t *testing.T) {
		doc := strings.Replace(trustDoc(`{"StringEquals":{"token.actions.githubusercontent.com:aud":"sts.amazonaws.com"},"StringLike":{"token.actions.githubusercontent.com:sub":"repo:spore-host/spawn:ref:refs/tags/v*"}}`),
			"966362334030", "435415984226", 1)
		if ps := Check(withTrust(t, inv, releaseRole, doc), sk); len(ps) == 0 {
			t.Error("Check passed a trust whose OIDC provider is in another account")
		}
	})
	t.Run("the forbidden account", func(t *testing.T) {
		doc := strings.Replace(trustDoc(`{"StringEquals":{"token.actions.githubusercontent.com:aud":"sts.amazonaws.com"},"StringLike":{"token.actions.githubusercontent.com:sub":"repo:spore-host/spawn:ref:refs/tags/v*"}}`),
			"966362334030", forbiddenAccount, 1)
		if ps := Check(withTrust(t, inv, releaseRole, doc), sk); len(ps) == 0 {
			t.Error("Check passed a trust naming " + forbiddenAccount)
		}
	})
}

func TestCheckRefusesASignerOrDenyThatDisagrees(t *testing.T) {
	inv, sk := committed(t)
	other := sk
	other.Signers = []string{"arn:aws:iam::966362334030:role/SomeoneElse"}
	if ps := Check(inv, other); len(ps) == 0 {
		t.Error("Check passed a signer that is not an inventoried role")
	}
	loose := sk
	loose.RequiredDeny = json.RawMessage(strings.Replace(string(sk.RequiredDeny), releaseRole, "GitHubActions-spawn-lambda-deploy", 1))
	if ps := Check(inv, loose); len(ps) == 0 {
		t.Error("Check passed a required Deny that exempts a role other than the signers")
	}
	allow := sk
	allow.RequiredDeny = json.RawMessage(strings.Replace(string(sk.RequiredDeny), `"Deny"`, `"Allow"`, 1))
	if ps := Check(inv, allow); len(ps) == 0 {
		t.Error("Check passed a required_deny that is an Allow")
	}
}

func getRole(t *testing.T, inv Inventory, name string, trust string) []byte {
	t.Helper()
	for _, r := range inv.Roles {
		if r.Role == name {
			if trust == "" {
				trust = string(r.Trust)
			}
			b, err := json.Marshal(map[string]any{"Role": map[string]any{
				"RoleName": name, "Arn": r.RoleARN, "AssumeRolePolicyDocument": json.RawMessage(trust),
			}})
			if err != nil {
				t.Fatal(err)
			}
			return b
		}
	}
	t.Fatalf("%s not in the inventory", name)
	return nil
}

func TestCompareRole(t *testing.T) {
	inv, _ := committed(t)
	for _, r := range inv.Roles {
		if ps, err := CompareRole(inv, r.Role, getRole(t, inv, r.Role, "")); err != nil || len(ps) > 0 {
			t.Errorf("%s: the expected trust as live: %v %v", r.Role, err, ps)
		}
	}
	// The raw API's URL-encoded string form, with a Sid and the sub as a list.
	enc := url.QueryEscape(strings.Replace(trustDoc(`{"StringEquals":{"token.actions.githubusercontent.com:aud":["sts.amazonaws.com"]},"StringLike":{"token.actions.githubusercontent.com:sub":["repo:spore-host/spawn:ref:refs/tags/v*"]}}`),
		`{"Effect"`, `{"Sid":"x","Effect"`, 1))
	b, _ := json.Marshal(map[string]any{"Role": map[string]any{"Arn": "arn:aws:iam::966362334030:role/" + releaseRole, "AssumeRolePolicyDocument": enc}})
	if ps, err := CompareRole(inv, releaseRole, b); err != nil || len(ps) > 0 {
		t.Errorf("URL-encoded equivalent trust: %v %v", err, ps)
	}
	broad := trustDoc(`{"StringEquals":{"token.actions.githubusercontent.com:aud":"sts.amazonaws.com"},"StringLike":{"token.actions.githubusercontent.com:sub":"repo:spore-host/spawn:*"}}`)
	if ps, err := CompareRole(inv, releaseRole, getRole(t, inv, releaseRole, broad)); err != nil || len(ps) == 0 {
		t.Errorf("a live sub of repo:spore-host/spawn:* compared clean: %v", err)
	}
	// An extra statement next to the expected one is a difference too.
	var doc map[string]any
	_ = json.Unmarshal(inv.Roles[0].Trust, &doc)
	st := doc["Statement"].([]any)
	doc["Statement"] = append(st, map[string]any{"Effect": "Allow", "Principal": map[string]any{"AWS": "arn:aws:iam::966362334030:root"}, "Action": "sts:AssumeRole"})
	extra, _ := json.Marshal(doc)
	if ps, err := CompareRole(inv, inv.Roles[0].Role, getRole(t, inv, inv.Roles[0].Role, string(extra))); err != nil || len(ps) == 0 {
		t.Errorf("an extra live statement compared clean: %v", err)
	}
}

const defaultKeyPolicy = `{"Version":"2012-10-17","Statement":[
 {"Sid":"Enable IAM User Permissions","Effect":"Allow","Principal":{"AWS":"arn:aws:iam::966362334030:root"},"Action":"kms:*","Resource":"*"}`

func TestCompareKeyPolicy(t *testing.T) {
	_, sk := committed(t)
	deny := string(sk.RequiredDeny)
	release := "arn:aws:iam::966362334030:role/" + releaseRole
	cases := []struct {
		name   string
		policy string
		ok     bool
	}{
		{"the default policy: IAM delegation, no Deny", defaultKeyPolicy + `]}`, false},
		{"IAM delegation with the required Deny", defaultKeyPolicy + `,` + deny + `]}`, true},
		{"only the release role", `{"Statement":[{"Effect":"Allow","Principal":{"AWS":"` + release + `"},"Action":["kms:Sign","kms:GetPublicKey"],"Resource":"*"}]}`, true},
		{"another role may sign", `{"Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::966362334030:role/Admin"},"Action":"kms:S*","Resource":"*"}]}`, false},
		{"anyone may sign", `{"Statement":[{"Effect":"Allow","Principal":"*","Action":"kms:Sign","Resource":"*"}]}`, false},
		{"NotAction that leaves Sign in", `{"Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::966362334030:role/Admin"},"NotAction":"kms:Decrypt","Resource":"*"}]}`, false},
		{"a Deny that exempts another role", defaultKeyPolicy + `,` + strings.Replace(deny, releaseRole, "Admin", 1) + `]}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ps, err := CompareKeyPolicy(sk, []byte(c.policy))
			if err != nil {
				t.Fatal(err)
			}
			if (len(ps) == 0) != c.ok {
				t.Errorf("ok = %v, want %v: %v", len(ps) == 0, c.ok, ps)
			}
		})
	}
	// `aws kms get-key-policy` without --output text wraps it in {"Policy": "<json>"}.
	wrapped, _ := json.Marshal(map[string]string{"Policy": defaultKeyPolicy + `]}`, "PolicyName": "default"})
	if ps, err := CompareKeyPolicy(sk, wrapped); err != nil || len(ps) == 0 {
		t.Errorf("the wrapped default policy compared clean: %v", err)
	}
}

func TestCheckTagRulesets(t *testing.T) {
	good := `{"name":"release tags","target":"tag","enforcement":"active",
	  "conditions":{"ref_name":{"include":["refs/tags/v*"],"exclude":[]}},
	  "rules":[{"type":"creation"},{"type":"update"},{"type":"deletion"}],
	  "bypass_actors":[{"actor_type":"RepositoryRole","actor_id":5,"bypass_mode":"always"}]}`
	cases := map[string]struct {
		doc string
		ok  bool
	}{
		"none (spawn today)":   {`[]`, false},
		"a protecting ruleset": {`[` + good + `]`, true},
		"evaluate mode only":   {`[` + strings.Replace(good, `"active"`, `"evaluate"`, 1) + `]`, false},
		"no creation rule":     {`[` + strings.Replace(good, `{"type":"creation"},`, ``, 1) + `]`, false},
		"a branch ruleset":     {`[` + strings.Replace(good, `"target":"tag"`, `"target":"branch"`, 1) + `]`, false},
		"other tags only":      {`[` + strings.Replace(good, `refs/tags/v*`, `refs/tags/lambda-*`, 1) + `]`, false},
	}
	for name, c := range cases {
		ps, err := CheckTagRulesets([]byte(c.doc))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if (len(ps) == 0) != c.ok {
			t.Errorf("%s: ok = %v, want %v: %v", name, len(ps) == 0, c.ok, ps)
		}
	}
}

func TestLiveEndToEnd(t *testing.T) {
	inv, _ := committed(t)
	dir := t.TempDir()
	f := filepath.Join(dir, "release.json")
	if err := os.WriteFile(f, getRole(t, inv, releaseRole, ""), 0o600); err != nil {
		t.Fatal(err)
	}
	kp := filepath.Join(dir, "kp.json")
	if err := os.WriteFile(kp, []byte(defaultKeyPolicy+`]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"live", "--dir", invDir, "--role", releaseRole + "=" + f}, &out); err != nil {
		t.Errorf("a matching live trust failed: %v\n%s", err, out.String())
	}
	out.Reset()
	if err := run([]string{"live", "--dir", invDir, "--role", releaseRole + "=" + f, "--key-policy", kp}, &out); err == nil {
		t.Errorf("the default key policy passed live:\n%s", out.String())
	}
	if err := run([]string{"live", "--dir", invDir}, &out); err == nil {
		t.Error("live with nothing to compare passed")
	}
}
