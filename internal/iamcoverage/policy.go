package iamcoverage

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Policy is the Allow statements of one or more policy documents a principal
// holds, reduced to what a coverage check needs: which actions, by exact name
// or by wildcard.
type Policy struct {
	// Sources names where each part came from, for messages.
	Sources []string
	exact   map[string]string // lower-cased action -> source
	globs   map[string]string // lower-cased pattern -> source
	// denied are the actions or patterns an unconditional Deny on every
	// resource names: no Allow can grant them.
	denied map[string]string
	// passStmts are the identity statements, Allow and Deny, whose Action
	// covers iam:PassRole, kept whole: PassRole is resource-scoped, so whether
	// a role may be passed depends on Resource and Condition (#1397).
	passStmts []map[string]any
	// Opaque are AWS-managed policies attached by ARN. Their content is not in
	// this repository, so they grant nothing here; a call they would cover is
	// reported, with the policy named, rather than assumed.
	Opaque []string
}

// Grant is how a policy covers an action.
type Grant int

const (
	// NotGranted: no Allow statement names the action.
	NotGranted Grant = iota
	// WildcardOnly: only a pattern such as "*", "s3:*" or "ec2:Describe*"
	// matches. The gate does not accept this: a wildcard grant is satisfiable
	// by anything, so it cannot tell a deliberate grant from an accident, and
	// most of spawn's policies are least-privilege by design.
	WildcardOnly
	// Exact: an Allow statement names the action.
	Exact
	// Denied: an Allow names it, but an unconditional Deny on every resource
	// names it too, and a Deny beats an Allow.
	Denied
)

// NewPolicy returns an empty policy.
func NewPolicy() *Policy {
	return &Policy{exact: map[string]string{}, globs: map[string]string{}, denied: map[string]string{}}
}

// Merge adds other's grants to p.
func (p *Policy) Merge(other *Policy) *Policy {
	p.Sources = append(p.Sources, other.Sources...)
	for k, v := range other.exact {
		p.exact[k] = v
	}
	for k, v := range other.globs {
		p.globs[k] = v
	}
	for k, v := range other.denied {
		p.denied[k] = v
	}
	p.passStmts = append(p.passStmts, other.passStmts...)
	p.Opaque = append(p.Opaque, other.Opaque...)
	return p
}

// Grants reports how p covers action, and the source that does.
func (p *Policy) Grants(action string) (Grant, string) {
	a := strings.ToLower(action)
	grant, src := p.allowed(a)
	if grant == NotGranted {
		return grant, src
	}
	for d, dsrc := range p.denied {
		if ok, _ := path.Match(d, a); ok {
			return Denied, dsrc
		}
	}
	return grant, src
}

// allowed is how p's Allow statements alone cover the lower-cased action a.
func (p *Policy) allowed(a string) (Grant, string) {
	if src, ok := p.exact[a]; ok {
		return Exact, src
	}
	for g, src := range p.globs {
		if ok, _ := path.Match(g, a); ok {
			return WildcardOnly, src
		}
	}
	return NotGranted, ""
}

// Actions returns every exactly-named action, sorted.
func (p *Policy) Actions() []string {
	out := make([]string, 0, len(p.exact))
	for a := range p.exact {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

func (p *Policy) addAction(action, source string) {
	a := strings.ToLower(strings.TrimSpace(action))
	if a == "" {
		return
	}
	if strings.ContainsAny(a, "*?") {
		p.globs[a] = source
		return
	}
	p.exact[a] = source
}

// AddManaged records an AWS-managed policy attached by ARN.
func (p *Policy) AddManaged(arn string) *Policy {
	p.Opaque = append(p.Opaque, arn)
	return p
}

// ParsePolicyJSON reads one IAM policy document.
func ParsePolicyJSON(source, doc string) (*Policy, error) {
	var parsed struct {
		Statement json.RawMessage `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(doc), &parsed); err != nil {
		return nil, fmt.Errorf("%s: not a policy document: %w", source, err)
	}
	var stmts []map[string]any
	if err := json.Unmarshal(parsed.Statement, &stmts); err != nil {
		var one map[string]any
		if err2 := json.Unmarshal(parsed.Statement, &one); err2 != nil {
			return nil, fmt.Errorf("%s: Statement is neither a list nor an object: %w", source, err)
		}
		stmts = []map[string]any{one}
	}
	p := NewPolicy()
	p.Sources = []string{source}
	if err := p.addStatements(source, stmts); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *Policy) addStatements(source string, stmts []map[string]any) error {
	if len(stmts) == 0 {
		return fmt.Errorf("%s: policy has no statements", source)
	}
	for _, st := range stmts {
		if _, ok := st["Principal"]; !ok && coversPassRole(st) {
			p.passStmts = append(p.passStmts, st)
		}
		if st["Effect"] == "Deny" && unconditionalOnEverything(st) {
			for _, a := range stringList(st["Action"]) {
				if a = strings.ToLower(strings.TrimSpace(a)); a != "" {
					p.denied[a] = source + " (Deny)"
				}
			}
		}
		if st["Effect"] != "Allow" {
			continue // a Deny grants nothing; one scoped by resource or condition is not modelled here
		}
		if _, ok := st["NotAction"]; ok {
			// "everything except" is a wildcard by another name.
			p.globs["*"] = source + " (NotAction)"
			continue
		}
		if _, ok := st["Principal"]; ok {
			continue // a trust or resource policy, not an identity grant
		}
		for _, a := range stringList(st["Action"]) {
			p.addAction(a, source)
		}
	}
	return nil
}

// coversPassRole reports whether a statement's Action names iam:PassRole,
// exactly or by a wildcard.
func coversPassRole(st map[string]any) bool {
	for _, a := range stringList(st["Action"]) {
		if ok, _ := path.Match(strings.ToLower(a), "iam:passrole"); ok {
			return true
		}
	}
	return false
}

// unconditionalOnEverything reports whether a statement applies to every
// resource with no condition: Resource "*", no Condition, no NotResource, no
// NotAction.
func unconditionalOnEverything(st map[string]any) bool {
	for _, k := range []string{"Condition", "NotResource", "NotAction"} {
		if _, ok := st[k]; ok {
			return false
		}
	}
	for _, r := range stringList(st["Resource"]) {
		if r == "*" {
			return true
		}
	}
	return false
}

// PassStatements are p's identity statements whose Action covers iam:PassRole.
func (p *Policy) PassStatements() []map[string]any { return p.passStmts }

func stringList(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		var out []string
		for _, e := range t {
			out = append(out, stringList(e)...)
		}
		return out
	case []string:
		return t
	case map[string]any:
		// CloudFormation intrinsic inside an action list: Fn::If yields
		// either branch, so take both.
		if br, ok := t["Fn::If"].([]any); ok && len(br) == 3 {
			return append(stringList(br[1]), stringList(br[2])...)
		}
	}
	return nil
}

// heredoc matches a shell here-document body: <<EOF, <<'EOF', <<"EOF", <<-EOF.
var heredoc = regexp.MustCompile(`(?s)<<-?\s*['"]?([A-Z_]+)['"]?[^\n]*\n(.*?)\n\s*([A-Z_]+)\s*\n`)

// shellVar is a shell expansion inside a here-document.
var shellVar = regexp.MustCompile(`\$\{[^}]*\}|\$[A-Za-z_][A-Za-z0-9_]*`)

// ParseShellPolicies reads every identity-policy document written as a
// here-document in a deploy script, for the roles spore.host's scripts create
// with `aws iam put-role-policy` / `create-policy`. Trust policies (those with a
// Principal) are skipped. Shell expansions become a placeholder: only the
// actions matter here, and no action in these scripts is a variable.
func ParseShellPolicies(file string) (*Policy, error) {
	b, err := os.ReadFile(file) //nolint:gosec // the repository's own deploy scripts
	if err != nil {
		return nil, err
	}
	p := NewPolicy()
	p.Sources = []string{file}
	found := 0
	for _, m := range heredoc.FindAllStringSubmatch(string(b), -1) {
		if m[1] != m[3] {
			continue
		}
		body := substituteShellVars(m[2])
		if !strings.Contains(body, `"Statement"`) || strings.Contains(body, `"Principal"`) {
			continue
		}
		doc, err := ParsePolicyJSON(file, body)
		if err != nil {
			return nil, fmt.Errorf("%s: a here-document that looks like a policy does not parse: %w", file, err)
		}
		p.Merge(doc)
		found++
	}
	for _, m := range managedPolicyARN.FindAllStringSubmatch(string(b), -1) {
		p.AddManaged(m[1])
	}
	if found == 0 && len(p.Opaque) == 0 {
		return nil, fmt.Errorf("%s: found no identity-policy here-document or managed policy — has the script's shape changed?", file)
	}
	p.Sources = []string{file}
	return p, nil
}

// substituteShellVars replaces each shell expansion with a placeholder: X inside
// a JSON string, "X" outside one (a script may splice a whole JSON value in,
// as deploy-custom-dns.sh does with its zone ARN list).
func substituteShellVars(body string) string {
	var b strings.Builder
	inString := false
	for i := 0; i < len(body); {
		if loc := shellVar.FindStringIndex(body[i:]); loc != nil && loc[0] == 0 {
			if inString {
				b.WriteString("X")
			} else {
				b.WriteString(`"X"`)
			}
			i += loc[1]
			continue
		}
		c := body[i]
		if c == '"' && (i == 0 || body[i-1] != '\\') {
			inString = !inString
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}

var managedPolicyARN = regexp.MustCompile(`--policy-arn\s+"?(arn:aws:iam::aws:policy/[A-Za-z0-9/_+=,.@-]+)"?`)

// samPolicyTemplates are the SAM policy templates spawn's templates use, with
// the actions each expands to. Copied from samtranslator's
// policy_templates_data/policy_templates.json (aws-sam-cli 1.170.0), not from
// memory. A template that is not here is an error, not an empty grant.
var samPolicyTemplates = map[string][]string{
	"DynamoDBCrudPolicy": {
		"dynamodb:GetItem", "dynamodb:DeleteItem", "dynamodb:PutItem", "dynamodb:Scan",
		"dynamodb:Query", "dynamodb:UpdateItem", "dynamodb:BatchWriteItem",
		"dynamodb:BatchGetItem", "dynamodb:DescribeTable", "dynamodb:ConditionCheckItem",
	},
	"S3ReadPolicy": {
		"s3:GetObject", "s3:ListBucket", "s3:GetBucketLocation", "s3:GetObjectVersion",
		"s3:GetLifecycleConfiguration",
	},
	"S3CrudPolicy": {
		"s3:GetObject", "s3:ListBucket", "s3:GetBucketLocation", "s3:GetObjectVersion",
		"s3:PutObject", "s3:PutObjectAcl", "s3:GetLifecycleConfiguration",
		"s3:PutLifecycleConfiguration", "s3:DeleteObject",
	},
}

// ParseCFNPolicies reads the identity policies a CloudFormation or SAM template
// gives the named resources: for an AWS::IAM::Role, its Policies and
// ManagedPolicyArns plus any AWS::IAM::Policy / ManagedPolicy naming it in
// Roles; for an AWS::Serverless::Function, its Policies (inline documents, SAM
// policy templates and managed policy names). A resource that is not found, or
// yields no grants, is an error.
func ParseCFNPolicies(file string, logicalIDs ...string) (*Policy, error) {
	b, err := os.ReadFile(file) //nolint:gosec // the repository's own templates
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", file, err)
	}
	root, ok := cfnValue(&doc).(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s is not a mapping", file)
	}
	resources, _ := root["Resources"].(map[string]any)
	p := NewPolicy()
	for _, id := range logicalIDs {
		res, ok := resources[id].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s: no resource %q", file, id)
		}
		src := file + "#" + id
		props, _ := res["Properties"].(map[string]any)
		switch res["Type"] {
		case "AWS::IAM::Role":
			for _, pol := range asSlice(props["Policies"]) {
				pm, _ := pol.(map[string]any)
				if err := p.addCFNDocument(src, pm["PolicyDocument"]); err != nil {
					return nil, err
				}
			}
			for _, arn := range asSlice(props["ManagedPolicyArns"]) {
				if s, ok := arn.(string); ok {
					p.AddManaged(s)
				}
			}
			for otherID, other := range resources {
				om, _ := other.(map[string]any)
				if om["Type"] != "AWS::IAM::Policy" && om["Type"] != "AWS::IAM::ManagedPolicy" {
					continue
				}
				op, _ := om["Properties"].(map[string]any)
				for _, r := range asSlice(op["Roles"]) {
					if ref, ok := r.(map[string]any); ok && ref["Ref"] == id {
						if err := p.addCFNDocument(file+"#"+otherID, op["PolicyDocument"]); err != nil {
							return nil, err
						}
					}
				}
			}
		case "AWS::Serverless::Function":
			var pols []any
			for _, raw := range asSlice(props["Policies"]) {
				pols = append(pols, ifBranches(raw)...)
			}
			for _, pol := range pols {
				switch t := pol.(type) {
				case string:
					p.AddManaged(t)
				case map[string]any:
					if _, ok := t["Statement"]; ok {
						if err := p.addCFNDocument(src, t); err != nil {
							return nil, err
						}
						continue
					}
					for name := range t {
						acts, ok := samPolicyTemplates[name]
						if !ok {
							return nil, fmt.Errorf("%s: SAM policy template %q is not modelled (add it to samPolicyTemplates)", src, name)
						}
						for _, a := range acts {
							p.addAction(a, src+" ("+name+")")
						}
					}
				}
			}
		default:
			return nil, fmt.Errorf("%s: resource %q is %v, not a role or function", file, id, res["Type"])
		}
		p.Sources = append(p.Sources, src)
	}
	if len(p.exact)+len(p.globs) == 0 && len(p.Opaque) == 0 {
		return nil, fmt.Errorf("%s: resources %v grant nothing — has the template's shape changed?", file, logicalIDs)
	}
	return p, nil
}

func (p *Policy) addCFNDocument(src string, v any) error {
	// A whole document may sit behind Fn::If (a conditional policy).
	if m, ok := v.(map[string]any); ok {
		if br, ok := m["Fn::If"].([]any); ok && len(br) == 3 {
			for _, b := range br[1:] {
				if bm, ok := b.(map[string]any); ok && bm["Ref"] != "AWS::NoValue" {
					if err := p.addCFNDocument(src, b); err != nil {
						return err
					}
				}
			}
			return nil
		}
	}
	m, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("%s: PolicyDocument is not a mapping", src)
	}
	var stmts []map[string]any
	for _, s := range asSlice(m["Statement"]) {
		for _, st := range ifBranches(s) {
			if sm, ok := st.(map[string]any); ok {
				stmts = append(stmts, sm)
			}
		}
	}
	return p.addStatements(src, stmts)
}

// ifBranches expands a Fn::If statement into its non-NoValue branches: a
// conditional grant is a grant in the configuration that enables it.
func ifBranches(v any) []any {
	m, ok := v.(map[string]any)
	if !ok {
		return []any{v}
	}
	br, ok := m["Fn::If"].([]any)
	if !ok || len(br) != 3 {
		return []any{v}
	}
	var out []any
	for _, b := range br[1:] {
		if bm, ok := b.(map[string]any); ok && bm["Ref"] == "AWS::NoValue" {
			continue
		}
		out = append(out, ifBranches(b)...)
	}
	return out
}

func asSlice(v any) []any {
	if s, ok := v.([]any); ok {
		return s
	}
	if v == nil {
		return nil
	}
	return []any{v}
}

// cfnValue converts a yaml.v3 node to plain values, rendering CloudFormation
// short-form intrinsics (!If, !Sub, !Ref) as their long-form single-key maps, as
// pkg/reaperiam's template gate does.
func cfnValue(n *yaml.Node) any {
	switch n.Kind {
	case yaml.DocumentNode:
		return cfnValue(n.Content[0])
	case yaml.AliasNode:
		return cfnValue(n.Alias)
	}
	var v any
	switch n.Kind {
	case yaml.MappingNode:
		m := map[string]any{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			m[n.Content[i].Value] = cfnValue(n.Content[i+1])
		}
		v = m
	case yaml.SequenceNode:
		s := []any{}
		for _, c := range n.Content {
			s = append(s, cfnValue(c))
		}
		v = s
	default:
		v = n.Value
	}
	if strings.HasPrefix(n.Tag, "!") && !strings.HasPrefix(n.Tag, "!!") {
		name := strings.TrimPrefix(n.Tag, "!")
		if name != "Ref" && name != "Condition" {
			name = "Fn::" + name
		}
		return map[string]any{name: v}
	}
	return v
}
