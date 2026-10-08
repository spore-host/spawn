package cmd

import (
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// The exit code is the entire point: `spawn terminate` exiting 0 on an abort
// meant a script or an agent checking it concluded the instance was gone when it
// was still running and still billing (#737).
func TestExitForError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
		why  string
	}{
		{"success", nil, 0, "nothing happened wrong"},
		{"a declined prompt", errAborted, ExitCodeAborted, "a caller must tell 'you said no' from 'it failed'"},
		{
			"a declined prompt with a per-command message",
			newAborted("aborted: %s is still running", "web-1"),
			ExitCodeAborted,
			"wrapping must not lose the sentinel",
		},
		{"a real failure", errors.New("AccessDenied"), 1, "ordinary errors stay 1"},
		{
			"a real failure wrapping something",
			fmt.Errorf("terminate: %w", errors.New("boom")),
			1,
			"only the abort sentinel gets the abort code",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := exitForError(tc.err); got != tc.want {
				t.Errorf("exitForError(%v) = %d, want %d — %s", tc.err, got, tc.want, tc.why)
			}
		})
	}
}

// ExitCodeAborted must not collide with 0 (success) or 1 (failure), or the
// distinction it exists for disappears.
func TestExitCodeAbortedIsDistinct(t *testing.T) {
	if ExitCodeAborted == 0 || ExitCodeAborted == 1 {
		t.Fatalf("ExitCodeAborted = %d, which is indistinguishable from success or failure", ExitCodeAborted)
	}
}

// A per-command abort message must say what was NOT done. "Aborted." left the
// reporter believing terminate had silently no-op'd, because the prompt itself
// was hidden by an output filter.
func TestAbortedMessagesNameTheConsequence(t *testing.T) {
	err := newAborted("aborted: %s is still running (terminate it with --yes)", "web-1 (i-abc)")
	if !isAborted(err) {
		t.Fatal("newAborted did not produce the sentinel")
	}
	msg := err.Error()
	if !strings.Contains(msg, "still running") {
		t.Errorf("message %q does not say what was left undone", msg)
	}
	if msg == "Aborted." || msg == "aborted" {
		t.Errorf("message %q is the bare form this issue is about", msg)
	}
}

// Every confirmation prompt in cmd/ must abort via the sentinel, not `return
// nil`. My own issue counted eleven such sites from a hand-picked file list; an
// exhaustive grep found NINETEEN. A static gate is the only way that count stays
// honest as commands are added.
func TestNoAbortSiteReturnsSuccess(t *testing.T) {
	// Matches ANY confirm* helper, not just confirmYes, and reads enough context
	// to clear the comment lines that usually sit inside the branch.
	//
	// Both of those were learned the hard way while writing this gate. The first
	// version grepped `confirmYes(` alone, so it missed terminate.go (which goes
	// through confirmTerminate) and PASSED against a deliberate revert of the very
	// site this issue was reported for. The second used -A 2, and the two lines
	// after terminate's prompt are comments — so the `return` sat outside the
	// window and it passed again. The same blind spot made my own count of these
	// sites eleven when an exhaustive grep found nineteen.
	out, err := exec.Command("grep", "-rnE", "-A", "8", `if !confirm[A-Za-z]*\(`, "./").Output()
	if err != nil {
		t.Skipf("grep: %v", err)
	}
	// A bare `return nil` — with or without a trailing comment, since
	// `return nil // pre-#737` is exactly what a revert looks like.
	retNil := regexp.MustCompile(`^([^:\-]+\.go)[-:](\d+)[-:]\s*return nil\s*(//.*)?$`)
	// Stop at the branch's closing brace so a `return nil` AFTER the if-block is
	// not blamed on it.
	closeBrace := regexp.MustCompile(`^[^:\-]+\.go[-:]\d+[-:]\s*\}\s*$`)

	lines := strings.Split(string(out), "\n")
	var bad []string
	for i, line := range lines {
		if !strings.Contains(line, "if !confirm") {
			continue
		}
		for _, follow := range lines[i+1 : min(i+9, len(lines))] {
			if closeBrace.MatchString(follow) {
				break
			}
			if m := retNil.FindStringSubmatch(follow); m != nil {
				bad = append(bad, m[1]+":"+m[2])
			}
		}
	}
	if len(bad) > 0 {
		t.Errorf("%d declined-prompt site(s) still return nil (exit 0), so a caller cannot "+
			"tell them from success:\n  %s\nReturn newAborted(...) instead.",
			len(bad), strings.Join(bad, "\n  "))
	}
}

// TestNoAbortSiteReturnsSuccess must be able to SEE a regression. Three earlier
// versions of it could not, each for a different reason, so its reach is now
// asserted directly rather than assumed.
func TestAbortGateSeesEveryConfirmHelper(t *testing.T) {
	out, err := exec.Command("grep", "-rhoE", `if !confirm[A-Za-z]*\(`, "./").Output()
	if err != nil {
		t.Skipf("grep: %v", err)
	}
	helpers := map[string]int{}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l != "" {
			helpers[l]++
		}
	}
	// Every confirm helper in the package, so adding a new one without covering
	// it here is a failure rather than a silent hole.
	for _, want := range []string{
		"if !confirmYes(",
		"if !confirmTerminate(",
		"if !confirmTypedPhrase(",
		"if !confirmReaperArm(",
	} {
		if helpers[want] == 0 {
			t.Errorf("no %q call sites found — either the helper was renamed (update this list "+
				"and the gate's pattern) or the grep is broken", want)
		}
	}
	total := 0
	for _, n := range helpers {
		total += n
	}
	if total < 25 {
		t.Errorf("found only %d confirmation sites; the gate previously covered 29, so its "+
			"reach has shrunk", total)
	}
	t.Logf("gate covers %d confirmation sites across %d helpers", total, len(helpers))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
