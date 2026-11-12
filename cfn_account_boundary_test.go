package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/spore-host/spawn/internal/treewalk"
	"github.com/spore-host/spawn/pkg/trustboundary"
)

// TestCrossAccountTemplatesRefuseTheForbiddenAccount is #1582's template gate.
//
// A CloudFormation template that creates a role another account may assume
// can be deployed in any account the deployer's credentials reach, including
// one spore.host must never be trusted into. CloudFormation cannot ask AWS
// Organizations, but it can refuse by account: every such template must carry
// a Rules assertion that AWS::AccountId is not each of
// trustboundary.ForbiddenAccounts().
//
// The templates are discovered, not listed: every YAML or JSON file in the
// repository that parses as a template is read, and a template is
// cross-account when an AWS::IAM::Role's AssumeRolePolicyDocument names an
// AWS principal. Discovery finding none fails, because that means it has
// broken, not that the repository is clean.
func TestCrossAccountTemplatesRefuseTheForbiddenAccount(t *testing.T) {
	var crossAccount []string
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if treewalk.Vanished(".", path, err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "testdata", "build", "dist":
				return filepath.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(path)
		if ext != ".yaml" && ext != ".yml" && ext != ".json" {
			return nil
		}
		b, err := os.ReadFile(path) //nolint:gosec // walking the repository
		if err != nil {
			return err
		}
		tmpl, ok := parseTemplate(b)
		if !ok || !trustsAnotherAccount(tmpl) {
			return nil
		}
		crossAccount = append(crossAccount, path)
		for _, acct := range trustboundary.ForbiddenAccounts() {
			if !refusesAccount(tmpl, acct) {
				t.Errorf("%s creates a role another account may assume but has no Rules assertion "+
					"!Not [!Equals [!Ref 'AWS::AccountId', '%s']] (#1582)", path, acct)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(crossAccount) == 0 {
		t.Fatal("found no cross-account CloudFormation template; the discovery has broken, not the repository")
	}
	t.Logf("cross-account templates: %s", strings.Join(crossAccount, ", "))
}

// parseTemplate reads a CloudFormation template, short-form intrinsics
// (!Ref, !Not, ...) turned into their long form, and reports whether it is
// one (it has Resources).
func parseTemplate(b []byte) (map[string]interface{}, bool) {
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil || len(doc.Content) == 0 {
		return nil, false
	}
	m, ok := cfnValue(doc.Content[0]).(map[string]interface{})
	if !ok {
		return nil, false
	}
	_, ok = m["Resources"].(map[string]interface{})
	return m, ok
}

// cfnValue is a YAML node as plain values, with a short-form intrinsic tag
// (!Ref x, !Not [...]) rewritten as its long form ({"Ref": x}, {"Fn::Not": [...]}).
func cfnValue(n *yaml.Node) interface{} {
	var v interface{}
	switch n.Kind {
	case yaml.MappingNode:
		m := map[string]interface{}{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			m[n.Content[i].Value] = cfnValue(n.Content[i+1])
		}
		v = m
	case yaml.SequenceNode:
		s := make([]interface{}, 0, len(n.Content))
		for _, c := range n.Content {
			s = append(s, cfnValue(c))
		}
		v = s
	case yaml.AliasNode:
		return cfnValue(n.Alias)
	default:
		v = n.Value
	}
	if strings.HasPrefix(n.Tag, "!") && !strings.HasPrefix(n.Tag, "!!") {
		fn := strings.TrimPrefix(n.Tag, "!")
		if fn != "Ref" && fn != "Condition" {
			fn = "Fn::" + fn
		}
		return map[string]interface{}{fn: v}
	}
	return v
}

// trustsAnotherAccount reports whether any AWS::IAM::Role in tmpl trusts an
// AWS principal (an account or a role in one), rather than a service only.
func trustsAnotherAccount(tmpl map[string]interface{}) bool {
	resources, _ := tmpl["Resources"].(map[string]interface{})
	for _, r := range resources {
		res, _ := r.(map[string]interface{})
		if res["Type"] != "AWS::IAM::Role" {
			continue
		}
		props, _ := res["Properties"].(map[string]interface{})
		doc, _ := props["AssumeRolePolicyDocument"].(map[string]interface{})
		stmts, _ := doc["Statement"].([]interface{})
		for _, s := range stmts {
			st, _ := s.(map[string]interface{})
			if p, ok := st["Principal"].(map[string]interface{}); ok {
				if _, aws := p["AWS"]; aws {
					return true
				}
			}
			if st["Principal"] == "*" {
				return true
			}
		}
	}
	return false
}

// refusesAccount reports whether tmpl has a rule asserting
// !Not [!Equals [!Ref 'AWS::AccountId', acct]] (either argument order).
func refusesAccount(tmpl map[string]interface{}, acct string) bool {
	ref := map[string]interface{}{"Ref": "AWS::AccountId"}
	wants := []interface{}{
		map[string]interface{}{"Fn::Not": []interface{}{map[string]interface{}{"Fn::Equals": []interface{}{ref, acct}}}},
		map[string]interface{}{"Fn::Not": []interface{}{map[string]interface{}{"Fn::Equals": []interface{}{acct, ref}}}},
	}
	rules, _ := tmpl["Rules"].(map[string]interface{})
	for _, r := range rules {
		rule, _ := r.(map[string]interface{})
		if _, conditional := rule["RuleCondition"]; conditional {
			continue // a rule that applies only sometimes does not refuse always
		}
		asserts, _ := rule["Assertions"].([]interface{})
		for _, a := range asserts {
			am, _ := a.(map[string]interface{})
			for _, w := range wants {
				if reflect.DeepEqual(am["Assert"], w) {
					return true
				}
			}
		}
	}
	return false
}

// The parser's own check: a template without the rule is caught, one with it
// passes, and a service-only role is not cross-account (rule 1 of writing a
// gate: watch it fail).
func TestCrossAccountTemplateGateCatchesAMissingRule(t *testing.T) {
	const role = `
Resources:
  R:
    Type: AWS::IAM::Role
    Properties:
      AssumeRolePolicyDocument:
        Statement:
          - Effect: Allow
            Principal:
              AWS: !Sub 'arn:aws:iam::${Other}:root'
            Action: sts:AssumeRole
`
	const rule = `
Rules:
  R:
    Assertions:
      - Assert: !Not [!Equals [!Ref 'AWS::AccountId', '942542972736']]
`
	const conditional = `
Rules:
  R:
    RuleCondition: !Equals [!Ref Env, prod]
    Assertions:
      - Assert: !Not [!Equals [!Ref 'AWS::AccountId', '942542972736']]
`
	const service = `
Resources:
  R:
    Type: AWS::IAM::Role
    Properties:
      AssumeRolePolicyDocument:
        Statement:
          - Effect: Allow
            Principal:
              Service: lambda.amazonaws.com
            Action: sts:AssumeRole
`
	for name, c := range map[string]struct {
		src          string
		cross, guard bool
	}{
		"cross-account without the rule": {role, true, false},
		"cross-account with the rule":    {role + rule, true, true},
		"the rule only conditionally":    {role + conditional, true, false},
		"service-only":                   {service, false, false},
	} {
		tmpl, ok := parseTemplate([]byte(c.src))
		if !ok {
			t.Fatalf("%s: not parsed as a template", name)
		}
		if got := trustsAnotherAccount(tmpl); got != c.cross {
			t.Errorf("%s: cross-account = %v, want %v", name, got, c.cross)
		}
		if got := refusesAccount(tmpl, "942542972736"); got != c.guard {
			t.Errorf("%s: refuses = %v, want %v", name, got, c.guard)
		}
	}
}
