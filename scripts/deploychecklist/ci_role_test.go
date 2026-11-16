package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// ciRoleScript is the role GitHub Actions assumes for code-only deploys (#1445).
const ciRoleScript = "scripts/setup-lambda-deploy-ci-role.sh"

// ciRoleFunctions reads the FUNCTIONS=( ... ) array from the CI role script.
func ciRoleFunctions(t *testing.T, script string) []string {
	t.Helper()
	m := regexp.MustCompile(`(?ms)^FUNCTIONS=\((.*?)^\)`).FindStringSubmatch(script)
	if m == nil {
		t.Fatalf("%s has no FUNCTIONS=( ... ) array", ciRoleScript)
	}
	var fns []string
	for _, l := range strings.Split(m[1], "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			fns = append(fns, l)
		}
	}
	sort.Strings(fns)
	return fns
}

// TestCIDeployRoleCoversTheOperatedFunctions keeps the CI deploy role's
// resource list equal to the operated functions discover() finds. A function
// the role misses fails its first automated deploy; one it names that is no
// longer operated is a standing grant to replace code nothing deploys.
func TestCIDeployRoleCoversTheOperatedFunctions(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, ciRoleScript))
	if err != nil {
		t.Fatal(err)
	}
	tree, err := newTree(root)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := discover(tree)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, l := range inv.Lambdas {
		want = append(want, l.Functions...)
	}
	want = uniqSorted(want)
	got := ciRoleFunctions(t, string(raw))
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s FUNCTIONS = %v, but the operated functions are %v; update the array, then rerun the script against the infra account", ciRoleScript, got, want)
	}
}

// TestEveryOperatedComponentHasAServiceComponentID: the CI deployer tags each
// function spore-host:component=<the Lambda directory's name> (#750), and the
// shell deploy scripts accept only the ids scripts/lib/service-tags.sh lists.
// A new Lambda must be added there, or its two deploy paths would tag it
// differently.
func TestEveryOperatedComponentHasAServiceComponentID(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "scripts/lib/service-tags.sh"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^SPORE_SERVICE_COMPONENTS="([^"]*)"`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("scripts/lib/service-tags.sh has no SPORE_SERVICE_COMPONENTS assignment")
	}
	ids := map[string]bool{}
	for _, c := range strings.Fields(string(m[1])) {
		ids[c] = true
	}
	tree, err := newTree(root)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := discover(tree)
	if err != nil {
		t.Fatal(err)
	}
	for id := range inv.Lambdas {
		if c := filepath.Base(id); !ids[c] {
			t.Errorf("operated Lambda %s would be tagged spore-host:component=%s, which SPORE_SERVICE_COMPONENTS in scripts/lib/service-tags.sh does not list", id, c)
		}
	}
}
