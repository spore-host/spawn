package main

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spore-host/spawn/internal/treewalk"

	"gopkg.in/yaml.v3"
)

// The principal a SpawnBotCrossAccount role trusts is stated in two places:
// SPORE_BOT_LAMBDA_ROLE_ARN (what `spawn notify register` writes into the
// trust policy) and the BotLambdaRoleArn parameter of
// deployment/cloudformation/bot-cross-account-role.yaml. Before #1046 the repo
// named three different roles for it; after it, both defaulted to the hosted
// spore-bot's role. spore.host no longer hosts a bot (spore-host#611), so
// neither has a default any more: the operator names their own self-hosted
// bot. These tests fail if a default principal, or any reference to the
// retired hosted bot's role, comes back.

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

func TestBotCrossAccountTemplateHasNoDefaultPrincipal(t *testing.T) {
	cfn := readCFNBotTrust(t)
	if cfn.principalDefault != "" {
		t.Errorf("%s: parameter %s defaults to %q. spore.host hosts no bot (spore-host#611): the principal must be the operator's own, with no default.",
			botTemplate, cfn.principalParam, cfn.principalDefault)
	}
}

// TestBotCrossAccountTemplateHasNoSharedExternalID: the role's ExternalId is
// the operator's own, with no default and no fallback to the legacy shared
// "<prefix>-bot" value, which spore-bot no longer presents (spore-host#413).
func TestBotCrossAccountTemplateHasNoSharedExternalID(t *testing.T) {
	b, err := os.ReadFile(botTemplate)
	if err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	p := mapGet(mapGet(doc.Content[0], "Parameters"), "ExternalId")
	if p == nil {
		t.Fatalf("%s has no ExternalId parameter; the template's shape changed", botTemplate)
	}
	if d := mapGet(p, "Default"); d != nil {
		t.Errorf("%s: ExternalId has a Default (%q); it must be the operator's own value", botTemplate, d.Value)
	}
	if regexp.MustCompile(`(?m)^[^#]*\$\{TagPrefix\}-bot`).Match(b) {
		t.Errorf("%s still writes the shared ${TagPrefix}-bot ExternalId somewhere", botTemplate)
	}
}

// iamRoleARN matches an IAM role ARN with a literal, wildcard or CloudFormation
// account. The role-name class is IAM's own.
var iamRoleARN = regexp.MustCompile(`arn:aws:iam::(?:\d{12}|\*|\$\{AWS::AccountId\}):role/([\w+=,.@-]+)`)

// retiredHostedBotAccount is the spore.host infra account the hosted bot ran
// in. No self-hosted bot runs there, so a bot role ARN in it is the retired
// hosted principal (spore-bot-role, or the prism-bot stack's role).
const retiredHostedBotAccount = "966362334030"

// Every IAM role ARN anywhere in the repo that names a bot role in the
// retired hosted account would point a trust at a bot that no longer exists.
// The trustor, the customer-side role the bot assumes, is not a principal; its
// name is read from the template, not listed here.
func TestNoFileNamesTheRetiredHostedBotPrincipal(t *testing.T) {
	trustor := readCFNBotTrust(t).trustorRoleName

	skipDir := map[string]bool{".git": true, "bin": true, "node_modules": true, "vendor": true, "dist": true}
	var bad []string
	scanned := 0
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if treewalk.Vanished(".", path, err) {
				return nil
			}
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
				if strings.Contains(m[0], "::"+retiredHostedBotAccount+":") {
					bad = append(bad, path+": "+m[0])
				}
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
	for _, b := range bad {
		t.Errorf("%s names spore.host's retired hosted bot principal; the principal is the operator's own (SPORE_BOT_LAMBDA_ROLE_ARN)", b)
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
