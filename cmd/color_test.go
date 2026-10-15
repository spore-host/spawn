package cmd

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// TestColorAllowed pins the colour policy (#898): colour only on a terminal, and
// never when the user has opted out by flag, NO_COLOR, or TERM=dumb.
func TestColorAllowed(t *testing.T) {
	tty := func() bool { return true }
	pipe := func() bool { return false }
	cases := []struct {
		name   string
		flag   bool
		env    map[string]string
		isTerm func() bool
		want   bool
	}{
		{"terminal, nothing set", false, nil, tty, true},
		{"terminal, ordinary TERM", false, map[string]string{"TERM": "xterm-256color"}, tty, true},
		{"stdout is a pipe or file", false, nil, pipe, false},
		{"--no-color / --accessibility", true, nil, tty, false},
		{"NO_COLOR set", false, map[string]string{"NO_COLOR": "1"}, tty, false},
		{"NO_COLOR set but empty is not an opt-out", false, map[string]string{"NO_COLOR": ""}, tty, true},
		{"TERM=dumb", false, map[string]string{"TERM": "dumb"}, tty, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := colorAllowed(c.flag, envOf(c.env), c.isTerm); got != c.want {
				t.Errorf("colorAllowed = %v, want %v", got, c.want)
			}
		})
	}
}

var ansiEscape = regexp.MustCompile("\x1b\\[")

// TestColorizeStateOffTTYHasNoEscapes is the reported symptom where it lives:
// with no flag given and stdout not a terminal, `spawn list` state cells must
// be plain text (#898). Before the fix only --no-color suppressed them.
func TestColorizeStateOffTTYHasNoEscapes(t *testing.T) {
	setColorInputs(t, false, false)
	for _, s := range []string{"running", "stopped", "stopping", "pending"} {
		if got := colorizeState(s); got != s {
			t.Errorf("colorizeState(%q) off-TTY = %q, want plain %q", s, got, s)
		}
	}

	// And the converse, so the test cannot pass by colour having been removed.
	setColorInputs(t, true, false)
	if got := colorizeState("running"); !ansiEscape.MatchString(got) {
		t.Errorf("colorizeState(running) on a TTY = %q, want it coloured", got)
	}
}

// TestNoColorFlagStillWins: --no-color must keep working on a real terminal.
func TestNoColorFlagStillWins(t *testing.T) {
	setColorInputs(t, true, true)
	if got := colorizeState("running"); got != "running" {
		t.Errorf("colorizeState with --no-color = %q, want plain", got)
	}
}

// TestSectionHeadingsOffTTYHaveNoEscapes covers the bold group headings, which
// had their own copy of the flag check.
func TestSectionHeadingsOffTTYHaveNoEscapes(t *testing.T) {
	setColorInputs(t, false, false)
	out := captureStdout(t, func() {
		displayStandaloneInstances(nil, true)
		displayStandaloneJobArrays(nil)
		displayParameterSweeps(nil)
	})
	if ansiEscape.MatchString(out) {
		t.Errorf("headings off-TTY contain ANSI escapes: %q", out)
	}
}

// TestColorDecisionIsCentral: the --no-color variable may be read only by the
// central decision. A command that consults the flag directly is exactly the
// pre-#898 shape, which ignored pipes and NO_COLOR.
func TestColorDecisionIsCentral(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("discovered no Go files in cmd/ (err=%v)", err)
	}
	read := regexp.MustCompile(`\bflagNoColor\b`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "color.go" || f == "root.go" {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if read.Match(b) {
			t.Errorf("%s reads flagNoColor directly; call colorEnabled() instead", f)
		}
	}
}

// setColorInputs fixes every input of colorEnabled for one test.
func setColorInputs(t *testing.T, terminal, noColorFlag bool) {
	t.Helper()
	oldTTY, oldFlag, oldA11y := stdoutIsTerminal, flagNoColor, flagAccessibility
	t.Cleanup(func() { stdoutIsTerminal, flagNoColor, flagAccessibility = oldTTY, oldFlag, oldA11y })
	stdoutIsTerminal = func() bool { return terminal }
	flagNoColor, flagAccessibility = noColorFlag, false
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }()
	fn()
	_ = w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	return buf.String()
}
