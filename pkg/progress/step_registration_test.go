package progress

import (
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func nonTTYProgress() *Progress {
	p := newProgress(false)
	p.tty = false
	return p
}

// A step name the fixed list does not contain must still take effect. Before
// #739, Start/Complete/Error/Skip scanned the nine-step list and returned
// silently on no match — 24 of the 32 names used across cmd/ matched nothing,
// and they were spawn's longest operations.
func TestUnregisteredStepNameStillTakesEffect(t *testing.T) {
	p := nonTTYProgress()
	before := len(p.steps)

	out := captureStdout(t, func() { p.Start("Verifying spored agent") })
	if !strings.Contains(out, "Verifying spored agent") {
		t.Errorf("Start on an unregistered name printed %q, want the step name — "+
			"this is the five-minute silent wait in #739", out)
	}
	if len(p.steps) != before+1 {
		t.Errorf("step count %d -> %d, want one appended", before, len(p.steps))
	}
	if got := p.steps[len(p.steps)-1].Status; got != "running" {
		t.Errorf("appended step status = %q, want running", got)
	}

	out = captureStdout(t, func() { p.Complete("Verifying spored agent") })
	if !strings.Contains(out, "Verifying spored agent") {
		t.Errorf("Complete on the same name printed %q, want the step name", out)
	}
	if len(p.steps) != before+1 {
		t.Errorf("Complete appended a duplicate: %d steps", len(p.steps))
	}
}

// A RUNNING step must be announced on a non-TTY. Measured at 0 bytes before the
// fix, which made a long wait and a wedged process indistinguishable.
func TestNonTTYAnnouncesARunningStep(t *testing.T) {
	p := nonTTYProgress()
	name := p.steps[0].Name

	out := captureStdout(t, func() { p.Start(name) })
	if strings.TrimSpace(out) == "" {
		t.Fatalf("Start emitted nothing on a non-TTY — a step in flight is invisible (#739)")
	}
	if !strings.Contains(out, name) {
		t.Errorf("output %q does not name the step %q", out, name)
	}

	// ...and exactly once, however many times display() is driven.
	again := captureStdout(t, func() { p.Start(name); p.Start(name) })
	if n := strings.Count(again, name); n != 0 {
		t.Errorf("re-announced a running step %d times; the start line must be printed once", n)
	}

	fin := captureStdout(t, func() { p.Complete(name) })
	if !strings.Contains(fin, name) {
		t.Errorf("Complete printed %q, want the step name", fin)
	}
}

// Error must print its message even for a name the fixed list lacks. That
// fmt.Printf used to sit INSIDE the matched-step branch, so an unregistered name
// swallowed the error text as well as the step line.
func TestErrorPrintsForAnUnregisteredStep(t *testing.T) {
	p := nonTTYProgress()
	out := captureStdout(t, func() { p.Error("Recalling FSx filesystem", os.ErrDeadlineExceeded) })
	if !strings.Contains(out, os.ErrDeadlineExceeded.Error()) {
		t.Errorf("error text missing from %q — a failure reported nowhere at all", out)
	}
	if !strings.Contains(out, "Recalling FSx filesystem") {
		t.Errorf("step name missing from %q", out)
	}
}

// Quiet mode must stay silent — it exists so `-o json` output is parseable, and
// appending steps must not change that.
func TestQuietStaysSilentForUnregisteredSteps(t *testing.T) {
	p := NewQuietProgress()
	p.tty = false
	out := captureStdout(t, func() {
		p.Start("Uploading ISO to S3")
		p.Error("Uploading ISO to S3", os.ErrClosed)
		p.Complete("Uploading ISO to S3")
	})
	if strings.TrimSpace(out) != "" {
		t.Errorf("quiet progress wrote %q", out)
	}
}

// Hygiene gate: a name passed to Complete/Error/Skip but never to Start is a
// mismatched pair. Find-or-append makes that visible instead of silent (two
// steps in the display rather than one), but it is still a bug in the caller.
// Documented rather than enforced for now: five such pairs exist today, all in
// FSx/cohort paths, and fixing them is a separate change.
func TestReportMismatchedStepPairs(t *testing.T) {
	out, err := exec.Command("grep", "-rhoE", `prog\.(Start|Complete|Error|Skip)\("[^"]*"`, "../../cmd").Output()
	if err != nil {
		t.Skipf("grep unavailable: %v", err)
	}
	re := regexp.MustCompile(`prog\.(\w+)\("([^"]*)"`)
	verbs := map[string]map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		m := re.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if verbs[m[2]] == nil {
			verbs[m[2]] = map[string]bool{}
		}
		verbs[m[2]][m[1]] = true
	}
	var mismatched []string
	for name, v := range verbs {
		if !v["Start"] {
			mismatched = append(mismatched, name)
		}
	}
	sort.Strings(mismatched)
	// Known today; if this grows, a caller added another mismatched pair.
	const knownMismatched = 5
	if len(mismatched) > knownMismatched {
		t.Errorf("%d step names are completed/errored/skipped but never started (was %d): %v\n"+
			"Each renders as a second, never-started step. Pair the Start and the "+
			"Complete/Error/Skip on the same literal.",
			len(mismatched), knownMismatched, mismatched)
	}
	t.Logf("mismatched Start/finish pairs (pre-existing): %v", mismatched)
}
