package main

import (
	"encoding/json"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// botAppSecretNames are the spore-bot app-level secrets (spawn#1591): the env
// var names the bot reads them under, and the keys of the BotAppSecrets JSON.
var botAppSecretNames = []string{
	"BOT_OAUTH_SECRET",
	"SLACK_SIGNING_SECRET",
	"SPORE_SLACK_CLIENT_SECRET",
	"TEAMS_APP_SECRET",
	"TWILIO_AUTH_TOKEN",
}

// TestSporeBotTemplateKeepsAppSecretsOutOfTheEnvironment: the self-hosted bot
// template puts none of the app-level secrets in the function's environment,
// which anyone with lambda:GetFunctionConfiguration can read (#1591). They go
// into one Secrets Manager secret, keyed by the same names; the function gets
// its ARN and GetSecretValue on that secret alone.
func TestSporeBotTemplateKeepsAppSecretsOutOfTheEnvironment(t *testing.T) {
	b, err := os.ReadFile(botTemplatePath)
	if err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	resources := mapGet(doc.Content[0], "Resources")

	var secrets []string
	var fn *yaml.Node
	for i := 0; i+1 < len(resources.Content); i += 2 {
		name, res := resources.Content[i].Value, resources.Content[i+1]
		switch ty := mapGet(res, "Type"); {
		case ty == nil:
		case ty.Value == "AWS::SecretsManager::Secret":
			secrets = append(secrets, name)
		case ty.Value == "AWS::Serverless::Function":
			if fn != nil {
				t.Fatal("more than one function; the test assumes the one bot function")
			}
			fn = res
		}
	}
	if fn == nil || len(secrets) != 1 {
		t.Fatalf("want one function and one AWS::SecretsManager::Secret, found secrets %v", secrets)
	}
	secret := secrets[0]

	// The secret's value: exactly the five names, each from a parameter.
	ss := mapGet(mapGet(mapGet(resources, secret), "Properties"), "SecretString")
	if ss == nil || ss.Tag != "!Sub" {
		t.Fatalf("%s.SecretString must be a !Sub of the NoEcho parameters", secret)
	}
	var obj map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSpace(ss.Value)), &obj); err != nil {
		t.Fatalf("%s.SecretString is not a JSON object: %v", secret, err)
	}
	var keys []string
	for k, v := range obj {
		keys = append(keys, k)
		if !strings.HasPrefix(v, "${") || !strings.HasSuffix(v, "}") {
			t.Errorf("%s: %s = %q, want a ${Parameter} substitution", secret, k, v)
		}
	}
	sort.Strings(keys)
	if !slices.Equal(keys, botAppSecretNames) {
		t.Errorf("%s keys = %v, want %v", secret, keys, botAppSecretNames)
	}

	// The function's environment: none of them, and the secret's ARN.
	vars := mapGet(mapGet(mapGet(fn, "Properties"), "Environment"), "Variables")
	if vars == nil {
		t.Fatal("function has no Environment.Variables")
	}
	for _, n := range botAppSecretNames {
		if mapGet(vars, n) != nil {
			t.Errorf("function environment sets %s: an app-level secret, readable by lambda:GetFunctionConfiguration (#1591)", n)
		}
	}
	if v := mapGet(vars, "BOT_APP_SECRETS_ARN"); v == nil || v.Tag != "!Ref" || v.Value != secret {
		t.Errorf("BOT_APP_SECRETS_ARN must be !Ref %s", secret)
	}

	// GetSecretValue, on that secret only, and nothing else of secretsmanager.
	var grants []string
	walkYAML(fn, func(n *yaml.Node) {
		if mapGet(n, "Effect") == nil || mapGet(n, "Action") == nil {
			return
		}
		for _, a := range yamlStrings(mapGet(n, "Action")) {
			if !strings.HasPrefix(a, "secretsmanager:") {
				continue
			}
			r := mapGet(n, "Resource")
			res := ""
			if r != nil {
				res = strings.TrimSpace(r.Tag + " " + r.Value)
			}
			grants = append(grants, a+" on "+res)
		}
	})
	if want := []string{"secretsmanager:GetSecretValue on !Ref " + secret}; !slices.Equal(grants, want) {
		t.Errorf("secretsmanager grants = %v, want %v", grants, want)
	}
}
