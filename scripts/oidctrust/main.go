// Command oidctrust checks the expected GitHub OIDC trust of every AWS role a
// spawn workflow assumes, and who may sign with the spored signing key (#1601),
// against the inventory in infra/iam/github-oidc/, and compares live IAM, KMS
// and GitHub ruleset output against it. It makes no AWS or GitHub call: the
// live documents are files someone with read access fetched (README.md there).
//
//	go -C scripts run ./oidctrust check
//	go -C scripts run ./oidctrust live \
//	    --role GitHubActions-spore-host-release=/tmp/release.json \
//	    --role GitHubActions-spawn-lambda-deploy=/tmp/lambda.json \
//	    --key-policy /tmp/key-policy.json \
//	    --rulesets /tmp/rulesets.json
//
// check (CI) fails when an expected trust breaks a rule below, when the
// signing key's signers are not inventoried roles, or when its required Deny
// does not name exactly those signers. The workflows half (every role a
// workflow assumes is inventoried, its trigger or environment matches the
// trust's sub, kms sign only runs under a signer) is the root module's
// oidc_trust_test.go, which can parse YAML.
//
// The rules every trust follows, as in spore-host's infra/iam/github-oidc:
//   - one statement, Effect Allow, Action sts:AssumeRoleWithWebIdentity;
//   - the principal is token.actions.githubusercontent.com in the role's own
//     account;
//   - aud is StringEquals sts.amazonaws.com;
//   - sub is StringEquals one exact ref or environment of one repository, or
//     StringLike a ref:refs/tags/ pattern (the tag-triggered release);
//   - no other condition, and no forbidden account anywhere.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// forbiddenAccount may never appear in a trust or key policy: spawn's
// forbiddenTrustAccounts refuses it (an Amazon-internal account and an org
// management account).
const forbiddenAccount = "942542972736"

const (
	oidcHost  = "token.actions.githubusercontent.com"
	audKey    = oidcHost + ":aud"
	subKey    = oidcHost + ":sub"
	audience  = "sts.amazonaws.com"
	assumeAct = "sts:AssumeRoleWithWebIdentity"
)

// Role is one entry of trusts.json.
type Role struct {
	Role       string          `json:"role"`
	Account    string          `json:"account"`
	RoleARN    string          `json:"role_arn"`
	Variables  []string        `json:"variables"`
	UsedBy     []string        `json:"used_by"`
	CodifiedIn *string         `json:"codified_in"`
	Why        string          `json:"why"`
	Trust      json.RawMessage `json:"trust"`
}

// Inventory is trusts.json.
type Inventory struct {
	Roles []Role `json:"roles"`
}

// SigningKey is signing-key.json.
type SigningKey struct {
	KeyARN       string          `json:"key_arn"`
	Alias        string          `json:"alias"`
	Account      string          `json:"account"`
	Variables    []string        `json:"variables"`
	Signers      []string        `json:"signers"`
	Why          string          `json:"why"`
	RequiredDeny json.RawMessage `json:"required_deny"`
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "oidctrust:", err)
		os.Exit(1)
	}
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: oidctrust check|live [flags]")
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	dir := fs.String("dir", "../infra/iam/github-oidc", "the inventory directory (trusts.json, signing-key.json)")
	var roles multiFlag
	fs.Var(&roles, "role", "NAME=FILE: `aws iam get-role --role-name NAME` output (live; repeatable)")
	keyPolicy := fs.String("key-policy", "", "`aws kms get-key-policy` output for the signing key (live)")
	rulesets := fs.String("rulesets", "", "a JSON array of spawn's rulesets, each from `gh api repos/spore-host/spawn/rulesets/<id>` (live)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	inv, sk, err := load(*dir)
	if err != nil {
		return err
	}
	problems := Check(inv, sk)
	switch args[0] {
	case "check":
		if len(roles) > 0 || *keyPolicy != "" || *rulesets != "" {
			return errors.New("check takes no live documents; use live")
		}
	case "live":
		if len(roles) == 0 && *keyPolicy == "" && *rulesets == "" {
			return errors.New("live needs at least one --role, --key-policy or --rulesets")
		}
		for _, r := range roles {
			name, file, ok := strings.Cut(r, "=")
			if !ok {
				return fmt.Errorf("--role %q: want NAME=FILE", r)
			}
			b, err := os.ReadFile(file) //nolint:gosec // a file the operator named
			if err != nil {
				return err
			}
			ps, err := CompareRole(inv, name, b)
			if err != nil {
				return fmt.Errorf("%s: %w", file, err)
			}
			report(out, "role "+name, ps)
			problems = append(problems, ps...)
		}
		if *keyPolicy != "" {
			b, err := os.ReadFile(*keyPolicy)
			if err != nil {
				return err
			}
			ps, err := CompareKeyPolicy(sk, b)
			if err != nil {
				return fmt.Errorf("%s: %w", *keyPolicy, err)
			}
			report(out, "key policy "+sk.Alias, ps)
			problems = append(problems, ps...)
		}
		if *rulesets != "" {
			b, err := os.ReadFile(*rulesets)
			if err != nil {
				return err
			}
			ps, err := CheckTagRulesets(b)
			if err != nil {
				return fmt.Errorf("%s: %w", *rulesets, err)
			}
			report(out, "v* tag ruleset", ps)
			problems = append(problems, ps...)
		}
	default:
		return fmt.Errorf("unknown subcommand %q (want check or live)", args[0])
	}
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(out, "FAIL:", p)
		}
		return fmt.Errorf("%d problem(s)", len(problems))
	}
	fmt.Fprintf(out, "ok: %d role(s), signing key %s\n", len(inv.Roles), sk.Alias)
	return nil
}

func report(out io.Writer, what string, problems []string) {
	if len(problems) == 0 {
		fmt.Fprintf(out, "matches: %s\n", what)
	}
}

func load(dir string) (Inventory, SigningKey, error) {
	var inv Inventory
	var sk SigningKey
	b, err := os.ReadFile(filepath.Join(dir, "trusts.json")) //nolint:gosec // the repo's own inventory
	if err != nil {
		return inv, sk, err
	}
	if err := strictUnmarshal(b, &inv); err != nil {
		return inv, sk, fmt.Errorf("trusts.json: %w", err)
	}
	b, err = os.ReadFile(filepath.Join(dir, "signing-key.json")) //nolint:gosec // the repo's own inventory
	if err != nil {
		return inv, sk, err
	}
	if err := strictUnmarshal(b, &sk); err != nil {
		return inv, sk, fmt.Errorf("signing-key.json: %w", err)
	}
	if len(inv.Roles) == 0 {
		return inv, sk, errors.New("trusts.json lists no roles")
	}
	return inv, sk, nil
}

// strictUnmarshal rejects unknown keys other than "_about", so a typo'd field
// is not silently ignored.
func strictUnmarshal(b []byte, v any) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	delete(raw, "_about")
	clean, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(strings.NewReader(string(clean)))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// Statement is a policy statement with every string-or-list field read as a
// list.
type Statement struct {
	Sid       string
	Effect    string
	Principal map[string][]string // "*" principal becomes {"*": ["*"]}
	Action    []string
	NotAction []string
	Resource  []string
	Condition map[string]map[string][]string
}

func strs(v any) ([]string, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case string:
		return []string{t}, nil
	case []any:
		var out []string
		for _, e := range t {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("%v is not a string", e)
			}
			out = append(out, s)
		}
		return out, nil
	}
	return nil, fmt.Errorf("%v is neither a string nor a list of strings", v)
}

// ParsePolicy reads a policy document: JSON, or the URL-encoded JSON IAM's
// raw API returns.
func ParsePolicy(b []byte) ([]Statement, error) {
	s := strings.TrimSpace(string(b))
	if strings.HasPrefix(s, "%7B") || strings.HasPrefix(s, "%7b") {
		u, err := url.QueryUnescape(s)
		if err != nil {
			return nil, err
		}
		s = u
	}
	var doc struct {
		Statement json.RawMessage
	}
	if err := json.Unmarshal([]byte(s), &doc); err != nil {
		return nil, err
	}
	var raws []map[string]any
	if err := json.Unmarshal(doc.Statement, &raws); err != nil {
		var one map[string]any
		if err2 := json.Unmarshal(doc.Statement, &one); err2 != nil {
			return nil, fmt.Errorf("Statement: %w", err)
		}
		raws = []map[string]any{one}
	}
	var out []Statement
	for i, r := range raws {
		st := Statement{Principal: map[string][]string{}, Condition: map[string]map[string][]string{}}
		var err error
		for k, v := range r {
			switch k {
			case "Sid":
				st.Sid, _ = v.(string)
			case "Effect":
				st.Effect, _ = v.(string)
			case "Action":
				st.Action, err = strs(v)
			case "NotAction":
				st.NotAction, err = strs(v)
			case "Resource", "NotResource":
				st.Resource, err = strs(v)
				if k == "NotResource" {
					err = errors.New("NotResource is not used by these policies; read it by hand")
				}
			case "Principal":
				if s, ok := v.(string); ok {
					st.Principal["*"] = []string{s}
					break
				}
				m, ok := v.(map[string]any)
				if !ok {
					err = fmt.Errorf("Principal %v", v)
					break
				}
				for pk, pv := range m {
					if st.Principal[pk], err = strs(pv); err != nil {
						break
					}
				}
			case "NotPrincipal":
				err = errors.New("NotPrincipal is not used by these policies; read it by hand")
			case "Condition":
				m, ok := v.(map[string]any)
				if !ok {
					err = fmt.Errorf("Condition %v", v)
					break
				}
				for op, kv := range m {
					km, ok := kv.(map[string]any)
					if !ok {
						err = fmt.Errorf("Condition %s: %v", op, kv)
						break
					}
					st.Condition[op] = map[string][]string{}
					for ck, cv := range km {
						if st.Condition[op][ck], err = strs(cv); err != nil {
							break
						}
					}
				}
			default:
				err = fmt.Errorf("unknown statement field %q", k)
			}
			if err != nil {
				return nil, fmt.Errorf("statement %d: %w", i, err)
			}
		}
		out = append(out, st)
	}
	return out, nil
}

// canonical is a statement as comparable text: Sid dropped, every list sorted.
func (s Statement) canonical() string {
	sorted := func(v []string) []string { c := append([]string(nil), v...); sort.Strings(c); return c }
	p := map[string][]string{}
	for k, v := range s.Principal {
		p[k] = sorted(v)
	}
	c := map[string]map[string][]string{}
	for op, kv := range s.Condition {
		c[op] = map[string][]string{}
		for k, v := range kv {
			c[op][k] = sorted(v)
		}
	}
	b, _ := json.Marshal(struct {
		Effect    string
		Principal map[string][]string
		Action    []string
		NotAction []string
		Resource  []string
		Condition map[string]map[string][]string
	}{s.Effect, p, sorted(s.Action), sorted(s.NotAction), sorted(s.Resource), c})
	return string(b)
}

var (
	exactSub = regexp.MustCompile(`^repo:[\w.-]+/[\w.-]+:(ref:refs/heads/[\w./-]+|ref:refs/tags/[\w./-]+|environment:[\w.-]+)$`)
	tagSub   = regexp.MustCompile(`^repo:[\w.-]+/[\w.-]+:ref:refs/tags/[\w./*-]+$`)
)

// TrustRules is the list of rule breaks in one role's trust.
func TrustRules(r Role, stmts []Statement) []string {
	var out []string
	bad := func(f string, a ...any) { out = append(out, r.Role+": "+fmt.Sprintf(f, a...)) }
	if len(stmts) != 1 {
		bad("%d statements, want exactly 1", len(stmts))
	}
	provider := "arn:aws:iam::" + r.Account + ":oidc-provider/" + oidcHost
	for _, s := range stmts {
		if s.Effect != "Allow" {
			bad("Effect %q, want Allow", s.Effect)
		}
		if len(s.Principal) != 1 || len(s.Principal["Federated"]) != 1 || s.Principal["Federated"][0] != provider {
			bad("principal %v, want exactly {Federated: %s}", s.Principal, provider)
		}
		if len(s.Action) != 1 || s.Action[0] != assumeAct || len(s.NotAction) > 0 {
			bad("action %v, want exactly %s", s.Action, assumeAct)
		}
		eq, like := s.Condition["StringEquals"], s.Condition["StringLike"]
		for op := range s.Condition {
			if op != "StringEquals" && op != "StringLike" {
				bad("condition operator %s; only StringEquals (aud, sub) and StringLike (a tag sub) are expected", op)
			}
		}
		if a := eq[audKey]; len(a) != 1 || a[0] != audience {
			bad("aud is %v under StringEquals, want exactly %q", a, audience)
		}
		for k := range eq {
			if k != audKey && k != subKey {
				bad("StringEquals %s: not an expected condition key", k)
			}
		}
		for k := range like {
			if k != subKey {
				bad("StringLike %s: only the sub may be a pattern", k)
			}
		}
		switch subs, likes := eq[subKey], like[subKey]; {
		case len(subs) == 1 && len(likes) == 0:
			if !exactSub.MatchString(subs[0]) {
				bad("sub %q is not one exact ref or environment of one repository", subs[0])
			}
		case len(subs) == 0 && len(likes) == 1:
			if !tagSub.MatchString(likes[0]) {
				bad("StringLike sub %q: a pattern is allowed only for the tag name, ref:refs/tags/<pattern>", likes[0])
			}
		default:
			bad("sub is StringEquals %v / StringLike %v, want exactly one value under one of them", subs, likes)
		}
	}
	return out
}

// Check is check: the inventory's own rules.
func Check(inv Inventory, sk SigningKey) []string {
	var out []string
	seen := map[string]bool{}
	arns := map[string]bool{}
	for _, r := range inv.Roles {
		if seen[r.Role] {
			out = append(out, r.Role+": listed twice")
		}
		seen[r.Role] = true
		if want := "arn:aws:iam::" + r.Account + ":role/" + r.Role; r.RoleARN != want {
			out = append(out, fmt.Sprintf("%s: role_arn %q, want %q", r.Role, r.RoleARN, want))
		}
		arns[r.RoleARN] = true
		if len(r.UsedBy) == 0 || strings.TrimSpace(r.Why) == "" {
			out = append(out, r.Role+": used_by and why are required")
		}
		if strings.Contains(string(r.Trust), forbiddenAccount) || r.Account == forbiddenAccount {
			out = append(out, r.Role+": names "+forbiddenAccount+", which no spore.host trust may")
		}
		stmts, err := ParsePolicy(r.Trust)
		if err != nil {
			out = append(out, r.Role+": "+err.Error())
			continue
		}
		out = append(out, TrustRules(r, stmts)...)
	}
	if len(sk.Signers) == 0 {
		out = append(out, "signing-key.json: no signers")
	}
	for _, s := range sk.Signers {
		if !arns[s] {
			out = append(out, "signing-key.json: signer "+s+" is not a role in trusts.json")
		}
	}
	if !regexp.MustCompile(`^arn:aws:kms:[a-z0-9-]+:` + regexp.QuoteMeta(sk.Account) + `:key/[0-9a-f-]{36}$`).MatchString(sk.KeyARN) {
		out = append(out, fmt.Sprintf("signing-key.json: key_arn %q is not a KMS key in %s", sk.KeyARN, sk.Account))
	}
	deny, err := ParsePolicy([]byte(`{"Statement":` + string(sk.RequiredDeny) + `}`))
	if err != nil || len(deny) != 1 {
		out = append(out, fmt.Sprintf("signing-key.json: required_deny is not one statement: %v", err))
	} else if !denyBindsSigners(deny[0], sk.Signers) {
		out = append(out, "signing-key.json: required_deny must Deny kms:Sign to every principal whose aws:PrincipalArn is not one of signers")
	}
	return out
}

// denyBindsSigners reports whether s denies kms:Sign to everyone but signers.
func denyBindsSigners(s Statement, signers []string) bool {
	if s.Effect != "Deny" || !actionMatches(s.Action, "kms:Sign") || len(s.NotAction) > 0 {
		return false
	}
	if !everyone(s.Principal) {
		return false
	}
	if len(s.Condition) != 1 {
		return false
	}
	for op, kv := range s.Condition {
		if op != "ArnNotEquals" && op != "ArnNotLike" && op != "StringNotEquals" {
			return false
		}
		if len(kv) != 1 {
			return false
		}
		got := append([]string(nil), kv["aws:PrincipalArn"]...)
		want := append([]string(nil), signers...)
		sort.Strings(got)
		sort.Strings(want)
		return strings.Join(got, "\n") == strings.Join(want, "\n")
	}
	return false
}

// everyone reports whether a principal is "*" or {"AWS": "*"}.
func everyone(p map[string][]string) bool {
	if len(p) != 1 {
		return false
	}
	for k, v := range p {
		return (k == "*" || k == "AWS") && len(v) == 1 && v[0] == "*"
	}
	return false
}

// actionMatches reports whether any IAM action pattern in pats matches action
// (case-insensitively, * and ? as wildcards).
func actionMatches(pats []string, action string) bool {
	for _, p := range pats {
		re := "^" + strings.NewReplacer(`\*`, ".*", `\?`, ".").Replace(regexp.QuoteMeta(strings.ToLower(p))) + "$"
		if regexp.MustCompile(re).MatchString(strings.ToLower(action)) {
			return true
		}
	}
	return false
}

// CompareRole compares `aws iam get-role` output for name with the inventory.
func CompareRole(inv Inventory, name string, getRole []byte) ([]string, error) {
	var want *Role
	for i := range inv.Roles {
		if inv.Roles[i].Role == name {
			want = &inv.Roles[i]
		}
	}
	if want == nil {
		return nil, fmt.Errorf("%s is not in trusts.json", name)
	}
	var out struct {
		Role struct {
			RoleName                 string
			Arn                      string
			AssumeRolePolicyDocument json.RawMessage
		}
	}
	if err := json.Unmarshal(getRole, &out); err != nil {
		return nil, err
	}
	if len(out.Role.AssumeRolePolicyDocument) == 0 {
		return nil, errors.New("no Role.AssumeRolePolicyDocument: pass `aws iam get-role --role-name NAME` output as is")
	}
	doc := out.Role.AssumeRolePolicyDocument
	var s string
	if json.Unmarshal(doc, &s) == nil {
		doc = []byte(s) // the raw API's URL-encoded string
	}
	live, err := ParsePolicy(doc)
	if err != nil {
		return nil, err
	}
	exp, err := ParsePolicy(want.Trust)
	if err != nil {
		return nil, err
	}
	var problems []string
	if out.Role.Arn != want.RoleARN {
		problems = append(problems, fmt.Sprintf("%s: live ARN %q, expected %q (the wrong account's credentials?)", name, out.Role.Arn, want.RoleARN))
	}
	problems = append(problems, diffStatements(name+" trust", exp, live)...)
	for _, p := range TrustRules(*want, live) {
		problems = append(problems, "live "+p)
	}
	if strings.Contains(string(doc), forbiddenAccount) {
		problems = append(problems, name+": the live trust names "+forbiddenAccount)
	}
	return problems, nil
}

func diffStatements(what string, want, got []Statement) []string {
	count := map[string]int{}
	for _, s := range want {
		count[s.canonical()]++
	}
	for _, s := range got {
		count[s.canonical()]--
	}
	var out []string
	keys := make([]string, 0, len(count))
	for k := range count {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch n := count[k]; {
		case n > 0:
			out = append(out, what+": expected statement missing live: "+k)
		case n < 0:
			out = append(out, what+": live statement not expected: "+k)
		}
	}
	return out
}

// CompareKeyPolicy checks `aws kms get-key-policy` output (the JSON with a
// "Policy" string, or the policy itself from --output text) against the
// signers: every Allow that reaches kms:Sign names only a signer, or the
// account root, and when it names the root the required Deny is present.
func CompareKeyPolicy(sk SigningKey, b []byte) ([]string, error) {
	var wrapped struct{ Policy string }
	if json.Unmarshal(b, &wrapped) == nil && wrapped.Policy != "" {
		b = []byte(wrapped.Policy)
	}
	stmts, err := ParsePolicy(b)
	if err != nil {
		return nil, err
	}
	signer := map[string]bool{}
	for _, s := range sk.Signers {
		signer[s] = true
	}
	root := map[string]bool{"arn:aws:iam::" + sk.Account + ":root": true, sk.Account: true}
	var problems []string
	delegated, denied := false, false
	for i, s := range stmts {
		id := s.Sid
		if id == "" {
			id = fmt.Sprintf("#%d", i)
		}
		reaches := actionMatches(s.Action, "kms:Sign") || (len(s.NotAction) > 0 && !actionMatches(s.NotAction, "kms:Sign"))
		if !reaches {
			continue
		}
		if s.Effect == "Deny" {
			denied = denied || denyBindsSigners(s, sk.Signers)
			continue
		}
		if len(s.Condition) > 0 {
			problems = append(problems, fmt.Sprintf("statement %s allows kms:Sign under a condition %v; read it by hand", id, s.Condition))
		}
		for kind, ps := range s.Principal {
			for _, p := range ps {
				switch {
				case kind == "AWS" && signer[p]:
				case kind == "AWS" && root[p]:
					delegated = true
				default:
					problems = append(problems, fmt.Sprintf("statement %s allows kms:Sign to %s %s, which is not a signer in signing-key.json", id, kind, p))
				}
			}
		}
	}
	if delegated && !denied {
		problems = append(problems, "the key policy delegates kms:Sign to IAM in "+sk.Account+
			" (the account root principal) without the required Deny, so any principal there whose identity policy allows kms:Sign or kms:* can sign spored; add signing-key.json's required_deny")
	}
	if strings.Contains(string(b), forbiddenAccount) {
		problems = append(problems, "the key policy names "+forbiddenAccount)
	}
	return problems, nil
}

// Ruleset is the part of a repository ruleset (GET /repos/{o}/{r}/rulesets/{id}) this reads.
type Ruleset struct {
	Name        string `json:"name"`
	Target      string `json:"target"`
	Enforcement string `json:"enforcement"`
	Conditions  struct {
		RefName struct {
			Include []string `json:"include"`
			Exclude []string `json:"exclude"`
		} `json:"ref_name"`
	} `json:"conditions"`
	Rules []struct {
		Type string `json:"type"`
	} `json:"rules"`
	BypassActors []struct {
		ActorType  string `json:"actor_type"`
		ActorID    *int   `json:"actor_id"`
		BypassMode string `json:"bypass_mode"`
	} `json:"bypass_actors"`
}

// CheckTagRulesets requires an active tag ruleset covering refs/tags/v* that
// blocks creation, update and deletion, so a v* tag (and with it the release
// role and kms:Sign) is reachable only by the ruleset's bypass list.
func CheckTagRulesets(b []byte) ([]string, error) {
	var rs []Ruleset
	if err := json.Unmarshal(b, &rs); err != nil {
		return nil, fmt.Errorf("want a JSON array of rulesets (jq -s): %w", err)
	}
	for _, r := range rs {
		if r.Target != "tag" || r.Enforcement != "active" {
			continue
		}
		covers := false
		for _, inc := range r.Conditions.RefName.Include {
			covers = covers || inc == "refs/tags/v*" || inc == "~ALL"
		}
		for _, exc := range r.Conditions.RefName.Exclude {
			if strings.HasPrefix(exc, "refs/tags/v") || exc == "~ALL" {
				covers = false
			}
		}
		rules := map[string]bool{}
		for _, rule := range r.Rules {
			rules[rule.Type] = true
		}
		if covers && rules["creation"] && rules["update"] && rules["deletion"] {
			var bypass []string
			for _, a := range r.BypassActors {
				id := "-"
				if a.ActorID != nil {
					id = fmt.Sprint(*a.ActorID)
				}
				bypass = append(bypass, a.ActorType+":"+id+"("+a.BypassMode+")")
			}
			fmt.Fprintf(os.Stderr, "ruleset %q protects v* tags; who may still create one: %v\n", r.Name, bypass)
			return nil, nil
		}
	}
	return []string{"no active tag ruleset covers refs/tags/v* with creation, update and deletion rules: anyone with write access to spawn can push a v* tag, run release.yaml, assume GitHubActions-spore-host-release and kms:Sign"}, nil
}
