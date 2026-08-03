package cmd

import (
	"os"
	"strings"
	"testing"
)

// The formatting gate is wiring, not code: a Makefile target plus a CI step. Both
// halves are trivially deletable and their absence is silent — the tree just
// starts drifting again, and nobody notices until an unrelated PR picks up a
// reformatting diff. That is exactly how three files sat unformatted on main for
// months. These tests make removing the gate fail a test instead.

// TestFormatGateReportsRatherThanRewrites guards the distinction the gate rests
// on. `gofmt -w` mutates the tree and exits 0 — it can never fail a build. Only
// `gofmt -l`/`-d` reports. If check-fmt were "fixed" to write files, CI would go
// green on a dirty tree forever, which is indistinguishable from having no gate.
func TestFormatGateReportsRatherThanRewrites(t *testing.T) {
	body, ok := makeTarget(t, "check-fmt")
	if !ok {
		t.Fatal("the Makefile has no check-fmt target; CI's format gate calls it")
	}
	if !strings.Contains(body, "gofmt -l") {
		t.Error("check-fmt does not run 'gofmt -l'; it must LIST offenders to be able to fail")
	}
	// Look at commands only, not message text: the recipe's error message tells
	// the reader to "run 'gofmt -w' on them", which is advice, not an invocation.
	if strings.Contains(unquote(body), "gofmt -w") {
		t.Error("check-fmt runs 'gofmt -w': that rewrites files and always exits 0, " +
			"so it reports success on a dirty tree. A gate must report, not fix.")
	}
	if !strings.Contains(body, "exit 1") {
		t.Error("check-fmt never exits non-zero, so CI can't fail on drift")
	}
}

// TestCheckRunsTheFormatGate: `make check` is what CLAUDE.md tells us to run
// before every commit. It formats in place (convenient) and must then gate, so
// local runs and CI agree instead of local silently "passing" what CI rejects.
func TestCheckRunsTheFormatGate(t *testing.T) {
	body, ok := makeTarget(t, "check")
	if !ok {
		t.Fatal("the Makefile has no check target")
	}
	if !strings.Contains(body, "check-fmt") {
		t.Error("`make check` does not invoke check-fmt, so it can pass on a tree CI will reject")
	}
}

// TestCIRunsTheFormatGate: the target only helps if CI calls it.
func TestCIRunsTheFormatGate(t *testing.T) {
	data, err := os.ReadFile("../.github/workflows/ci.yml")
	if err != nil {
		t.Fatalf("read ci.yml: %v", err)
	}
	if !strings.Contains(string(data), "make check-fmt") {
		t.Error("the CI workflow does not run 'make check-fmt'; without it nothing " +
			"prevents unformatted code from reaching main")
	}
}

// unquote strips single- and double-quoted spans from a shell recipe, leaving
// roughly the commands. Crude, but the question it answers is narrow: does the
// recipe RUN something, or merely mention it in a message?
func unquote(s string) string {
	var b strings.Builder
	var quote rune
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// makeTarget returns the recipe lines of a Makefile target: everything after
// "<name>:" up to the next line that starts in column 0. Good enough for this
// Makefile's plain targets, and it deliberately excludes the comment block above
// the target so a mention in prose can't satisfy an assertion.
func makeTarget(t *testing.T, name string) (string, bool) {
	t.Helper()
	data, err := os.ReadFile("../Makefile")
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	var body []string
	in := false
	for _, line := range strings.Split(string(data), "\n") {
		if in {
			// Recipe lines are tab-indented; a blank line or column-0 text ends it.
			if line == "" || !strings.HasPrefix(line, "\t") {
				break
			}
			body = append(body, line)
			continue
		}
		if strings.HasPrefix(line, name+":") {
			in = true
		}
	}
	return strings.Join(body, "\n"), in
}
