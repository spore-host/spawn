package cmd

import (
	"os"
	"strings"
	"testing"
)

// `spawn alerts create` must consult the delivery check, and must do it BEFORE
// writing.
//
// #790: the command wrote to `spawn-alerts`, printed "Alert created" and
// returned 0, while the only reader of that table is deployed nowhere. The fix
// is a check — and a check that exists but is not called is the shape of #774's
// advertised-and-ignored `--region`, which is the same defect wearing a
// different hat.
//
// Order is load-bearing and not a stylistic preference. Checking after the write
// leaves a dead record behind AND returns an error: the worst of both, and
// indistinguishable from the fix in a test that only asserts "an error was
// returned".
func TestAlertsCreateChecksDeliverabilityBeforeWriting(t *testing.T) {
	body := functionBody(t, "cmd/alerts.go", "func runAlertsCreate(")

	check := strings.Index(body, "checkAlertsDeliverable(")
	if check < 0 {
		t.Fatal("runAlertsCreate does not call checkAlertsDeliverable; " +
			"`spawn alerts create` would accept alerts that cannot be delivered (#790)")
	}

	write := strings.Index(body, ".CreateAlert(")
	if write < 0 {
		t.Fatal("runAlertsCreate no longer calls CreateAlert — this gate is reading the wrong function")
	}

	if check > write {
		t.Errorf("runAlertsCreate calls checkAlertsDeliverable AFTER CreateAlert (offsets %d > %d); "+
			"refusing after the write leaves a dead record behind", check, write)
	}
}

// The check itself must consult the shared implementation rather than deciding
// locally, so the three-way Determined/Deliverable branch stays in one tested
// place.
func TestCheckAlertsDeliverableUsesTheSharedDecision(t *testing.T) {
	body := functionBody(t, "cmd/alerts.go", "func checkAlertsDeliverable(")

	for _, want := range []string{
		"infrastructure.CheckAlertDelivery(",
		".Decide()",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("checkAlertsDeliverable does not call %s; the refuse/warn decision must stay in "+
				"pkg/infrastructure where it is unit-tested", want)
		}
	}
}

// functionBody returns the source of one function with comments stripped,
// bounded by the next top-level func.
//
// Comments are stripped because these functions explain the defect they guard
// ("Refuse to create an alert nothing can deliver", "Deliberately BEFORE the
// write"), and a scan of the raw text would be satisfied by the prose — the
// failure mode recorded as rule 3 in CLAUDE.md, which this repo has hit four
// times.
func functionBody(t *testing.T, relPath, signature string) string {
	t.Helper()

	b, err := os.ReadFile(repoRootFromCmd(t) + "/" + relPath) //nolint:gosec // fixed repo-relative path
	if err != nil {
		t.Fatalf("read %s: %v", relPath, err)
	}

	var stripped strings.Builder
	for _, line := range strings.Split(string(b), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		stripped.WriteString(line)
		stripped.WriteString("\n")
	}
	src := stripped.String()

	start := strings.Index(src, signature)
	if start < 0 {
		t.Fatalf("%s: could not find %q — the gate is reading the wrong file or the function was renamed",
			relPath, signature)
	}
	rest := src[start+len(signature):]
	if end := strings.Index(rest, "\nfunc "); end >= 0 {
		return rest[:end]
	}
	return rest
}
