package main

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// expectation is what the live function must already be for a code-only update
// to be safe, taken from the build and from the component's own deploy
// mechanisms at the tag, never from a list in this tool.
type expectation struct {
	Architecture string // from the checklist build's GOARCH
	Runtime      string // what the mechanisms declare
	Handler      string // what the mechanisms declare; must be the zip's `bootstrap`
	PackageType  string // Zip: every mechanism ships a zip
	// BodyCheck: a mechanism verifies the published spored bootstrap body
	// before it deploys (deploy-sweep-orchestrator.sh, #1368).
	BodyCheck bool
	// SAM: a mechanism is a SAM template, so a CloudFormation stack owns the
	// function and a direct code update drifts it.
	SAM bool
}

var (
	samRuntimeRe   = regexp.MustCompile(`(?m)^\s+Runtime:\s*['"]?([a-z][a-z0-9.]*)['"]?\s*$`)
	samHandlerRe   = regexp.MustCompile(`(?m)^\s+Handler:\s*['"]?([A-Za-z0-9_./-]+)['"]?\s*$`)
	shellRuntimeRe = regexp.MustCompile(`--runtime\s+("[^"]*"|'[^']*'|\S+)`)
	shellHandlerRe = regexp.MustCompile(`--handler\s+("[^"]*"|'[^']*'|\S+)`)
	// The body check in deploy-sweep-orchestrator.sh: `go run . bootstrap-body`
	// at a command position (after `(`, `$(`, `|` or line start), so a comment
	// or an echo naming it does not count.
	bodyCheckRe = regexp.MustCompile(`(?m)(?:^|[($|&;]\s*)(?:[A-Z_]+=\S+\s+)*go\s+run\b[^\n]*\bbootstrap-body\b`)
	assignRe    = regexp.MustCompile(`^\s*(?:export\s+|readonly\s+|local\s+)?([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)
)

// stripComments drops whole-line # comments, which in these scripts often
// explain the very call being looked for.
func stripComments(b []byte) string {
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if t := strings.TrimSpace(l); strings.HasPrefix(t, "#") || strings.HasPrefix(t, "@#") {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// shellValue resolves a word that is a literal, "$VAR" or "${VAR}", where VAR
// is assigned a literal or "${X:-default}" in the script.
func shellValue(word, code string) (string, bool) {
	word = strings.Trim(word, `"'`)
	if !strings.HasPrefix(word, "$") {
		return word, word != ""
	}
	name := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(word, "$"), "{"), "}")
	for _, l := range strings.Split(code, "\n") {
		m := assignRe.FindStringSubmatch(l)
		if m == nil || m[1] != name {
			continue
		}
		v := strings.Trim(strings.TrimSpace(m[2]), `"'`)
		if strings.HasPrefix(v, "${") && strings.HasSuffix(v, "}") {
			_, d, ok := strings.Cut(v[2:len(v)-1], ":-")
			if !ok {
				return "", false
			}
			v = strings.Trim(d, `"'`)
		}
		if v == "" || strings.Contains(v, "$") {
			return "", false
		}
		return v, true
	}
	return "", false
}

// expect derives the expectation for one Lambda from the tag checkout at repo.
func expect(repo string, l checklistLambda) (expectation, error) {
	arch, err := lambdaArch(l.Build.GOARCH)
	if err != nil {
		return expectation{}, err
	}
	e := expectation{Architecture: arch, PackageType: "Zip"}
	runtimes, handlers := map[string][]string{}, map[string][]string{}
	if len(l.Mechanisms) == 0 {
		return e, fmt.Errorf("%s: the checklist lists no deploy mechanism", l.ID)
	}
	for _, m := range l.Mechanisms {
		raw, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(path.Clean(m.Path))))
		if err != nil {
			return e, fmt.Errorf("%s: read its mechanism: %w", l.ID, err)
		}
		code := stripComments(raw)
		switch m.Kind {
		case "sam":
			e.SAM = true
			for _, x := range samRuntimeRe.FindAllStringSubmatch(code, -1) {
				runtimes[x[1]] = append(runtimes[x[1]], m.Path)
			}
			for _, x := range samHandlerRe.FindAllStringSubmatch(code, -1) {
				handlers[x[1]] = append(handlers[x[1]], m.Path)
			}
		case "shell":
			for _, x := range shellRuntimeRe.FindAllStringSubmatch(code, -1) {
				v, ok := shellValue(x[1], code)
				if !ok {
					return e, fmt.Errorf("%s: %s sets --runtime %s, which cannot be resolved", l.ID, m.Path, x[1])
				}
				runtimes[v] = append(runtimes[v], m.Path)
			}
			for _, x := range shellHandlerRe.FindAllStringSubmatch(code, -1) {
				v, ok := shellValue(x[1], code)
				if !ok {
					return e, fmt.Errorf("%s: %s sets --handler %s, which cannot be resolved", l.ID, m.Path, x[1])
				}
				handlers[v] = append(handlers[v], m.Path)
			}
			if bodyCheckRe.MatchString(code) {
				e.BodyCheck = true
			}
		default:
			return e, fmt.Errorf("%s: mechanism %s has kind %q, which this deployer does not know", l.ID, m.Path, m.Kind)
		}
	}
	one := func(what string, set map[string][]string) (string, error) {
		if len(set) != 1 {
			var all []string
			for v, where := range set {
				all = append(all, fmt.Sprintf("%s in %s", v, strings.Join(uniq(where), ", ")))
			}
			sort.Strings(all)
			return "", fmt.Errorf("%s: its mechanisms declare %d %ss (%s); need exactly one to check the live function against", l.ID, len(set), what, strings.Join(all, "; "))
		}
		for v := range set {
			return v, nil
		}
		return "", nil
	}
	if e.Runtime, err = one("runtime", runtimes); err != nil {
		return e, err
	}
	if e.Handler, err = one("handler", handlers); err != nil {
		return e, err
	}
	if !strings.HasPrefix(e.Runtime, "provided.") {
		return e, fmt.Errorf("%s: declared runtime %s is not a custom runtime, but the build produces a Go `bootstrap`", l.ID, e.Runtime)
	}
	if e.Handler != "bootstrap" {
		return e, fmt.Errorf("%s: declared handler %s, but the zip holds `bootstrap`", l.ID, e.Handler)
	}
	return e, nil
}

func uniq(s []string) []string {
	m := map[string]bool{}
	var out []string
	for _, x := range s {
		if !m[x] {
			m[x] = true
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}
