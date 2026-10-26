package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/spore-host/spawn/pkg/botsecret"
)

// The bot workspace signing secret is stored KMS-encrypted (#868). These gates
// keep it that way: every Go writer goes through pkg/botsecret, and the
// templates grant exactly the KMS use that package makes.

// botsecretOwner is the one file allowed to name the signing_secret attribute.
const botsecretOwner = "pkg/botsecret/botsecret.go"

var signingSecretAttr = regexp.MustCompile(`\bsigning_secret\b`)

// TestSigningSecretAttributeOnlyInBotsecret walks every non-test Go file in the
// repo, nested modules included, and fails on any string literal naming the
// signing_secret attribute outside pkg/botsecret, and on any struct tag mapping
// a field to it for a marshaller other than encoding/json (a dynamodbav tag
// would let attributevalue.MarshalMap write the field as is). A writer that
// builds the item by hand, by struct or by update expression, has to name the
// attribute, so it is caught; one that goes through botsecret.Put is not.
func TestSigningSecretAttributeOnlyInBotsecret(t *testing.T) {
	var files, ownerHits int
	var bad []string
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		files++
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Errorf("parse %s: %v", p, perr)
			return nil
		}
		rel := filepath.ToSlash(p)
		tags := map[*ast.BasicLit]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			if fld, ok := n.(*ast.Field); ok && fld.Tag != nil {
				tags[fld.Tag] = true
				tag, _ := strconv.Unquote(fld.Tag.Value)
				for _, key := range tagKeys(tag) {
					if key == "json" {
						continue
					}
					name, _, _ := strings.Cut(reflect.StructTag(tag).Get(key), ",")
					if name == "signing_secret" {
						bad = append(bad, fset.Position(fld.Pos()).String()+": struct tag "+key+`:"signing_secret"`)
					}
				}
			}
			return true
		})
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING || tags[lit] {
				return true
			}
			s, uerr := strconv.Unquote(lit.Value)
			if uerr != nil || !signingSecretAttr.MatchString(s) {
				return true
			}
			if rel == botsecretOwner {
				ownerHits++
				return true
			}
			bad = append(bad, fset.Position(lit.Pos()).String()+": string literal "+lit.Value)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 100 {
		t.Fatalf("discovered only %d Go files; the walk is broken, not the repo clean", files)
	}
	if ownerHits == 0 {
		t.Fatalf("%s does not name the attribute; the scan cannot see literals, or the owner moved", botsecretOwner)
	}
	for _, b := range bad {
		t.Errorf("%s — read or write the signing secret through pkg/botsecret (Put/Stored/Open), which encrypts it; naming the attribute directly can store it in the clear (#868)", b)
	}
}

// tagKeys lists the keys of a struct tag (`a:"x" b:"y"` -> a, b).
func tagKeys(tag string) []string {
	var keys []string
	for _, m := range regexp.MustCompile(`([A-Za-z0-9_]+):"`).FindAllStringSubmatch(tag, -1) {
		keys = append(keys, m[1])
	}
	return keys
}

// kmsGrant is one IAM statement granting kms: actions, normalised.
type kmsGrant struct {
	where     string
	actions   []string
	resource  string // "!GetAtt X.Arn" for CFN, the literal for JSON
	purpose   string // Condition StringEquals kms:EncryptionContext:purpose
	aliases   []string
	principal string
}

const botTemplatePath = "deployment/cloudformation/spore-bot.yaml"

// TestSporeBotTemplateGrantsOnlyTheSigningSecretKMSUse: the self-hosted bot
// template creates the key behind botsecret.DefaultKeyAlias and grants the bot
// function exactly kms:Encrypt and kms:Decrypt on it, conditioned on the
// encryption-context purpose botsecret always sets. Every IAM statement in the
// template is discovered; any other kms: grant fails.
func TestSporeBotTemplateGrantsOnlyTheSigningSecretKMSUse(t *testing.T) {
	b, err := os.ReadFile(botTemplatePath)
	if err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	resources := mapGet(doc.Content[0], "Resources")
	if resources == nil {
		t.Fatal("no Resources")
	}
	types := map[string]string{}
	var keys, aliasTargets []string
	var grants []kmsGrant
	for i := 0; i+1 < len(resources.Content); i += 2 {
		name, res := resources.Content[i].Value, resources.Content[i+1]
		ty := ""
		if n := mapGet(res, "Type"); n != nil {
			ty = n.Value
		}
		types[name] = ty
		switch ty {
		case "AWS::KMS::Key":
			keys = append(keys, name)
		case "AWS::KMS::Alias":
			props := mapGet(res, "Properties")
			if a := mapGet(props, "AliasName"); a != nil && a.Value == botsecret.DefaultKeyAlias {
				if tgt := mapGet(props, "TargetKeyId"); tgt != nil && tgt.Tag == "!Ref" {
					aliasTargets = append(aliasTargets, tgt.Value)
				}
			}
		}
		walkYAML(res, func(n *yaml.Node) {
			if mapGet(n, "Effect") == nil || mapGet(n, "Action") == nil {
				return
			}
			g := kmsGrant{where: name, actions: yamlStrings(mapGet(n, "Action"))}
			if !slices.ContainsFunc(g.actions, func(a string) bool { return strings.HasPrefix(a, "kms:") }) {
				return
			}
			if r := mapGet(n, "Resource"); r != nil {
				g.resource = strings.TrimSpace(r.Tag + " " + r.Value)
			}
			if v := mapGet(mapGet(mapGet(n, "Condition"), "StringEquals"), "kms:EncryptionContext:purpose"); v != nil {
				g.purpose = v.Value
			}
			if p := mapGet(mapGet(n, "Principal"), "AWS"); p != nil {
				g.principal = p.Tag + " " + p.Value
			}
			grants = append(grants, g)
		})
	}
	if len(keys) != 1 {
		t.Fatalf("want exactly one AWS::KMS::Key, found %v", keys)
	}
	key := keys[0]
	if !slices.Equal(aliasTargets, []string{key}) {
		t.Errorf("want one AWS::KMS::Alias %q targeting !Ref %s (the key pkg/botsecret uses by default), found targets %v", botsecret.DefaultKeyAlias, key, aliasTargets)
	}
	var fnGrants int
	for _, g := range grants {
		if g.where == key {
			// The key policy: delegation to this account's IAM only.
			if g.principal != "!Sub arn:aws:iam::${AWS::AccountId}:root" {
				t.Errorf("key policy statement grants %v to %q; only the account root (IAM delegation) belongs there", g.actions, g.principal)
			}
			continue
		}
		if types[g.where] != "AWS::Serverless::Function" {
			t.Errorf("%s (%s) is granted %v; only the bot function should be", g.where, types[g.where], g.actions)
			continue
		}
		fnGrants++
		assertSigningSecretGrant(t, g, "!GetAtt "+key+".Arn")
	}
	if fnGrants != 1 {
		t.Errorf("bot function has %d kms: statements, want exactly 1", fnGrants)
	}
}

const dashboardRoleScript = "lambda/dashboard-api/scripts/setup-dashboard-lambda-role.sh"

// TestDashboardRoleScriptGrantsTheSigningSecretKMSUse: the dashboard's Slack
// OAuth install writes the signing secret through botsecret, so the script
// that provisions SpawnDashboardLambdaRole must grant the same, and only that,
// KMS use, and must actually attach it (put-role-policy, which updates an
// existing role; create-policy would not).
func TestDashboardRoleScriptGrantsTheSigningSecretKMSUse(t *testing.T) {
	b, err := os.ReadFile(dashboardRoleScript)
	if err != nil {
		t.Fatal(err)
	}
	script := string(b)
	heredoc := regexp.MustCompile(`(?ms)^cat > (\S+) <<EOF\n(.*?)^EOF$`)
	docs := heredoc.FindAllStringSubmatch(script, -1)
	if len(docs) == 0 {
		t.Fatal("no heredoc policy documents discovered")
	}
	var grants []kmsGrant
	var kmsFiles []string
	for _, d := range docs {
		var pol struct {
			Statement []struct {
				Action    any            `json:"Action"`
				Resource  any            `json:"Resource"`
				Condition map[string]any `json:"Condition"`
			}
		}
		body := strings.ReplaceAll(d[2], "${ACCOUNT_ID}", "123456789012")
		if err := json.Unmarshal([]byte(body), &pol); err != nil {
			t.Fatalf("%s: heredoc for %s is not JSON: %v", dashboardRoleScript, d[1], err)
		}
		for _, st := range pol.Statement {
			g := kmsGrant{where: d[1], actions: jsonStrings(st.Action)}
			if !slices.ContainsFunc(g.actions, func(a string) bool { return strings.HasPrefix(a, "kms:") }) {
				continue
			}
			g.resource = strings.Join(jsonStrings(st.Resource), ",")
			if se, ok := st.Condition["StringEquals"].(map[string]any); ok {
				g.purpose, _ = se["kms:EncryptionContext:purpose"].(string)
			}
			if fa, ok := st.Condition["ForAnyValue:StringEquals"].(map[string]any); ok {
				g.aliases = jsonStrings(fa["kms:ResourceAliases"])
			}
			grants = append(grants, g)
			kmsFiles = append(kmsFiles, d[1])
		}
	}
	if len(grants) != 1 {
		t.Fatalf("want exactly one kms: statement in %s, found %d", dashboardRoleScript, len(grants))
	}
	g := grants[0]
	assertSigningSecretGrant(t, g, "arn:aws:kms:*:123456789012:key/*")
	if !slices.Equal(g.aliases, []string{botsecret.DefaultKeyAlias}) {
		t.Errorf("grant is not scoped to the key behind %s: kms:ResourceAliases = %v", botsecret.DefaultKeyAlias, g.aliases)
	}
	// The document must be attached by an actual put-role-policy invocation.
	joined := strings.ReplaceAll(script, "\\\n", " ")
	attach := regexp.MustCompile(`(?m)^\s*aws iam put-role-policy\b.*--role-name "\$ROLE_NAME".*--policy-document file://` + regexp.QuoteMeta(kmsFiles[0]) + `\s*$`)
	if !attach.MatchString(joined) {
		t.Errorf("%s writes %s but no `aws iam put-role-policy --role-name \"$ROLE_NAME\" ... --policy-document file://%s` attaches it", dashboardRoleScript, kmsFiles[0], kmsFiles[0])
	}
}

func assertSigningSecretGrant(t *testing.T, g kmsGrant, wantResource string) {
	t.Helper()
	got := slices.Clone(g.actions)
	sort.Strings(got)
	if want := []string{"kms:Decrypt", "kms:Encrypt"}; !slices.Equal(got, want) {
		t.Errorf("%s: kms actions %v, want exactly %v (seal on write, open on read)", g.where, got, want)
	}
	if g.resource != wantResource {
		t.Errorf("%s: kms Resource %q, want %q", g.where, g.resource, wantResource)
	}
	if g.purpose != botsecret.Purpose {
		t.Errorf("%s: kms grant not conditioned on kms:EncryptionContext:purpose = %q (got %q)", g.where, botsecret.Purpose, g.purpose)
	}
}

func walkYAML(n *yaml.Node, visit func(*yaml.Node)) {
	if n == nil {
		return
	}
	visit(n)
	for _, c := range n.Content {
		walkYAML(c, visit)
	}
}

func yamlStrings(n *yaml.Node) []string {
	if n == nil {
		return nil
	}
	if n.Kind == yaml.ScalarNode {
		return []string{n.Value}
	}
	var out []string
	for _, c := range n.Content {
		out = append(out, c.Value)
	}
	return out
}

func jsonStrings(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		var out []string
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
