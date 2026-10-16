package cmd

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// costLimitAction matches the lifecycle verbs a flag's help can use to say what
// spored does to the instance. Anchored on whole words so "stopped" (a state, e.g.
// "EBS bills while stopped") is not read as a claim about the action.
var costLimitAction = regexp.MustCompile(`(?i)\b(stop|stops|hibernate|hibernates|terminate|terminates)\b`)

// checkCostLimitActionText fails when text describes the cap's action as anything
// but termination. The enforcement is pkg/agent checkAndAct → a.terminate(...,
// ExitCostLimitExceeded), gated by TestCheckAndAct_CostLimitTerminatesOnEBS.
// Returns whether the text described an action at all.
func checkCostLimitActionText(t *testing.T, where, text string) bool {
	t.Helper()
	verbs := costLimitAction.FindAllString(text, -1)
	for _, v := range verbs {
		if !strings.HasPrefix(strings.ToLower(v), "terminate") {
			t.Errorf("%s says spored will %q at the cost limit, but the cap terminates the instance "+
				"(pkg/agent checkAndAct, #616/#959):\n  %s", where, v, text)
		}
	}
	return len(verbs) > 0
}

// TestCostLimitHelpSaysTerminate: every cost-limit flag's help that says what
// happens at the cap must say the instance is terminated (#959). `launch
// --cost-limit` and `extend --keep-cost-limit` both said "stop" for months after the
// enforcement terminated. Flags are discovered from the command tree, not listed.
func TestCostLimitHelpSaysTerminate(t *testing.T) {
	var flags, describing int
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		c.LocalFlags().VisitAll(func(f *pflag.Flag) {
			if !strings.Contains(f.Name, "cost-limit") {
				return
			}
			flags++
			if checkCostLimitActionText(t, c.CommandPath()+" --"+f.Name, f.Usage) {
				describing++
			}
		})
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(rootCmd)
	// A discovery that found nothing has broken, not proven the help clean.
	if flags == 0 || describing == 0 {
		t.Fatalf("found %d cost-limit flags, %d describing the cap's action — discovery is broken", flags, describing)
	}
}

// TestCostLimitDocsSayTerminate is the same rule over the committed reference in
// docs-gen/, which the docs site vendors: the help and the shipped docs must both
// agree with the enforcement action, and check-docs only proves they agree with
// each other.
func TestCostLimitDocsSayTerminate(t *testing.T) {
	dir := filepath.Join(repoRootFromCmd(t), "docs-gen")
	files, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no docs-gen fragments in %s (err=%v) — the gate is reading the wrong directory", dir, err)
	}
	row := regexp.MustCompile("^\\|\\s*`--[a-z-]*cost-limit[a-z-]*`")
	var rows, describing int
	for _, f := range files {
		b, err := os.ReadFile(f) //nolint:gosec // test reads committed repo files
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if !row.MatchString(line) {
				continue
			}
			rows++
			if checkCostLimitActionText(t, filepath.Base(f)+":"+strconv.Itoa(i+1), line) {
				describing++
			}
		}
	}
	if rows == 0 || describing == 0 {
		t.Fatalf("found %d cost-limit flag rows, %d describing the cap's action — discovery is broken", rows, describing)
	}
}
