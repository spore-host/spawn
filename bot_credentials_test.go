package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
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

	"github.com/spore-host/spawn/internal/treewalk"
	"github.com/spore-host/spawn/pkg/botsecret"
)

// The bot workspace credentials (pkg/botsecret.Fields: signing secret, bot
// token, refresh token, incoming webhook URL) are stored KMS-encrypted (#868).
// These gates keep it that way: every Go writer goes through pkg/botsecret, and
// the templates grant exactly the KMS use each principal makes.

// botsecretOwner is the one file allowed to name the credential attributes.
const botsecretOwner = "pkg/botsecret/botsecret.go"

// credentialAttributes discovers the attribute names from the Field literals
// in botsecretOwner, rather than listing them here, and checks it found one
// per botsecret.Fields() entry.
func credentialAttributes(t *testing.T) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), botsecretOwner, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	var attrs []string
	ast.Inspect(f, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if id, ok := cl.Type.(*ast.Ident); !ok || id.Name != "Field" || len(cl.Elts) == 0 {
			return true
		}
		if lit, ok := cl.Elts[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
			s, _ := strconv.Unquote(lit.Value)
			attrs = append(attrs, s)
		}
		return true
	})
	if len(attrs) != len(botsecret.Fields()) {
		t.Fatalf("found %d Field literals %v in %s, but botsecret.Fields() has %d entries", len(attrs), attrs, botsecretOwner, len(botsecret.Fields()))
	}
	return attrs
}

// TestCredentialAttributesOnlyInBotsecret walks every non-test Go file in the
// repo, nested modules included, and fails on any string literal naming a
// credential attribute outside pkg/botsecret, and on any struct tag mapping a
// field to one for a marshaller other than encoding/json (a dynamodbav tag
// would let attributevalue.MarshalMap write the field as is). A writer that
// builds the item by hand, by struct, map or update expression, has to name
// the attribute, so it is caught; one that goes through pkg/botsecret is not.
//
// One exemption: a literal inside a url.Values composite literal is an OAuth
// form field sent to Slack (grant_type=refresh_token), not the attribute.
func TestCredentialAttributesOnlyInBotsecret(t *testing.T) {
	attrs := credentialAttributes(t)
	attrRE := regexp.MustCompile(`\b(` + strings.Join(attrs, "|") + `)\b`)
	var files int
	ownerHits := map[string]int{}
	var bad []string
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if treewalk.Vanished(".", p, err) {
				return nil
			}
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
		skip := map[*ast.BasicLit]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.Field:
				if x.Tag == nil {
					return true
				}
				skip[x.Tag] = true
				tag, _ := strconv.Unquote(x.Tag.Value)
				for _, key := range tagKeys(tag) {
					if key == "json" {
						continue
					}
					name, _, _ := strings.Cut(reflect.StructTag(tag).Get(key), ",")
					if slices.Contains(attrs, name) {
						bad = append(bad, fset.Position(x.Pos()).String()+": struct tag "+key+`:"`+name+`"`)
					}
				}
			case *ast.CompositeLit:
				if sel, ok := x.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Values" {
					if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "url" {
						ast.Inspect(x, func(m ast.Node) bool {
							if lit, ok := m.(*ast.BasicLit); ok {
								skip[lit] = true
							}
							return true
						})
					}
				}
			}
			return true
		})
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING || skip[lit] {
				return true
			}
			s, uerr := strconv.Unquote(lit.Value)
			if uerr != nil {
				return true
			}
			found := attrRE.FindAllString(s, -1)
			if len(found) == 0 {
				return true
			}
			if rel == botsecretOwner {
				for _, a := range found {
					ownerHits[a]++
				}
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
	for _, a := range attrs {
		if ownerHits[a] == 0 {
			t.Errorf("%s does not name %q in a string literal; the scan cannot see literals, or the owner moved", botsecretOwner, a)
		}
	}
	for _, b := range bad {
		t.Errorf("%s — read or write workspace credentials through pkg/botsecret (Field.Put/Stored/Open, RotatedTokens), which encrypts them; naming the attribute directly can store it in the clear (#868)", b)
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
	resource  string   // "!GetAtt X.Arn" for CFN, the literal for JSON
	purposes  []string // Condition StringEquals kms:EncryptionContext:purpose
	aliases   []string
	principal string
}

// purposes returns the botsecret purposes of fs.
func purposes(fs ...botsecret.Field) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Purpose())
	}
	sort.Strings(out)
	return out
}

// Who may do what with each credential (the intent the grants must match):
//
//   - spore-bot (the bot function in spore-bot.yaml) writes workspaces in its
//     OAuth install and admin API, so it encrypts every field. It decrypts the
//     signing secret to verify requests, the bot token to call Slack and the
//     webhook URL to post, and never refreshes tokens, so it cannot open a
//     refresh token.
//   - the dashboard role writes all four in its Slack OAuth install. It
//     decrypts the signing secret it preserves and the bot and refresh tokens
//     its refresh paths use, and never reads the webhook URL.
var (
	botFunctionKMS = map[string][]string{
		"kms:Encrypt": purposes(botsecret.Fields()...),
		"kms:Decrypt": purposes(botsecret.SigningSecret, botsecret.BotToken, botsecret.IncomingWebhookURL),
	}
	dashboardRoleKMS = map[string][]string{
		"kms:Encrypt": purposes(botsecret.Fields()...),
		"kms:Decrypt": purposes(botsecret.SigningSecret, botsecret.BotToken, botsecret.RefreshToken),
	}
)

const botTemplatePath = "deployment/cloudformation/spore-bot.yaml"

// TestSporeBotTemplateGrantsOnlyTheCredentialKMSUse: the self-hosted bot
// template creates the key behind botsecret.DefaultKeyAlias and grants the bot
// function exactly botFunctionKMS on it: each kms: action conditioned on the
// encryption-context purposes of the fields it needs. Every IAM statement in
// the template is discovered; any other kms: grant fails.
func TestSporeBotTemplateGrantsOnlyTheCredentialKMSUse(t *testing.T) {
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
			g.purposes = yamlStrings(mapGet(mapGet(mapGet(n, "Condition"), "StringEquals"), "kms:EncryptionContext:purpose"))
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
	var fnGrants []kmsGrant
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
		fnGrants = append(fnGrants, g)
	}
	assertCredentialGrants(t, botTemplatePath, fnGrants, "!GetAtt "+key+".Arn", botFunctionKMS)
}

const dashboardRoleScript = "lambda/dashboard-api/scripts/setup-dashboard-lambda-role.sh"

// TestDashboardRoleScriptGrantsTheCredentialKMSUse: the dashboard's Slack
// OAuth install and token refresh go through botsecret, so the script that
// provisions SpawnDashboardLambdaRole must grant exactly dashboardRoleKMS,
// scoped to the key behind the alias, and must actually attach it with
// put-role-policy (which updates an existing role).
func TestDashboardRoleScriptGrantsTheCredentialKMSUse(t *testing.T) {
	b, err := os.ReadFile(dashboardRoleScript)
	if err != nil {
		t.Fatal(err)
	}
	script := string(b)
	// Policy documents are heredocs, either captured (VAR=$(cat <<EOF) or
	// written to a file (cat > FILE <<EOF).
	heredoc := regexp.MustCompile(`(?ms)^(?:(\w+)=\$\(cat|cat > (\S+)) <<EOF\n(.*?)^EOF$`)
	docs := heredoc.FindAllStringSubmatch(script, -1)
	if len(docs) == 0 {
		t.Fatal("no heredoc policy documents discovered")
	}
	shellVar := regexp.MustCompile(`\$\{\w+\}`)
	var grants []kmsGrant
	var kmsDocs []string // how each kms: document is passed to --policy-document
	for _, d := range docs {
		ref := "file://" + d[2]
		if d[1] != "" {
			ref = `"$` + d[1] + `"`
		}
		var pol struct {
			Statement []struct {
				Action    any            `json:"Action"`
				Resource  any            `json:"Resource"`
				Condition map[string]any `json:"Condition"`
			}
		}
		body := strings.ReplaceAll(d[3], "${ACCOUNT_ID}", "123456789012")
		body = shellVar.ReplaceAllString(body, "X")
		if err := json.Unmarshal([]byte(body), &pol); err != nil {
			continue // not a policy document (e.g. a trust policy fragment)
		}
		has := false
		for _, st := range pol.Statement {
			g := kmsGrant{where: ref, actions: jsonStrings(st.Action)}
			if !slices.ContainsFunc(g.actions, func(a string) bool { return strings.HasPrefix(a, "kms:") }) {
				continue
			}
			g.resource = strings.Join(jsonStrings(st.Resource), ",")
			if se, ok := st.Condition["StringEquals"].(map[string]any); ok {
				g.purposes = jsonStrings(se["kms:EncryptionContext:purpose"])
			}
			if fa, ok := st.Condition["ForAnyValue:StringEquals"].(map[string]any); ok {
				g.aliases = jsonStrings(fa["kms:ResourceAliases"])
			}
			if !slices.Equal(g.aliases, []string{botsecret.DefaultKeyAlias}) {
				t.Errorf("%s: %v is not scoped to the key behind %s: kms:ResourceAliases = %v", ref, g.actions, botsecret.DefaultKeyAlias, g.aliases)
			}
			grants = append(grants, g)
			has = true
		}
		if has {
			kmsDocs = append(kmsDocs, ref)
		}
	}
	assertCredentialGrants(t, dashboardRoleScript, grants, "arn:aws:kms:*:123456789012:key/*", dashboardRoleKMS)
	// Each document must be attached by an actual put-role-policy invocation.
	joined := strings.ReplaceAll(script, "\\\n", " ")
	for _, ref := range kmsDocs {
		attach := regexp.MustCompile(`(?m)^\s*aws iam put-role-policy\b.*--role-name "\$ROLE_NAME".*--policy-document ` + regexp.QuoteMeta(ref) + `\s*$`)
		if !attach.MatchString(joined) {
			t.Errorf("%s builds %s but no `aws iam put-role-policy --role-name \"$ROLE_NAME\" ... --policy-document %s` attaches it", dashboardRoleScript, ref, ref)
		}
	}
}

// assertCredentialGrants checks that grants, taken together, allow exactly
// want (kms action -> purposes) on wantResource: only kms:Encrypt and
// kms:Decrypt, every statement conditioned on purpose, no purpose beyond want.
func assertCredentialGrants(t *testing.T, where string, grants []kmsGrant, wantResource string, want map[string][]string) {
	t.Helper()
	if len(grants) == 0 {
		t.Fatalf("%s: no kms: statements discovered", where)
	}
	got := map[string][]string{}
	for _, g := range grants {
		if g.resource != wantResource {
			t.Errorf("%s: kms Resource %q, want %q", g.where, g.resource, wantResource)
		}
		if len(g.purposes) == 0 {
			t.Errorf("%s: %v not conditioned on StringEquals kms:EncryptionContext:purpose, so it covers anything sealed with the key", g.where, g.actions)
		}
		for _, a := range g.actions {
			if _, ok := want[a]; !ok {
				t.Errorf("%s: grants %s; only %v are needed (seal on write, open on read)", g.where, a, slices.Sorted(maps.Keys(want)))
				continue
			}
			got[a] = append(got[a], g.purposes...)
		}
	}
	for a, w := range want {
		g := slices.Compact(slices.Sorted(slices.Values(got[a])))
		if !slices.Equal(g, w) {
			t.Errorf("%s: %s allowed for purposes %v, want exactly %v", where, a, g, w)
		}
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
