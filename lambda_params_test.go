package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// paramNameRe matches a parameter name inside a SAM template's Parameters
// block: a 2-space-indented key with NOTHING after the colon.
//
// The end-of-line anchor is load-bearing, not decoration. A Conditions block can
// sit between Parameters and Resources, and its entries are indented
// identically — `  ScanSelfEnabled: !Equals [...]`. Requiring the line to end at
// the colon is what distinguishes a parameter declaration from a condition, and
// without it ttl-reaper's six conditions would be demanded of its Makefile.
var paramNameRe = regexp.MustCompile(`(?m)^  ([A-Z][A-Za-z0-9]*):$`)

// topLevelKeyRe matches a top-level YAML key, used to bound the Parameters
// block. Bounded by "the next top-level key" rather than by "Resources:",
// because Conditions/Globals/Transform may intervene and the bound must not
// depend on which.
var topLevelKeyRe = regexp.MustCompile(`(?m)^[A-Za-z][A-Za-z0-9]*:`)

// TestEveryLambdaTemplateParameterIsPassedByItsMakefile is spawn#776.
//
// A parameter declared in a template but never passed by the deploy takes its
// DEFAULT on every deploy, and an operator supplying it on the command line is
// ignored without a word. That is the whole failure: not a crash, a silent
// substitution.
//
// lambda/ttl-reaper had its own version of this check, and it caught me adding
// LogRetentionDays to three templates and wiring none of them (#770) — the
// default being 30 either way is why nothing looked wrong.
// autoscale-orchestrator and pipeline-orchestrator had NO tests at all, so when
// I added ScheduleState/ScheduleRate (#772) nothing would have noticed if I had
// forgotten. I happened to remember because that morning's failure was fresh,
// which is not a mechanism.
//
// ONE test over lambda/*/ rather than a copy per module, deliberately. Three
// copies drift, and more to the point a per-module check cannot cover the module
// nobody added one to — which is exactly how this gap existed. Same reasoning as
// scripts/lambda-deploy-census.sh (#654), whose first version hardcoded five
// function names in an account holding sixteen.
func TestEveryLambdaTemplateParameterIsPassedByItsMakefile(t *testing.T) {
	templates, err := filepath.Glob("lambda/*/template.yaml")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(templates) == 0 {
		t.Fatal("no lambda/*/template.yaml found — this gate is matching nothing")
	}

	checked := 0
	for _, tpl := range templates {
		dir := filepath.Dir(tpl)
		body, err := os.ReadFile(tpl)
		if err != nil {
			t.Fatalf("read %s: %v", tpl, err)
		}
		params := templateParameters(string(body))
		if len(params) == 0 {
			continue // a template with no parameters has nothing to pass
		}

		mkPath := filepath.Join(dir, "Makefile")
		mk, err := os.ReadFile(mkPath)
		if err != nil {
			// A template with parameters and no Makefile cannot pass them at all.
			t.Errorf("%s declares parameters %v but %s does not exist, so nothing passes them",
				tpl, params, mkPath)
			continue
		}
		recipe := stripMakefileComments(string(mk))
		checked++

		for _, p := range params {
			// Two deploy styles in this repo, both accepted:
			//   PARAM_MAP form   Version:VERSION
			//   inline override  Version=$(VERSION)
			// Matching the NAME alone would pass on any mention, including the
			// template's own Description text echoed into a comment — comments
			// are stripped above for that reason. Three gates in one session
			// passed against their own prose.
			if strings.Contains(recipe, p+":") || strings.Contains(recipe, p+"=") {
				continue
			}
			t.Errorf("%s: template parameter %s is never passed by %s.\n"+
				"    `make deploy %s=…` would be SILENTLY IGNORED and the template default "+
				"used instead.\n"+
				"    Add it to PARAM_MAP (ttl-reaper style) or to --parameter-overrides.",
				dir, p, mkPath, p)
		}
	}

	if checked == 0 {
		t.Fatal("no module with both a parameterised template and a Makefile was checked — " +
			"the gate ran but asserted nothing")
	}
}

// templateParameters returns the parameter names declared in a SAM template.
func templateParameters(body string) []string {
	start := strings.Index(body, "\nParameters:\n")
	if start < 0 {
		return nil
	}
	rest := body[start+len("\nParameters:\n"):]
	// Bound at the next top-level key so Conditions/Resources/Globals all end it.
	if loc := topLevelKeyRe.FindStringIndex(rest); loc != nil {
		rest = rest[:loc[0]]
	}
	var out []string
	for _, m := range paramNameRe.FindAllStringSubmatch(rest, -1) {
		out = append(out, m[1])
	}
	return out
}

// stripMakefileComments removes whole-line comments so a parameter named only in
// an explanatory comment does not satisfy the check.
func stripMakefileComments(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}
