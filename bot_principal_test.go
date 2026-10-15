package main

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	spawnconfig "github.com/spore-host/spawn/pkg/config"
)

// The principal a SpawnBotCrossAccount role trusts is stated in three places:
// pkg/config (what `spawn notify register` writes into the trust policy), the
// BotLambdaRoleArn parameter of deployment/cloudformation/bot-cross-account-role.yaml
// (what a customer deploying the template trusts), and whatever docs mention it.
// Before #1046 they named three different roles: the code a CloudFormation-
// generated prism-bot role that spore-bot no longer runs as, the template no
// default at all and an example role that never existed (spore-bot-lambda).
// These tests make pkg/config's DefaultBotLambdaRoleARN the one source and fail
// when anything else disagrees.

const botTemplate = "deployment/cloudformation/bot-cross-account-role.yaml"

// cfnBotTrust is what the bot cross-account template says, read from its
// structure rather than by name: the parameter its role's trust Principal.AWS
// is a !Ref to, that parameter's Default, and the default name of the role
// itself.
type cfnBotTrust struct {
	principalParam   string
	principalDefault string
	trustorRoleName  string
}

func readCFNBotTrust(t *testing.T) cfnBotTrust {
	t.Helper()
	b, err := os.ReadFile(botTemplate)
	if err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse %s: %v", botTemplate, err)
	}
	root := doc.Content[0]
	params, resources := mapGet(root, "Parameters"), mapGet(root, "Resources")
	if params == nil || resources == nil {
		t.Fatalf("%s has no Parameters/Resources", botTemplate)
	}
	refOf := func(n *yaml.Node) string {
		if n != nil && n.Tag == "!Ref" {
			return n.Value
		}
		return ""
	}
	paramDefault := func(name string) string {
		if d := mapGet(mapGet(params, name), "Default"); d != nil {
			return d.Value
		}
		return ""
	}
	var out cfnBotTrust
	roles := 0
	for i := 1; i < len(resources.Content); i += 2 {
		res := resources.Content[i]
		if ty := mapGet(res, "Type"); ty == nil || ty.Value != "AWS::IAM::Role" {
			continue
		}
		roles++
		props := mapGet(res, "Properties")
		out.trustorRoleName = paramDefault(refOf(mapGet(props, "RoleName")))
		stmts := mapGet(mapGet(props, "AssumeRolePolicyDocument"), "Statement")
		if stmts == nil {
			continue
		}
		for _, st := range stmts.Content {
			if p := refOf(mapGet(mapGet(st, "Principal"), "AWS")); p != "" {
				if out.principalParam != "" && out.principalParam != p {
					t.Fatalf("%s trusts two principal parameters (%s, %s); this test assumes one", botTemplate, out.principalParam, p)
				}
				out.principalParam = p
			}
		}
	}
	if roles != 1 || out.principalParam == "" || out.trustorRoleName == "" {
		t.Fatalf("%s: found %d IAM role(s), principal parameter %q, role name %q; the template's shape changed, so this test no longer checks it",
			botTemplate, roles, out.principalParam, out.trustorRoleName)
	}
	out.principalDefault = paramDefault(out.principalParam)
	return out
}

func mapGet(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

func TestBotCrossAccountTemplateTrustsTheDefaultBotPrincipal(t *testing.T) {
	cfn := readCFNBotTrust(t)
	if cfn.principalDefault != spawnconfig.DefaultBotLambdaRoleARN {
		t.Errorf("%s: parameter %s defaults to %q, but `spawn notify register` trusts %q (pkg/config DefaultBotLambdaRoleARN). Make them the same.",
			botTemplate, cfn.principalParam, cfn.principalDefault, spawnconfig.DefaultBotLambdaRoleARN)
	}
}

// iamRoleARN matches an IAM role ARN with a literal, wildcard or CloudFormation
// account. The role-name class is IAM's own.
var iamRoleARN = regexp.MustCompile(`arn:aws:iam::(?:\d{12}|\*|\$\{AWS::AccountId\}):role/([\w+=,.@-]+)`)

// Every IAM role ARN anywhere in the repo that names a bot role is a statement
// of what the bot runs as, and must be DefaultBotLambdaRoleARN. The one bot
// role that is not the principal is the trustor, the customer-side role the
// bot assumes; its name is read from the template, not listed here.
func TestEveryBotPrincipalARNInTheRepoIsTheDefault(t *testing.T) {
	trustor := readCFNBotTrust(t).trustorRoleName
	want := spawnconfig.DefaultBotLambdaRoleARN

	skipDir := map[string]bool{".git": true, "bin": true, "node_modules": true, "vendor": true, "dist": true}
	var bad []string
	agreeing, scanned := 0, 0
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDir[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		b, rerr := os.ReadFile(path) //nolint:gosec // walking the repo's own files
		if rerr != nil || bytes.IndexByte(b, 0) >= 0 {
			return nil // unreadable or binary
		}
		scanned++
		for _, line := range strings.Split(string(b), "\n") {
			for _, m := range iamRoleARN.FindAllStringSubmatch(line, -1) {
				name := m[1]
				if !strings.Contains(strings.ToLower(name), "bot") || name == trustor {
					continue
				}
				if m[0] == want {
					agreeing++
					continue
				}
				bad = append(bad, path+": "+m[0])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if scanned < 100 {
		t.Fatalf("scanned only %d files; the walk has broken rather than the repo having shrunk", scanned)
	}
	// The template's default is always one; finding none means the matcher broke.
	if agreeing == 0 {
		t.Fatalf("found no occurrence of %s in the repo, not even %s's default; the matcher is broken", want, botTemplate)
	}
	for _, b := range bad {
		t.Errorf("%s names a bot principal other than DefaultBotLambdaRoleARN (%s)", b, want)
	}
}

// The matcher itself, against the forms it has to catch and the ones it must
// leave alone. ARNs are assembled so this file is not one of its own matches.
func TestIAMRoleARNMatcher(t *testing.T) {
	arn := func(acct, name string) string { return "arn:aws:iam::" + acct + ":role/" + name }
	for _, tt := range []struct {
		in, name string
	}{
		{"Default: " + arn("966362334030", "prism-bot-PrismBotFunctionRole-U2vZFZXgWBeM"), "prism-bot-PrismBotFunctionRole-U2vZFZXgWBeM"},
		{`"` + arn("966362334030", "spore-bot-role") + `",`, "spore-bot-role"},
		{"Resource: '" + arn("*", "SpawnBotCrossAccount") + "'", "SpawnBotCrossAccount"},
		{"!Sub '" + arn("${AWS::AccountId}", "some-bot") + "'", "some-bot"},
		{"Example: " + arn("966362334030", "spore-bot-lambda"), "spore-bot-lambda"},
	} {
		m := iamRoleARN.FindStringSubmatch(tt.in)
		if m == nil || m[1] != tt.name {
			t.Errorf("match(%q) = %v, want role %q", tt.in, m, tt.name)
		}
	}
}
