package plugin_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/spore-host/spawn/pkg/plugin"
)

// TestEnvPassthrough_OnlyNamedVarsReachSteps verifies that a local step sees an
// opted-in controller variable but NOT arbitrary parent env (e.g. a secret that
// wasn't listed) — the core of the local.env_passthrough contract.
func TestEnvPassthrough_OnlyNamedVarsReachSteps(t *testing.T) {
	t.Setenv("TS_API_CLIENT_SECRET", "mint-me")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "should-not-leak")

	out := filepath.Join(t.TempDir(), "env.txt")
	// Write the two vars (empty if unset) so we can assert what the step saw.
	step := plugin.Step{
		Type: "run",
		Run:  `printf 'passed=%s leaked=%s' "$TS_API_CLIENT_SECRET" "$AWS_SECRET_ACCESS_KEY" > ` + out,
	}

	exec := plugin.NewLocalExecutor(nil).WithEnvPassthrough([]string{"TS_API_CLIENT_SECRET"})
	if _, err := exec.RunProvision(context.Background(), "p", []plugin.Step{step}, plugin.NewTemplateContext()); err != nil {
		t.Fatalf("RunProvision: %v", err)
	}

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if string(got) != "passed=mint-me leaked=" {
		t.Errorf("step env = %q, want passed=mint-me leaked= (only the opted-in var should pass through)", string(got))
	}
}

// TestEnvPassthrough_UnsetVarIsSkipped confirms a listed-but-unset variable
// simply doesn't appear (no empty-string injection surprises, no error).
func TestEnvPassthrough_UnsetVarIsSkipped(t *testing.T) {
	os.Unsetenv("SPAWN_TEST_ABSENT_VAR")
	out := filepath.Join(t.TempDir(), "env.txt")
	step := plugin.Step{Type: "run", Run: `printf '%s' "${SPAWN_TEST_ABSENT_VAR:-UNSET}" > ` + out}

	exec := plugin.NewLocalExecutor(nil).WithEnvPassthrough([]string{"SPAWN_TEST_ABSENT_VAR"})
	if _, err := exec.RunProvision(context.Background(), "p", []plugin.Step{step}, plugin.NewTemplateContext()); err != nil {
		t.Fatalf("RunProvision: %v", err)
	}
	got, _ := os.ReadFile(out)
	if string(got) != "UNSET" {
		t.Errorf("got %q, want UNSET (an unset passthrough var must not be injected)", string(got))
	}
}

// TestEnvPassthrough_NoPassthroughByDefault verifies the default (no
// env_passthrough) still strips the parent environment.
func TestEnvPassthrough_NoPassthroughByDefault(t *testing.T) {
	t.Setenv("TS_API_CLIENT_SECRET", "mint-me")
	out := filepath.Join(t.TempDir(), "env.txt")
	step := plugin.Step{Type: "run", Run: `printf '%s' "${TS_API_CLIENT_SECRET:-STRIPPED}" > ` + out}

	exec := plugin.NewLocalExecutor(nil) // no WithEnvPassthrough
	if _, err := exec.RunProvision(context.Background(), "p", []plugin.Step{step}, plugin.NewTemplateContext()); err != nil {
		t.Fatalf("RunProvision: %v", err)
	}
	got, _ := os.ReadFile(out)
	if string(got) != "STRIPPED" {
		t.Errorf("got %q, want STRIPPED (parent env must not leak without opt-in)", string(got))
	}
}
