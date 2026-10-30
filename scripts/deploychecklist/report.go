package main

import (
	"bytes"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
)

// The JSON is a contract with #1445, which will auto-deploy only Lambdas with
// status "changed" and deploy_kind "code-only". Bump SchemaVersion on any
// incompatible change.
const SchemaVersion = 1

const (
	StatusChanged   = "changed"
	StatusUnchanged = "unchanged"
	StatusAdded     = "added"   // discovered at --to only: a first deploy
	StatusRemoved   = "removed" // discovered at --from only: a teardown

	KindCodeOnly = "code-only" // only compiled inputs changed
	KindManual   = "manual"    // anything else changed
	KindNone     = "none"      // nothing changed

	ClassCompiled  = "compiled"     // a repo file compiled into the binary
	ClassModule    = "module"       // a linked third-party module version
	ClassGoVersion = "go-directive" // the main module's go directive
	ClassMechanism = "mechanism"    // the deploy script, template, Makefile or a file they read
)

// Change is one input that differs between the two refs.
type Change struct {
	Path   string `json:"path"`
	Class  string `json:"class"`
	Change string `json:"change"` // modified, added, removed
	Detail string `json:"detail,omitempty"`
}

type LambdaResult struct {
	LambdaComponent
	Status     string   `json:"status"`
	DeployKind string   `json:"deploy_kind"`
	Reasons    []string `json:"reasons"`
	Changes    []Change `json:"changes"`
}

type InputResult struct {
	DeployInput
	Status     string `json:"status"`
	DeployKind string `json:"deploy_kind"`
}

type Report struct {
	Schema     int            `json:"schema"`
	From       string         `json:"from"`
	To         string         `json:"to"`
	FromCommit string         `json:"from_commit"`
	ToCommit   string         `json:"to_commit"`
	Lambdas    []LambdaResult `json:"lambdas"`
	Inputs     []InputResult  `json:"deploy_inputs"`
	CLIDeploys []CLIDeploy    `json:"cli_deploys"`
}

// compileFunc computes a Lambda's compiled inputs in a tree; a seam for tests.
type compileFunc func(t *Tree, c *LambdaComponent) (*Compiled, error)

func goListCompile(t *Tree, c *LambdaComponent) (*Compiled, error) {
	return goList(t.Root, c.ID, c.Build)
}

func sameFile(a, b *Tree, rel string) (bool, error) {
	x, err := a.read(rel)
	if err != nil {
		return false, err
	}
	y, err := b.read(rel)
	if err != nil {
		return false, err
	}
	return bytes.Equal(x, y), nil
}

// diffFiles compares a set of repo files between the trees. A file in only one
// set is added or removed; one in both is compared by content.
func diffFiles(from, to *Tree, fromSet, toSet []string, class string) ([]Change, error) {
	inFrom, inTo := map[string]bool{}, map[string]bool{}
	for _, f := range fromSet {
		inFrom[f] = true
	}
	for _, f := range toSet {
		inTo[f] = true
	}
	var out []Change
	for _, f := range uniqSorted(append(append([]string{}, fromSet...), toSet...)) {
		switch {
		case inFrom[f] && inTo[f]:
			same, err := sameFile(from, to, f)
			if err != nil {
				return nil, err
			}
			if !same {
				out = append(out, Change{Path: f, Class: class, Change: "modified"})
			}
		case inTo[f]:
			out = append(out, Change{Path: f, Class: class, Change: "added"})
		default:
			out = append(out, Change{Path: f, Class: class, Change: "removed"})
		}
	}
	return out, nil
}

func mechanismFiles(c *LambdaComponent) []string {
	var out []string
	for _, m := range c.Mechanisms {
		out = append(out, m.Files...)
	}
	return uniqSorted(out)
}

func compareLambda(from, to *Tree, a, b *LambdaComponent, compile compileFunc) (LambdaResult, error) {
	r := LambdaResult{LambdaComponent: *b}
	ca, err := compile(from, a)
	if err != nil {
		return r, err
	}
	cb, err := compile(to, b)
	if err != nil {
		return r, err
	}
	compiled, err := diffFiles(from, to, ca.Files, cb.Files, ClassCompiled)
	if err != nil {
		return r, err
	}
	r.Changes = append(r.Changes, compiled...)
	for _, m := range uniqSorted(append(keys(ca.Modules), keys(cb.Modules)...)) {
		va, okA := ca.Modules[m]
		vb, okB := cb.Modules[m]
		switch {
		case okA && okB && va != vb:
			r.Changes = append(r.Changes, Change{Path: m, Class: ClassModule, Change: "modified", Detail: va + " -> " + vb})
		case okB && !okA:
			r.Changes = append(r.Changes, Change{Path: m, Class: ClassModule, Change: "added", Detail: vb})
		case okA && !okB:
			r.Changes = append(r.Changes, Change{Path: m, Class: ClassModule, Change: "removed", Detail: va})
		}
	}
	if ca.GoVersion != cb.GoVersion {
		r.Changes = append(r.Changes, Change{Path: "go.mod", Class: ClassGoVersion, Change: "modified", Detail: ca.GoVersion + " -> " + cb.GoVersion})
	}
	mech, err := diffFiles(from, to, mechanismFiles(a), mechanismFiles(b), ClassMechanism)
	if err != nil {
		return r, err
	}
	r.Changes = append(r.Changes, mech...)

	r.Status, r.DeployKind = StatusUnchanged, KindNone
	if len(r.Changes) > 0 {
		r.Status, r.DeployKind = StatusChanged, KindCodeOnly
		if len(mech) > 0 {
			r.DeployKind = KindManual
		}
	}
	r.Reasons = reasons(r.Changes)
	return r, nil
}

// reasons summarises changes for a reader: compiled files grouped by package
// directory, then modules, the go directive and mechanism files.
func reasons(changes []Change) []string {
	byDir := map[string][]string{}
	var out []string
	for _, c := range changes {
		switch c.Class {
		case ClassCompiled:
			name := path.Base(c.Path)
			if c.Change != "modified" {
				name += " (" + c.Change + ")"
			}
			byDir[path.Dir(c.Path)] = append(byDir[path.Dir(c.Path)], name)
		case ClassModule:
			out = append(out, fmt.Sprintf("links %s %s (%s)", c.Path, c.Detail, c.Change))
		case ClassGoVersion:
			out = append(out, "go directive "+c.Detail)
		case ClassMechanism:
			out = append(out, fmt.Sprintf("deploy mechanism %s %s", c.Path, c.Change))
		}
	}
	var compiled []string
	for _, d := range keys(byDir) {
		compiled = append(compiled, fmt.Sprintf("compiles %s: %s", d, strings.Join(byDir[d], ", ")))
	}
	return append(compiled, out...)
}

func buildReport(from, to *Tree, fromInv, toInv *Inventory, compile compileFunc) (*Report, error) {
	rep := &Report{Schema: SchemaVersion, CLIDeploys: toInv.CLIDeploys}
	for _, id := range uniqSorted(append(keys(fromInv.Lambdas), keys(toInv.Lambdas)...)) {
		a, b := fromInv.Lambdas[id], toInv.Lambdas[id]
		switch {
		case a != nil && b != nil:
			r, err := compareLambda(from, to, a, b, compile)
			if err != nil {
				return nil, err
			}
			rep.Lambdas = append(rep.Lambdas, r)
		case b != nil:
			rep.Lambdas = append(rep.Lambdas, LambdaResult{LambdaComponent: *b, Status: StatusAdded, DeployKind: KindManual,
				Reasons: []string{"new operated Lambda: its first deploy creates the function, so it is not a code update"}})
		default:
			rep.Lambdas = append(rep.Lambdas, LambdaResult{LambdaComponent: *a, Status: StatusRemoved, DeployKind: KindManual,
				Reasons: []string{"no mechanism deploys it at the new ref: tear the live function down by hand, with its role, log group and triggers"}})
		}
	}
	for _, p := range uniqSorted(append(keys(fromInv.Inputs), keys(toInv.Inputs)...)) {
		a, b := fromInv.Inputs[p], toInv.Inputs[p]
		r := InputResult{Status: StatusUnchanged, DeployKind: KindNone}
		switch {
		case a != nil && b != nil:
			r.DeployInput = *b
			// A file can be a deploy input at both refs; compare its bytes.
			same, err := sameFile(from, to, p)
			if err != nil {
				return nil, err
			}
			if !same {
				r.Status, r.DeployKind = StatusChanged, KindManual
			}
		case b != nil:
			r.DeployInput, r.Status, r.DeployKind = *b, StatusAdded, KindManual
		default:
			r.DeployInput, r.Status, r.DeployKind = *a, StatusRemoved, KindManual
			r.Command = "deleted: remove whatever it created if nothing else owns it"
		}
		rep.Inputs = append(rep.Inputs, r)
	}
	sort.Slice(rep.Lambdas, func(i, j int) bool { return rep.Lambdas[i].ID < rep.Lambdas[j].ID })
	return rep, nil
}

// writeMarkdown renders the checklist posted on the release PR.
func writeMarkdown(w io.Writer, r *Report) {
	p := func(format string, a ...any) { fmt.Fprintf(w, format, a...) }
	short := func(s string) string {
		if len(s) > 12 {
			return s[:12]
		}
		return s
	}
	p("## Deploy checklist: %s..%s\n\n", r.From, r.To)
	p("Operated components whose deploy inputs differ between `%s` (%s) and `%s` (%s), from `scripts/deploychecklist` (#1444). ", r.From, short(r.FromCommit), r.To, short(r.ToCommit))
	p("**code-only**: only compiled inputs changed, so a code update ships it. **manual**: a script, template, policy or mapping changed; review and apply it by hand. ")
	p("Deploy from a clean checkout of `%s`. How to read this: docs/infra-account.md, \"Deploy checklist\".\n\n", r.To)

	var changed, unchanged []LambdaResult
	for _, l := range r.Lambdas {
		if l.Status == StatusUnchanged {
			unchanged = append(unchanged, l)
		} else {
			changed = append(changed, l)
		}
	}
	p("### Lambdas (%d of %d need a deploy)\n\n", len(changed), len(r.Lambdas))
	if len(changed) == 0 {
		p("None.\n\n")
	}
	for _, l := range changed {
		fn := ""
		if len(l.Functions) > 0 {
			fn = " (`" + strings.Join(l.Functions, "`, `") + "`)"
		}
		p("- [ ] **%s**%s: %s, **%s**\n", l.ID, fn, l.Status, l.DeployKind)
		for _, why := range l.Reasons {
			p("  - %s\n", why)
		}
		for _, m := range l.Mechanisms {
			p("  - deploy (%s): `%s`\n", m.Kind, m.Command)
		}
	}
	if len(unchanged) > 0 {
		var ids []string
		for _, l := range unchanged {
			ids = append(ids, "`"+l.ID+"`")
		}
		p("\nUnchanged, no deploy: %s.\n", strings.Join(ids, ", "))
	}

	var ch []InputResult
	n := 0
	for _, in := range r.Inputs {
		n++
		if in.Status != StatusUnchanged {
			ch = append(ch, in)
		}
	}
	p("\n### Other deploy inputs (%d of %d changed, all manual)\n\n", len(ch), n)
	if len(ch) == 0 {
		p("None.\n")
	}
	for _, in := range ch {
		where := ""
		if in.Component != "" {
			where = " [" + in.Component + "]"
		}
		p("- [ ] `%s`%s: %s. %s\n  - %s\n", in.Path, where, in.Status, in.Why, commandLine(in.Command))
	}
	if len(r.CLIDeploys) > 0 {
		p("\n### Shipped with the CLI, no deploy\n\n")
		for _, c := range r.CLIDeploys {
			p("- `%s`: %s\n", c.Dir, c.Why)
		}
	}
}

func commandLine(c string) string {
	if strings.HasPrefix(c, "applied by") || strings.HasPrefix(c, "no script") || strings.HasPrefix(c, "deleted:") {
		return c
	}
	return "run: `" + c + "`"
}
