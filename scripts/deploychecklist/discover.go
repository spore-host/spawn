package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Discovery mirrors scripts/lambda-deploy-census.sh, which is the inventory of
// spore.host's operated deploy mechanisms (#654, #799, #1156). It is the SAME
// rule, not a second list: TestDiscoveryAgreesWithTheCensus runs the census and
// fails if the two disagree, so a mechanism the census learns to see and this
// tool does not (or the reverse) cannot ship. The three forms are:
//
//   - a SAM template, lambda/*/template.yaml, declaring AWS::Serverless::Function;
//   - a shell script anywhere in the repo that runs
//     `aws lambda create-function|update-function-code`;
//   - Go code calling the Lambda SDK's CreateFunction (pkg/reaperdeploy). That
//     one deploys the SELF-HOSTED reaper into a user's own account from the
//     CLI, so it ships with the CLI release and is reported as such, not as an
//     operated Lambda. (Not spelled as the call here: the census finds Go
//     deploys by a raw-text match that includes comments, and this comment
//     once made it report this package as a deploy path.)

var (
	// Exactly the census's shell-deploy rule: a raw-text match, so that the two
	// agree on what a deploy script is.
	shellDeployRe = regexp.MustCompile(`aws lambda (create-function|update-function-code)`)
	samFunctionRe = regexp.MustCompile(`AWS::Serverless::Function`)
	goDeployRe    = regexp.MustCompile(`\.CreateFunction\(`)

	// A CloudFormation or SAM template: the format-version key or the SAM
	// transform as a top-level YAML key or a JSON key. Anchored, so a script or
	// a document that only mentions the key is not a template.
	cfnTemplateRe = regexp.MustCompile(`(?m)^(AWSTemplateFormatVersion:|Transform:\s*['"]?AWS::Serverless|\s*"AWSTemplateFormatVersion"\s*:)`)

	// An AWS CLI call at a command position: line start, or after a shell
	// operator, `$(`, a Make recipe's `@`, or a compound-command keyword. This
	// is what keeps `echo "  aws lambda update-…"` hints and the census's own
	// grep patterns from counting as invocations (CLAUDE.md "Writing a gate",
	// rule 3). Global options (--profile X, --region X) may precede the service.
	awsInvocationRe = regexp.MustCompile(`(?:^|[;&|(@!]|\b(?:then|do|else|if|while|until|exec|time)\s)\s*aws\s+(?:--[a-z-]+(?:[= ](?:"[^"]*"|'[^']*'|[^\s-][^\s]*))?\s+)*([a-z0-9-]+)\s+([a-z0-9-]+)`)

	// A verb that changes control-plane state.
	mutatingVerbRe = regexp.MustCompile(`^(create|put|update|delete|attach|detach|tag|untag|add|remove|enable|disable|associate|disassociate|change|request|set|register|deregister|publish|deploy|import|modify|replace|subscribe|unsubscribe)(-|$)`)

	// DynamoDB item writes are data operations (backfills, cleanups), not
	// deploys: the table is the deploy input, a row is not.
	dynamoItemVerbRe = regexp.MustCompile(`^(put-item|update-item|delete-item|batch-write-item|transact-write-items|execute-statement|batch-execute-statement)$`)

	fileRefRe = regexp.MustCompile(`file://([A-Za-z0-9_./-]+)`)
	lambdaRef = regexp.MustCompile(`lambda/([a-z0-9][a-z0-9-]*)`)

	assignRe = regexp.MustCompile(`^\s*(?:export\s+|readonly\s+|local\s+)?([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)
	usageRe  = regexp.MustCompile(`(?i)^#\s*usage:\s*(.+)$`)
	// A script that changes to its own directory or computes paths from its
	// own location, so it can be run from anywhere.
	selfLocatingRe = regexp.MustCompile(`dirname|BASH_SOURCE`)
)

// scriptExts are the files whose AWS CLI calls are read. Makefiles are matched
// by name.
var scriptExts = map[string]bool{".sh": true, ".bash": true, ".py": true, ".ps1": true, ".mk": true}

// Tree is one checked-out revision: a directory and the repo-relative,
// slash-separated paths of the files in it.
type Tree struct {
	Root  string
	Files []string
	set   map[string]bool
	dirs  map[string]bool
}

func newTree(root string) (*Tree, error) {
	t := &Tree{Root: root, set: map[string]bool{}}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == ".git" || rel == "bin" {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		t.Files = append(t.Files, rel)
		t.set[rel] = true
		return nil
	})
	sort.Strings(t.Files)
	return t, err
}

func (t *Tree) has(rel string) bool { return t.set[rel] }

// exists is has, for a file or a directory.
func (t *Tree) exists(rel string) bool {
	rel = path.Clean(rel)
	if t.set[rel] {
		return true
	}
	if t.dirs == nil {
		t.dirs = map[string]bool{}
		for _, f := range t.Files {
			for d := path.Dir(f); d != "."; d = path.Dir(d) {
				t.dirs[d] = true
			}
		}
	}
	return t.dirs[rel]
}

func (t *Tree) read(rel string) ([]byte, error) {
	return os.ReadFile(filepath.Join(t.Root, filepath.FromSlash(rel)))
}

// Build is the go build a mechanism runs, parsed from its own `go build` line,
// so the file set is computed for the platform and tags actually shipped.
type Build struct {
	GOOS    string   `json:"goos"`
	GOARCH  string   `json:"goarch"`
	Tags    []string `json:"tags,omitempty"`
	Command string   `json:"command"`
}

// Mechanism is one way a Lambda is deployed.
type Mechanism struct {
	Path    string `json:"path"`
	Kind    string `json:"kind"` // "sam" or "shell"
	Command string `json:"command"`
	// Files are the mechanism's own deploy inputs: the script or template, the
	// Makefile beside a template, and any repo file they reference. A change to
	// any of them is a manual deploy.
	Files []string `json:"files"`
}

// LambdaComponent is one operated Lambda: a package directory and every
// mechanism that deploys it.
type LambdaComponent struct {
	ID         string      `json:"id"` // lambda/<name>
	Functions  []string    `json:"functions"`
	Build      Build       `json:"build"`
	Mechanisms []Mechanism `json:"mechanisms"`
}

// DeployInput is a non-code deploy input: a role, policy, table, stream,
// mapping or API script, a CloudFormation template, or a policy document a
// script applies.
type DeployInput struct {
	Path      string `json:"path"`
	Component string `json:"component,omitempty"` // lambda/<name> when it lives in one
	Why       string `json:"why"`
	Command   string `json:"command"`
}

// CLIDeploy is a Go deploy path: it ships inside the CLI release.
type CLIDeploy struct {
	Dir string `json:"dir"`
	Why string `json:"why"`
}

// Inventory is everything discovered in one tree.
type Inventory struct {
	Lambdas    map[string]*LambdaComponent
	Inputs     map[string]*DeployInput
	CLIDeploys []CLIDeploy
}

// stripComments drops whole-line `#` comments, which in these scripts often
// explain the very call a scan is looking for.
func stripComments(b []byte) string {
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		l := sc.Text()
		// `@#` is a silenced comment inside a Make recipe.
		if tl := strings.TrimSpace(l); strings.HasPrefix(tl, "#") || strings.HasPrefix(tl, "@#") {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// shellDefault resolves "${VAR:-default}" to default and strips quotes.
func shellDefault(v string) string {
	v = strings.TrimSpace(v)
	if i := strings.Index(v, " #"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	v = strings.Trim(v, `"'`)
	if strings.HasPrefix(v, "${") && strings.HasSuffix(v, "}") {
		inner := v[2 : len(v)-1]
		if i := strings.Index(inner, ":-"); i >= 0 {
			return strings.Trim(inner[i+2:], `"'`)
		}
		return ""
	}
	return v
}

func assignments(code string) map[string]string {
	m := map[string]string{}
	for _, l := range strings.Split(code, "\n") {
		if a := assignRe.FindStringSubmatch(l); a != nil {
			if _, seen := m[a[1]]; !seen {
				m[a[1]] = shellDefault(a[2])
			}
		}
	}
	return m
}

// resolve expands a value that is a single $VAR / ${VAR} reference.
func resolve(v string, vars map[string]string) (string, bool) {
	v = strings.Trim(v, `"'`)
	if !strings.HasPrefix(v, "$") {
		return v, v != ""
	}
	name := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(v, "$"), "{"), "}")
	r, ok := vars[name]
	if !ok || strings.Contains(r, "$") || r == "" {
		return "", false
	}
	return r, true
}

var (
	// `go build` as the command, after any VAR=value prefixes.
	goBuildRe = regexp.MustCompile(`^\s*@?(?:[A-Za-z_][A-Za-z0-9_]*=\S+\s+)*go\s+build\b`)
	goosRe    = regexp.MustCompile(`\bGOOS=("[^"]*"|\S+)`)
	goarchRe  = regexp.MustCompile(`\bGOARCH=("[^"]*"|\S+)`)
	tagsRe    = regexp.MustCompile(`\s-tags[= ]("[^"]*"|\S+)`)
)

// parseBuild reads the first `go build` line of a mechanism.
func parseBuild(file, code string) (Build, error) {
	vars := assignments(code)
	for _, l := range strings.Split(code, "\n") {
		if !goBuildRe.MatchString(l) {
			continue
		}
		b := Build{Command: strings.TrimSpace(strings.TrimLeft(l, "\t @"))}
		var ok bool
		if m := goosRe.FindStringSubmatch(l); m != nil {
			if b.GOOS, ok = resolve(m[1], vars); !ok {
				return b, fmt.Errorf("%s: cannot resolve GOOS=%s in %q", file, m[1], b.Command)
			}
		} else {
			return b, fmt.Errorf("%s: `go build` sets no GOOS: %q", file, b.Command)
		}
		if m := goarchRe.FindStringSubmatch(l); m != nil {
			if b.GOARCH, ok = resolve(m[1], vars); !ok {
				return b, fmt.Errorf("%s: cannot resolve GOARCH=%s in %q", file, m[1], b.Command)
			}
		} else {
			return b, fmt.Errorf("%s: `go build` sets no GOARCH: %q", file, b.Command)
		}
		if m := tagsRe.FindStringSubmatch(l); m != nil {
			tags, ok := resolve(m[1], vars)
			if !ok {
				return b, fmt.Errorf("%s: cannot resolve -tags %s", file, m[1])
			}
			b.Tags = strings.Split(tags, ",")
		}
		return b, nil
	}
	return Build{}, fmt.Errorf("%s: deploys a Lambda but has no `go build` line, so its compiled inputs cannot be computed", file)
}

// scriptCommand is how to run a script, with the arguments from its own
// `# Usage:` line. A script that locates itself (dirname "$0", BASH_SOURCE) runs
// from anywhere, so it is given by its repo path. Otherwise it runs from the
// directory its relative paths resolve from: its own (`go build .`, the usual
// case), its parent (lambda/dashboard-api/scripts/setup-all.sh runs
// ./scripts/...), or the repo root (deploy-custom-dns.sh runs cd lambda/...).
func scriptCommand(t *Tree, rel string, raw []byte) string {
	args := ""
	for _, l := range strings.Split(string(raw), "\n") {
		if m := usageRe.FindStringSubmatch(strings.TrimSpace(l)); m != nil {
			if f := strings.Fields(m[1]); len(f) > 1 {
				args = " " + strings.Join(f[1:], " ")
			}
			break
		}
	}
	if b := path.Base(rel); b == "Makefile" || b == "GNUmakefile" {
		return "make -C " + path.Dir(rel)
	}
	code := stripComments(raw)
	if selfLocatingRe.MatchString(code) {
		return rel + args
	}
	dir := path.Dir(rel)
	best, bestScore := dir, 0
	for _, base := range []string{dir, path.Dir(dir), "."} {
		score := 0
		for _, tok := range pathTokens(code) {
			if strings.Contains(tok, "/") && t.exists(path.Join(base, tok)) {
				score++
			}
		}
		if score > bestScore {
			best, bestScore = base, score
		}
	}
	if best == "." {
		return rel + args
	}
	r := strings.TrimPrefix(rel, best+"/")
	if !strings.Contains(r, "/") {
		r = "./" + r
	}
	return fmt.Sprintf("(cd %s && %s%s)", best, r, args)
}

func pathTokens(code string) []string {
	var out []string
	for _, tok := range strings.FieldsFunc(code, func(r rune) bool {
		return strings.ContainsRune(" \t\n\"'`=(){}[];|&<>,", r)
	}) {
		if strings.Contains(tok, "$") || strings.HasPrefix(tok, "/") || strings.Contains(tok, "://") || strings.HasPrefix(tok, "-") {
			continue
		}
		out = append(out, tok)
	}
	return out
}

// componentOf is lambda/<name> for a path inside a Lambda directory.
func componentOf(rel string) string {
	parts := strings.SplitN(rel, "/", 3)
	if len(parts) == 3 && parts[0] == "lambda" {
		return "lambda/" + parts[1]
	}
	return ""
}

func isScript(rel string) bool {
	b := path.Base(rel)
	return b == "Makefile" || b == "GNUmakefile" || scriptExts[path.Ext(rel)]
}

func excluded(rel string) bool {
	for _, p := range strings.Split(rel, "/") {
		if p == "testdata" {
			return true
		}
	}
	return strings.HasSuffix(strings.TrimSuffix(rel, path.Ext(rel)), "_test")
}

// workloadServices are AWS services whose resources here are WORKLOADS that a
// launch or a smoke test creates and the reaper tracks (instances, their EFS
// and FSx file systems), never operated control-plane state. A script that
// only creates and deletes these (scripts/hardware-smoke.sh) is not a deploy.
var workloadServices = map[string]bool{"ec2": true, "efs": true, "fsx": true}

// mutatingCalls lists the control-plane-changing AWS CLI calls in a script.
func mutatingCalls(code string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range strings.Split(code, "\n") {
		for _, m := range awsInvocationRe.FindAllStringSubmatch(l, -1) {
			svc, verb := m[1], m[2]
			if workloadServices[svc] || !mutatingVerbRe.MatchString(verb) {
				continue
			}
			if svc == "dynamodb" && dynamoItemVerbRe.MatchString(verb) {
				continue
			}
			c := "aws " + svc + " " + verb
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	sort.Strings(out)
	return out
}

// referencedFiles are the repo files a script or Makefile names: file://
// documents, and any path that resolves (from the repo root or from the file's
// own directory) to a file in the tree.
func referencedFiles(t *Tree, rel, code string) []string {
	seen := map[string]bool{}
	dir := path.Dir(rel)
	add := func(p string) {
		p = path.Clean(p)
		if p != rel && t.has(p) && !seen[p] {
			seen[p] = true
		}
	}
	for _, m := range fileRefRe.FindAllStringSubmatch(code, -1) {
		if !strings.HasPrefix(m[1], "/") {
			add(m[1])
			add(path.Join(dir, m[1]))
		}
	}
	for _, tok := range pathTokens(code) {
		if !strings.Contains(tok, ".") {
			continue
		}
		add(tok)
		add(path.Join(dir, tok))
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		if b := path.Base(p); !strings.HasSuffix(b, ".go") && !strings.HasSuffix(b, ".md") && b != "go.mod" && b != "go.sum" {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func samFunctionNames(raw []byte) []string {
	text := string(raw)
	env := ""
	if m := regexp.MustCompile(`(?m)^  Environment:\s*\n(?:\s{4}.*\n)*?\s{4}Default:\s*(\S+)`).FindStringSubmatch(text); m != nil {
		env = strings.Trim(m[1], `"'`)
	}
	var out []string
	for _, m := range regexp.MustCompile(`(?m)^\s+FunctionName:\s*(.+)$`).FindAllStringSubmatch(text, -1) {
		v := strings.TrimSpace(m[1])
		if strings.HasPrefix(v, "!Ref") || strings.HasPrefix(v, "!GetAtt") {
			continue // a reference to the function, not its name
		}
		v = strings.TrimSpace(strings.TrimPrefix(v, "!Sub"))
		v = strings.Trim(v, `"'`)
		if env != "" {
			v = strings.ReplaceAll(v, "${Environment}", env)
		}
		out = append(out, v)
	}
	return out
}

// discover builds the inventory of one tree.
//
// Order matters. Independent deploy inputs (scripts with a mutating
// control-plane call, CloudFormation templates) are found first, so that a
// deploy script which merely MENTIONS a role script ("run
// setup-dashboard-lambda-role.sh first") does not swallow it: the role script
// stays its own line item, and only the files a mechanism reads and nothing
// else deploys (a policy document, a parameter-merging helper) count as part of
// the mechanism.
func discover(t *Tree) (*Inventory, error) {
	inv := &Inventory{Lambdas: map[string]*LambdaComponent{}, Inputs: map[string]*DeployInput{}}
	var errs []string

	type mech struct {
		id   string
		fns  []string
		b    Build
		m    Mechanism
		code string
	}
	var mechs []mech
	primary := map[string]bool{} // the deploy scripts, templates and Makefiles themselves

	// SAM templates: lambda/*/template.yaml, as the census globs them.
	for _, rel := range t.Files {
		parts := strings.Split(rel, "/")
		if len(parts) != 3 || parts[0] != "lambda" || parts[2] != "template.yaml" {
			continue
		}
		raw, err := t.read(rel)
		if err != nil {
			return nil, err
		}
		if !samFunctionRe.Match(raw) {
			continue
		}
		dir := path.Dir(rel)
		mk := dir + "/Makefile"
		if !t.has(mk) {
			errs = append(errs, fmt.Sprintf("%s declares a function but %s has no Makefile, so there is no build to read", rel, dir))
			continue
		}
		mkRaw, err := t.read(mk)
		if err != nil {
			return nil, err
		}
		mkCode := stripComments(mkRaw)
		b, err := parseBuild(mk, mkCode)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		primary[rel], primary[mk] = true, true
		mechs = append(mechs, mech{id: dir, fns: samFunctionNames(raw), b: b, code: mkCode,
			m: Mechanism{Path: rel, Kind: "sam", Command: "make -C " + dir + " deploy", Files: []string{mk, rel}}})
	}

	// Shell deploys: any *.sh in the repo, as the census finds them.
	for _, rel := range t.Files {
		if path.Ext(rel) != ".sh" {
			continue
		}
		raw, err := t.read(rel)
		if err != nil {
			return nil, err
		}
		if !shellDeployRe.Match(raw) {
			continue
		}
		code := stripComments(raw)
		id := componentOf(rel)
		if id == "" {
			refs := map[string]bool{}
			for _, m := range lambdaRef.FindAllStringSubmatch(code, -1) {
				if hasGo(t, "lambda/"+m[1]) {
					refs["lambda/"+m[1]] = true
				}
			}
			if len(refs) != 1 {
				errs = append(errs, fmt.Sprintf("%s deploys a Lambda but names %d Lambda directories %v; cannot tell which package it ships", rel, len(refs), keys(refs)))
				continue
			}
			for k := range refs {
				id = k
			}
		}
		b, err := parseBuild(rel, code)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		var fns []string
		if v, ok := assignments(code)["FUNCTION_NAME"]; ok && v != "" {
			fns = append(fns, v)
		}
		primary[rel] = true
		mechs = append(mechs, mech{id: id, fns: fns, b: b, code: code,
			m: Mechanism{Path: rel, Kind: "shell", Command: scriptCommand(t, rel, raw), Files: []string{rel}}})
	}

	// Go deploys ship with the CLI.
	goDirs := map[string]bool{}
	for _, rel := range t.Files {
		if path.Ext(rel) != ".go" || strings.HasSuffix(rel, "_test.go") {
			continue
		}
		raw, err := t.read(rel)
		if err != nil {
			return nil, err
		}
		if goDeployRe.Match(raw) {
			goDirs[path.Dir(rel)] = true
		}
	}
	for _, d := range keys(goDirs) {
		inv.CLIDeploys = append(inv.CLIDeploys, CLIDeploy{Dir: d, Why: "creates a Lambda from Go inside the spawn CLI (the self-hosted reaper, in the user's own account), so it ships with the CLI release, not as a spore.host deploy"})
	}

	// Independent deploy inputs.
	scripts := map[string]string{} // rel -> comment-stripped code
	raws := map[string][]byte{}
	for _, rel := range t.Files {
		if excluded(rel) || primary[rel] {
			continue
		}
		if isScript(rel) {
			raw, err := t.read(rel)
			if err != nil {
				return nil, err
			}
			code := stripComments(raw)
			scripts[rel], raws[rel] = code, raw
			if calls := mutatingCalls(code); len(calls) > 0 {
				inv.Inputs[rel] = &DeployInput{Path: rel, Why: "runs " + strings.Join(calls, ", ")}
			}
			continue
		}
		switch path.Ext(rel) {
		case ".yaml", ".yml", ".json", ".template":
			raw, err := t.read(rel)
			if err != nil {
				return nil, err
			}
			if cfnTemplateRe.Match(raw) {
				inv.Inputs[rel] = &DeployInput{Path: rel, Why: "CloudFormation/SAM template"}
			}
		}
	}
	// Scripts that run a deploy-input script (setup-all.sh), to a fixed point.
	for changed := true; changed; {
		changed = false
		for _, rel := range keys(scripts) {
			if inv.Inputs[rel] != nil {
				continue
			}
			var runs []string
			for _, ref := range referencedFiles(t, rel, scripts[rel]) {
				if inv.Inputs[ref] != nil && isScript(ref) {
					runs = append(runs, ref)
				}
			}
			if len(runs) > 0 {
				inv.Inputs[rel] = &DeployInput{Path: rel, Why: "runs " + strings.Join(runs, ", ")}
				changed = true
			}
		}
	}
	// Documents a deploy-input script applies with file://.
	for _, rel := range keys(scripts) {
		if inv.Inputs[rel] == nil || !isScript(rel) {
			continue
		}
		for _, m := range fileRefRe.FindAllStringSubmatch(scripts[rel], -1) {
			for _, ref := range []string{path.Clean(m[1]), path.Join(path.Dir(rel), m[1])} {
				if !t.has(ref) || primary[ref] || inv.Inputs[ref] != nil {
					continue
				}
				inv.Inputs[ref] = &DeployInput{Path: ref, Why: "document applied by " + rel}
			}
		}
	}

	// Mechanisms: each one's own files plus what it reads that nothing else
	// deploys.
	for _, mc := range mechs {
		files := mc.m.Files
		for _, ref := range referencedFiles(t, mc.m.Files[0], mc.code) {
			if inv.Inputs[ref] == nil && !primary[ref] {
				files = append(files, ref)
			}
		}
		mc.m.Files = uniqSorted(files)
		c := inv.Lambdas[mc.id]
		if c == nil {
			c = &LambdaComponent{ID: mc.id, Build: mc.b}
			inv.Lambdas[mc.id] = c
		} else if c.Build.GOOS != mc.b.GOOS || c.Build.GOARCH != mc.b.GOARCH || strings.Join(c.Build.Tags, ",") != strings.Join(mc.b.Tags, ",") {
			// Two mechanisms building one package for different targets compile
			// different file sets; refuse rather than pick one.
			errs = append(errs, fmt.Sprintf("%s: %s builds %s/%s %v, but another mechanism builds %s/%s %v", mc.id, mc.m.Path, mc.b.GOOS, mc.b.GOARCH, mc.b.Tags, c.Build.GOOS, c.Build.GOARCH, c.Build.Tags))
		}
		for _, f := range mc.fns {
			if !contains(c.Functions, f) {
				c.Functions = append(c.Functions, f)
			}
		}
		sort.Strings(c.Functions)
		c.Mechanisms = append(c.Mechanisms, mc.m)
		sort.Slice(c.Mechanisms, func(i, j int) bool { return c.Mechanisms[i].Path < c.Mechanisms[j].Path })
	}

	// How each input is applied.
	for rel, in := range inv.Inputs {
		in.Component = componentOf(rel)
		if isScript(rel) {
			in.Command = scriptCommand(t, rel, raws[rel])
			continue
		}
		var by []string
		needle := path.Base(path.Dir(rel)) + "/" + path.Base(rel)
		for _, s := range keys(scripts) {
			if inv.Inputs[s] != nil && strings.Contains(scripts[s], needle) {
				by = append(by, s)
			}
		}
		for _, mc := range mechs {
			if strings.Contains(mc.code, needle) || strings.Contains(mc.code, "file://"+rel) {
				by = append(by, mc.m.Files[0])
			}
		}
		if by = uniqSorted(by); len(by) > 0 {
			in.Command = "applied by " + strings.Join(by, ", ")
		} else {
			in.Command = "no script in the repo applies it: deploy by hand"
		}
	}

	if len(errs) > 0 {
		sort.Strings(errs)
		return nil, fmt.Errorf("discovery in %s failed:\n  %s", t.Root, strings.Join(errs, "\n  "))
	}
	// A discovery that found nothing has broken, not proven the repo clean.
	if len(inv.Lambdas) == 0 {
		return nil, fmt.Errorf("discovery found no operated Lambda in %s: the discovery is broken, not the repo", t.Root)
	}
	return inv, nil
}

func hasGo(t *Tree, dir string) bool {
	for _, f := range t.Files {
		if path.Dir(f) == dir && strings.HasSuffix(f, ".go") {
			return true
		}
	}
	return false
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func uniqSorted(s []string) []string {
	m := map[string]bool{}
	for _, x := range s {
		m[x] = true
	}
	return keys(m)
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
