package infrastructure

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every Lambda function name hardcoded in Go source must be a name some deploy
// mechanism in this repo actually creates.
//
// #790: pkg/infrastructure.Resolver constructed `spawn-scheduler-handler` from
// the naming convention. The deployed function is `scheduler-handler` —
// scripts/deploy-scheduler-handler.sh defaults to
// `SPAWN_LAMBDA_NAME:-scheduler-handler` — so `spawn validate --infrastructure`,
// the command whose entire job is reporting whether the control plane is
// healthy, reported a live function as missing. One of its two errors was false,
// which means the reader has to re-derive which half to trust, and a health
// check nobody trusts is not a health check.
//
// This is the "discover the set, don't list it" rule in CLAUDE.md applied to the
// gate itself: the expected names are read out of the deploy scripts and
// templates rather than written down here a second time, because a second copy
// of the naming convention would drift exactly the way the first one did.
func TestEveryHardcodedLambdaNameIsCreatedByADeployMechanism(t *testing.T) {
	root := repoRoot(t)

	created := lambdaNamesCreatedByDeployMechanisms(t, root)
	if len(created) == 0 {
		// Refusing to pass vacuously. If discovery finds nothing, the discovery
		// is broken; it has not proven the names correct.
		t.Fatal("found no Lambda function names in any deploy script or template — the discovery is broken")
	}

	used := lambdaNamesHardcodedInGoSource(t, root)
	if len(used) == 0 {
		t.Fatal("found no hardcoded Lambda function names in Go source — the discovery is broken")
	}

	for name, where := range used {
		if undeployedFunctions[name] != "" {
			continue
		}
		if !createdBySomething(name, created) {
			t.Errorf("%s hardcodes Lambda function %q, which no deploy script or template in this repo creates.\n"+
				"Known created names: %s\n"+
				"Either the name is wrong, or add it to undeployedFunctions with the issue that tracks deploying it.",
				where, name, strings.Join(sortedKeys(created), ", "))
		}
	}
}

// undeployedFunctions are names that are deliberately referenced while nothing
// deploys them, each with the issue tracking the decision.
//
// An exception list is the thing this gate exists to avoid, so it is kept to
// names with an open issue rather than a convenience. One entry today.
var undeployedFunctions = map[string]string{
	alertHandlerFunction: "#783 — builds in CI, deployed nowhere; `spawn alerts` cannot fire until it is",
}

// The resolver's own four names must be covered by the scan above. Without this,
// renaming a constant to something the scan happens not to find would silently
// shrink the gate's coverage to nothing.
func TestResolverLambdaNamesAreCoveredByTheScan(t *testing.T) {
	root := repoRoot(t)
	used := lambdaNamesHardcodedInGoSource(t, root)

	for _, name := range []string{
		schedulerHandlerFunction,
		sweepOrchestratorFunction,
		alertHandlerFunction,
		dashboardAPIFunction,
	} {
		if _, ok := used[name]; !ok {
			t.Errorf("the scan did not find resolver function name %q in Go source; "+
				"TestEveryHardcodedLambdaNameIsCreatedByADeployMechanism is not checking it", name)
		}
	}
}

// lambdaNamesCreatedByDeployMechanisms reads the names out of the shell scripts
// and CloudFormation/SAM templates that create functions.
func lambdaNamesCreatedByDeployMechanisms(t *testing.T, root string) map[string]string {
	t.Helper()
	created := map[string]string{}

	// FUNCTION_NAME="..." in any shell script, anywhere in the repo. Deploy
	// scripts live in both scripts/ and lambda/<fn>/ (and lambda/<fn>/scripts/),
	// which a glob of one directory would miss — spawn-dashboard-api is only
	// created by lambda/dashboard-api/deploy.sh.
	shellAssign := regexp.MustCompile(`(?m)^[[:space:]]*FUNCTION_NAME="([^"]+)"`)
	// FunctionName: in a template, optionally behind !Sub.
	tmplAssign := regexp.MustCompile(`(?m)^[[:space:]]*FunctionName:[[:space:]]*(?:!Sub[[:space:]]+)?['"]?([A-Za-z0-9._${}-]+)['"]?`)

	walkRepoFiles(t, root, func(path, rel string) {
		switch {
		case strings.HasSuffix(path, ".sh"):
			for _, m := range shellAssign.FindAllStringSubmatch(readStripped(t, path, "#"), -1) {
				if n := shellDefault(m[1]); n != "" {
					created[n] = rel
				}
			}
		case strings.HasSuffix(path, ".yaml"), strings.HasSuffix(path, ".yml"):
			for _, m := range tmplAssign.FindAllStringSubmatch(readStripped(t, path, "#"), -1) {
				n := m[1]
				// !Ref to another resource is a reference, not a name.
				if strings.HasPrefix(n, "!") || !strings.ContainsAny(n, "-") {
					continue
				}
				created[n] = rel
			}
		}
	})
	return created
}

// lambdaNamesHardcodedInGoSource finds `:function:<literal>` in non-test Go
// source, which is how every one of these names reaches AWS.
func lambdaNamesHardcodedInGoSource(t *testing.T, root string) map[string]string {
	t.Helper()
	used := map[string]string{}

	// The literal form only. `:function:%s` is a format string whose name comes
	// from a variable or a constant, and the constants are checked through their
	// own call sites.
	lit := regexp.MustCompile(`:function:([a-z0-9][a-z0-9-]*)`)
	// A bare constant declaration, so the resolver's names are seen even though
	// its Sprintf uses %s.
	constDecl := regexp.MustCompile(`(?m)^[[:space:]]*[A-Za-z_][A-Za-z0-9_]*Function[[:space:]]*=[[:space:]]*"([a-z0-9][a-z0-9-]*)"`)

	walkRepoFiles(t, root, func(path, rel string) {
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return
		}
		// Comments stripped: these files explain the bug this gate guards
		// ("the deployed function is `scheduler-handler`, not
		// `spawn-scheduler-handler`"), and scanning raw text would flag the
		// explanation as the defect. That exact mistake has been made four
		// times in this repo — hence the rule in CLAUDE.md to match the
		// assignment, not the bare name.
		src := readStripped(t, path, "//")
		for _, m := range lit.FindAllStringSubmatch(src, -1) {
			used[m[1]] = rel
		}
		for _, m := range constDecl.FindAllStringSubmatch(src, -1) {
			used[m[1]] = rel
		}
	})
	return used
}

// createdBySomething allows a template name carrying a substitution
// (`spawn-ttl-reaper-${Environment}`) to match by its literal prefix.
func createdBySomething(name string, created map[string]string) bool {
	if _, ok := created[name]; ok {
		return true
	}
	for c := range created {
		if i := strings.Index(c, "${"); i > 0 && strings.HasPrefix(name, c[:i]) {
			return true
		}
	}
	return false
}

// shellDefault resolves ${VAR:-default} to its default, and rejects a value that
// is purely a variable reference ("$2"), which carries no name.
func shellDefault(v string) string {
	if strings.HasPrefix(v, "${") && strings.Contains(v, ":-") && strings.HasSuffix(v, "}") {
		return strings.TrimSuffix(strings.SplitN(v, ":-", 2)[1], "}")
	}
	if strings.ContainsAny(v, "$") {
		return ""
	}
	return v
}

func readStripped(t *testing.T, path, commentPrefix string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // repo-relative path from a test walk
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out strings.Builder
	for _, line := range strings.Split(string(b), "\n") {
		if i := strings.Index(line, commentPrefix); i >= 0 {
			line = line[:i]
		}
		out.WriteString(line)
		out.WriteString("\n")
	}
	return out.String()
}

func walkRepoFiles(t *testing.T, root string, fn func(path, rel string)) {
	t.Helper()
	skip := map[string]bool{".git": true, "node_modules": true, "docs-gen": true, "vendor": true, "web": true}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skip[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		fn(path, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}

// repoRoot walks up until it finds the root go.mod, so the test does not depend
// on how deep the package sits.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if b, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil &&
			strings.Contains(string(b), "module github.com/spore-host/spawn\n") {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not find the repo root go.mod")
	return ""
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}
