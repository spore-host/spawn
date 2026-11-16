package servicetags

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestForIsTheThreeServiceTags(t *testing.T) {
	got := For("ttl-reaper", "lambda/ttl-reaper/template.yaml")
	want := map[string]string{
		"spore-host:role":        "service",
		"spore-host:component":   "ttl-reaper",
		"spore-host:deployed-by": "lambda/ttl-reaper/template.yaml",
	}
	if len(got) != len(want) {
		t.Fatalf("For = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("For: %s = %q, want %q", k, got[k], v)
		}
	}
	if !IsService(got) {
		t.Error("IsService(For(...)) = false")
	}
	for _, tags := range []map[string]string{nil, {}, {"spawn:managed": "true"}, {"spore-host:role": "workload"}} {
		if IsService(tags) {
			t.Errorf("IsService(%v) = true", tags)
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if b, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil && strings.Contains(string(b), "module github.com/spore-host/spawn\n") {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find the repo root go.mod")
		}
		dir = parent
	}
}

// assigned reads a `NAME = "value"` (Go) or `NAME="value"` (shell) assignment,
// anchored at the start of a line, so a comment naming the key cannot stand in
// for the definition.
func assigned(t *testing.T, file, src, name string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(name) + `\s*=\s*"([^"]*)"`).FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("%s: no assignment of %s", file, name)
	}
	return m[1]
}

// The shell scripts and the CI deployer cannot import this package, so each
// has its own copy of the keys. This keeps the three in step.
func TestServiceTagKeysAgree(t *testing.T) {
	root := repoRoot(t)
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	const shell = "scripts/lib/service-tags.sh"
	const deployer = "scripts/deploylambdas/servicetags.go"
	sh, dl := read(shell), read(deployer)
	for _, c := range []struct{ want, shellName, deployerName string }{
		{RoleKey, "SPORE_HOST_ROLE_KEY", "serviceRoleKey"},
		{RoleService, "SPORE_HOST_ROLE_SERVICE", "serviceRoleValue"},
		{ComponentKey, "SPORE_HOST_COMPONENT_KEY", "serviceComponentKey"},
		{DeployedByKey, "SPORE_HOST_DEPLOYED_BY_KEY", "serviceDeployedByKey"},
	} {
		if got := assigned(t, shell, sh, c.shellName); got != c.want {
			t.Errorf("%s: %s = %q, want %q", shell, c.shellName, got, c.want)
		}
		if got := assigned(t, deployer, dl, c.deployerName); got != c.want {
			t.Errorf("%s: %s = %q, want %q", deployer, c.deployerName, got, c.want)
		}
	}
}

// The component ids decided in spore-host/spore-host#750, all of which the
// shell library must accept.
func TestShellLibraryAcceptsEveryComponent(t *testing.T) {
	root := repoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "scripts/lib/service-tags.sh"))
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, c := range strings.Fields(assigned(t, "scripts/lib/service-tags.sh", string(b), "SPORE_SERVICE_COMPONENTS")) {
		have[c] = true
	}
	for _, c := range []string{
		"dashboard-api", "dashboard-websocket", "dashboard-websocket-processor", "dns-updater",
		"pipeline-orchestrator", "sweep-orchestrator", "cost-history-collector", "scheduler-handler",
		"rest-api", "spore-bot", "ttl-reaper",
	} {
		if !have[c] {
			t.Errorf("SPORE_SERVICE_COMPONENTS lacks %q", c)
		}
	}
}
