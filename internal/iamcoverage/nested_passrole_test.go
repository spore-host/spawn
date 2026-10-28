package iamcoverage

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/spore-host/spawn/internal/treewalk"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/instancerole"
	"gopkg.in/yaml.v3"
)

// passPrincipal is one spore.host-provisioned principal's identity
// statements: a deploy script's here-documents (union, as ParseShellPolicies
// reads them) or one role or function in a template.
type passPrincipal struct {
	name  string
	stmts []map[string]any
}

// TestNoOrchestratorMayPassANestedSpawnRole is #1372's IAM gate.
//
// The nested-spawn roles (pkg/aws.NestedSpawnRolePrefix, spawn-instance-nested-*)
// sit inside the spawn-instance-* family, so every principal granted PassRole on
// role/spawn-instance-* could hand one to an instance it launches: a role that
// may itself call RunInstances and iam:PassRole, given without the --ttl the
// --nested-spawn launch path requires.
//
// The principals are discovered, not listed: every deploy script and template
// in the repository is parsed, and any principal whose Allow lets it pass a
// nested role to EC2 must carry an explicit, unconditional Deny of iam:PassRole
// on exactly arn:aws:iam::<the Allow's account>:role/spawn-instance-nested-*.
// The verdict is then simulated (Deny beats Allow): the nested roles are
// denied, and the roles the Allow exists for (spored-instance-role, a
// per-policy spawn-instance-<hash>) are still passable, so the Deny shadows
// nothing it should not and is shadowed by nothing.
//
// The --nested-spawn role's own policy (pkg/aws.nestedSpawnStatements, Go, not
// a script or template) is deliberately outside this rule: a nested head node
// passes its own role to a nested child (cmd nestedChildProfile), and that
// role's name is a hash of the policy, so the policy cannot name it.
func TestNoOrchestratorMayPassANestedSpawnRole(t *testing.T) {
	principals := discoverPassPrincipals(t)
	nested := spawnaws.NestedSpawnRolePrefix + "0123abcd"

	var covered []string
	for _, p := range principals {
		acct, ok := allowsPass(p.stmts, nested)
		if !ok {
			continue
		}
		covered = append(covered, p.name)
		want := "arn:aws:iam::" + acct + ":role/" + spawnaws.NestedSpawnRolePrefix + "*"
		var denies int
		for _, st := range p.stmts {
			if st["Effect"] != "Deny" || !passesRole(st) {
				continue
			}
			res := resourceList(st["Resource"])
			_, cond := st["Condition"]
			_, notRes := st["NotResource"]
			_, notAct := st["NotAction"]
			if len(res) == 1 && res[0] == want && !cond && !notRes && !notAct {
				denies++
			}
		}
		if denies != 1 {
			t.Errorf("%s may pass role/spawn-instance-* to EC2 but has %d unconditional Deny of iam:PassRole on exactly %s, want 1 (#1372)",
				p.name, denies, want)
		}
		for _, role := range []string{spawnaws.NestedSpawnRoleName(), nested} {
			if passable(p.stmts, acct, role) {
				t.Errorf("%s: simulated PassRole of role/%s to EC2 is allowed (#1372)", p.name, role)
			}
		}
		for _, role := range []string{spawnaws.SporedRoleName, instancerole.NamePrefix + "abc12345"} {
			if !passable(p.stmts, acct, role) {
				t.Errorf("%s: simulated PassRole of role/%s to EC2 is denied; the Deny is wider than the nested roles", p.name, role)
			}
		}
	}
	sort.Strings(covered)
	t.Logf("principals that may pass spawn-instance-*: %v", covered)
	// The two #1372 names; if discovery stops seeing them it is blind.
	for _, want := range []string{
		"scripts/setup-sweep-cross-account-role.sh",
		"lambda/pipeline-orchestrator/template.yaml#PipelineOrchestratorFunction",
	} {
		found := false
		for _, c := range covered {
			found = found || c == want
		}
		if !found {
			t.Errorf("discovery did not find %s among the principals that may pass spawn-instance-* (%v); the walker is stale", want, covered)
		}
	}
}

// discoverPassPrincipals parses every shell script and YAML template in the
// repository for identity policies.
func discoverPassPrincipals(t *testing.T) []passPrincipal {
	t.Helper()
	var out []passPrincipal
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if treewalk.Vanished(repoRoot, path, err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "testdata", "dist":
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(repoRoot, path)
		rel = filepath.ToSlash(rel)
		switch filepath.Ext(path) {
		case ".sh":
			if p, ok := shellPrincipal(path, rel); ok {
				out = append(out, p)
			}
		case ".yaml", ".yml":
			out = append(out, cfnPrincipals(path, rel)...)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func shellPrincipal(path, rel string) (passPrincipal, bool) {
	b, err := os.ReadFile(path) //nolint:gosec // the repository's own scripts
	if err != nil {
		return passPrincipal{}, false // removed mid-walk by another package's test
	}
	p := passPrincipal{name: rel}
	for _, m := range heredoc.FindAllStringSubmatch(string(b), -1) {
		if m[1] != m[3] {
			continue
		}
		body := substituteShellVars(m[2])
		if !strings.Contains(body, `"Statement"`) || strings.Contains(body, `"Principal"`) {
			continue
		}
		var doc struct {
			Statement json.RawMessage
		}
		if json.Unmarshal([]byte(body), &doc) != nil {
			continue
		}
		var stmts []map[string]any
		if json.Unmarshal(doc.Statement, &stmts) != nil {
			var one map[string]any
			if json.Unmarshal(doc.Statement, &one) != nil {
				continue
			}
			stmts = []map[string]any{one}
		}
		p.stmts = append(p.stmts, stmts...)
	}
	return p, len(p.stmts) > 0
}

func cfnPrincipals(path, rel string) []passPrincipal {
	b, err := os.ReadFile(path) //nolint:gosec // the repository's own templates
	if err != nil {
		return nil
	}
	var doc yaml.Node
	if yaml.Unmarshal(b, &doc) != nil || len(doc.Content) == 0 {
		return nil
	}
	root, _ := cfnValue(&doc).(map[string]any)
	resources, _ := root["Resources"].(map[string]any)
	var out []passPrincipal
	for id, r := range resources {
		res, _ := r.(map[string]any)
		props, _ := res["Properties"].(map[string]any)
		var docs []any
		switch res["Type"] {
		case "AWS::IAM::Role":
			for _, pol := range asSlice(props["Policies"]) {
				pm, _ := pol.(map[string]any)
				docs = append(docs, pm["PolicyDocument"])
			}
			for _, other := range resources {
				om, _ := other.(map[string]any)
				if om["Type"] != "AWS::IAM::Policy" && om["Type"] != "AWS::IAM::ManagedPolicy" {
					continue
				}
				op, _ := om["Properties"].(map[string]any)
				for _, rr := range asSlice(op["Roles"]) {
					if ref, ok := rr.(map[string]any); ok && ref["Ref"] == id {
						docs = append(docs, op["PolicyDocument"])
					}
				}
			}
		case "AWS::Serverless::Function":
			for _, raw := range asSlice(props["Policies"]) {
				docs = append(docs, ifBranches(raw)...)
			}
		default:
			continue
		}
		p := passPrincipal{name: rel + "#" + id}
		for _, d := range docs {
			for _, branch := range ifBranches(d) {
				dm, _ := branch.(map[string]any)
				for _, s := range asSlice(dm["Statement"]) {
					for _, st := range ifBranches(s) {
						if sm, ok := st.(map[string]any); ok {
							p.stmts = append(p.stmts, sm)
						}
					}
				}
			}
		}
		if len(p.stmts) > 0 {
			out = append(out, p)
		}
	}
	return out
}

// passesRole reports whether a statement's Action covers iam:PassRole.
func passesRole(st map[string]any) bool {
	for _, a := range stringList(st["Action"]) {
		if globMatch(strings.ToLower(a), "iam:passrole") {
			return true
		}
	}
	return false
}

// resourceList is a statement's Resource values, with Fn::Sub strings taken as
// written and their ${...} variables (an account id) as X, as
// substituteShellVars renders a script's.
func resourceList(v any) []string {
	var out []string
	for _, r := range asSlice(v) {
		switch x := r.(type) {
		case string:
			out = append(out, x)
		case map[string]any:
			if s, ok := x["Fn::Sub"].(string); ok {
				out = append(out, shellVar.ReplaceAllString(s, "X"))
			}
		}
	}
	return out
}

// arnAccount matches a role ARN's account segment.
var arnAccount = regexp.MustCompile(`^arn:aws:iam::([^:]*):role/`)

// allowsPass reports whether an Allow statement lets the principal pass role
// to EC2, and the account segment of the resource that matched it.
func allowsPass(stmts []map[string]any, role string) (string, bool) {
	for _, st := range stmts {
		if st["Effect"] != "Allow" || !passesRole(st) {
			continue
		}
		for _, r := range resourceList(st["Resource"]) {
			m := arnAccount.FindStringSubmatch(r)
			if m == nil {
				if r == "*" {
					return "*", true
				}
				continue
			}
			if globMatch(r, "arn:aws:iam::"+m[1]+":role/"+role) && toEC2(st) {
				return m[1], true
			}
		}
	}
	return "", false
}

// toEC2 reports whether a statement's condition admits passing to EC2.
func toEC2(st map[string]any) bool {
	cond, ok := st["Condition"].(map[string]any)
	if !ok {
		return true
	}
	for op, kv := range cond {
		m, _ := kv.(map[string]any)
		for k, v := range m {
			if k != "iam:PassedToService" {
				return false // not modelled: treat as not matching
			}
			ok := false
			for _, s := range stringList(v) {
				if (op == "StringEquals" && s == "ec2.amazonaws.com") || (op == "StringLike" && globMatch(s, "ec2.amazonaws.com")) {
					ok = true
				}
			}
			if !ok {
				return false
			}
		}
	}
	return true
}

// passable simulates iam:PassRole of role/<role> in acct to EC2: some Allow
// matches and no Deny does (an explicit Deny beats an Allow).
func passable(stmts []map[string]any, acct, role string) bool {
	arn := "arn:aws:iam::" + strings.ReplaceAll(acct, "*", "111122223333") + ":role/" + role
	allowed := false
	for _, st := range stmts {
		if !passesRole(st) || !toEC2(st) {
			continue
		}
		matches := false
		for _, r := range resourceList(st["Resource"]) {
			matches = matches || globMatch(r, arn)
		}
		if !matches {
			continue
		}
		switch st["Effect"] {
		case "Deny":
			return false
		case "Allow":
			allowed = true
		}
	}
	return allowed
}

// globMatch is IAM's wildcard match: * any run, ? any one character.
func globMatch(pattern, s string) bool {
	var re strings.Builder
	re.WriteString("^")
	for _, c := range pattern {
		switch c {
		case '*':
			re.WriteString(".*")
		case '?':
			re.WriteString(".")
		default:
			re.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	re.WriteString("$")
	return regexp.MustCompile(re.String()).MatchString(s)
}
