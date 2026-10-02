package aws

import (
	"strings"
	"testing"
)

// TestReaperCoverageSummaryDistinguishesThreeStates is the heart of spawn#624: the
// old check was a constant that reported "not detected" in every account, covered or
// not, so it carried no information and users learned to scroll past it. The three
// states must be distinguishable, and in particular "I looked and there is none"
// must not be confused with "I could not look".
func TestReaperCoverageSummaryDistinguishesThreeStates(t *testing.T) {
	covered := ReaperCoverage{Covered: true, Determined: true, How: "in-account reaper spawn-ttl-reaper-production"}
	if got := covered.Summary(); !strings.Contains(got, "covered") || !strings.Contains(got, "spawn-ttl-reaper-production") {
		t.Errorf("a covered account must name its evidence, got %q", got)
	}

	absent := ReaperCoverage{Determined: true}
	if got := absent.Summary(); !strings.Contains(got, "NOT covered") || !strings.Contains(got, ReaperCoverageRoleName) {
		t.Errorf("a definitively uncovered account must say so and name the role that would grant coverage, got %q", got)
	}

	unknown := ReaperCoverage{Why: "iam:GetRole: AccessDenied"}
	got := unknown.Summary()
	if !strings.Contains(got, "could not be determined") {
		t.Errorf("an undetermined probe must say so, got %q", got)
	}
	if strings.Contains(got, "NOT covered") {
		t.Errorf("an undetermined probe must NOT claim the account is uncovered — that is a claim it cannot support: %q", got)
	}
}

// TestReaperCoverageAdviceOnlyFiresWhenItChangesTheOutcome. A warning on every launch
// is the failure mode being fixed, so the advice must be empty for the cases where
// the reaper is merely a backstop rather than the mechanism.
func TestReaperCoverageAdviceOnlyFiresWhenItChangesTheOutcome(t *testing.T) {
	uncovered := ReaperCoverage{Determined: true}
	covered := ReaperCoverage{Covered: true, Determined: true, How: "x"}

	cases := []struct {
		name         string
		cov          ReaperCoverage
		onComplete   string
		fsxLifecycle string
		wantAdvice   bool
		why          string
	}{
		{"stop, uncovered", uncovered, "stop", "", true, "a stopped instance's EBS keeps billing and spored cannot act"},
		{"hibernate, uncovered", uncovered, "hibernate", "", true, "same as stop"},
		{"ephemeral fsx, uncovered", uncovered, "", "ephemeral", true, "only the reaper deletes the filesystem"},
		{"both, uncovered", uncovered, "stop", "ephemeral", true, "both consequences apply"},
		{"terminate, uncovered", uncovered, "terminate", "", false, "spored terminates from inside; the reaper is only a backstop"},
		{"no flags, uncovered", uncovered, "", "", false, "nothing here depends on the reaper"},
		{"durable fsx, uncovered", uncovered, "", "durable", false, "a durable filesystem is not the reaper's job"},
		{"stop, COVERED", covered, "stop", "", false, "there is a reaper; nothing to warn about"},
		{"ephemeral, COVERED", covered, "", "ephemeral", false, "there is a reaper"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ReaperCoverageAdvice(c.cov, c.onComplete, c.fsxLifecycle)
			if c.wantAdvice && got == "" {
				t.Errorf("expected advice (%s)", c.why)
			}
			if !c.wantAdvice && got != "" {
				t.Errorf("expected NO advice (%s), got:\n%s", c.why, got)
			}
		})
	}
}

// TestReaperCoverageAdviceNamesTheCost: the warning has to say what the user will pay
// for, not describe the architecture.
func TestReaperCoverageAdviceNamesTheCost(t *testing.T) {
	uncovered := ReaperCoverage{Determined: true}

	stop := ReaperCoverageAdvice(uncovered, "stop", "")
	if !strings.Contains(stop, "EBS") {
		t.Errorf("the stop warning must name what keeps billing:\n%s", stop)
	}

	fsx := ReaperCoverageAdvice(uncovered, "", "ephemeral")
	for _, want := range []string{"1200 GiB", "$174", "nothing else will"} {
		if !strings.Contains(fsx, want) {
			t.Errorf("the ephemeral-FSx warning must contain %q:\n%s", want, fsx)
		}
	}

	// Both flags together must report both consequences, not just the first.
	both := ReaperCoverageAdvice(uncovered, "stop", "ephemeral")
	if !strings.Contains(both, "EBS") || !strings.Contains(both, "1200 GiB") {
		t.Errorf("both consequences must appear:\n%s", both)
	}
}

// TestReaperCoverageAdviceHedgesWhenUndetermined: an unprovable verdict must not be
// stated as fact. The user still needs the warning — they may well be uncovered — but
// the wording has to be honest about which it is.
func TestReaperCoverageAdviceHedgesWhenUndetermined(t *testing.T) {
	undetermined := ReaperCoverage{Why: "lambda:ListFunctions: AccessDenied"}
	got := ReaperCoverageAdvice(undetermined, "stop", "")

	if got == "" {
		t.Fatal("an undetermined verdict on a reaper-dependent launch should still warn")
	}
	if !strings.Contains(got, "Could not confirm") {
		t.Errorf("the wording must hedge rather than assert absence:\n%s", got)
	}
	if strings.Contains(got, "No out-of-band TTL reaper covers this account,") {
		t.Errorf("must not assert absence it cannot prove:\n%s", got)
	}
}
