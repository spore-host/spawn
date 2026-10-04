package cmd

import (
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/reaperdeploy"
)

// TestReaperVersionSkewIsReported is spawn#654.
//
// The CLI and a deployed reaper are INDEPENDENTLY deployed and nothing
// synchronises them. The case that prompted this: the spore.host-operated reaper
// sat untouched from 2026-07-31 to 2026-10-04 while the CLI went from ~v0.9x to
// v0.116.0 — so it was missing an alarm, two IAM grants and a packaging fix, and
// nothing anywhere said so. Skew is the normal state, not an exception.
func TestReaperVersionSkewIsReported(t *testing.T) {
	s := compareReaperVersion("0.110.0", "0.117.0")
	if s.Matched {
		t.Fatal("0.110.0 vs 0.117.0 must not be reported as matched")
	}
	if !strings.Contains(s.Note, "0.117.0") {
		t.Errorf("the note must name THIS CLI's version so the gap is concrete: %q", s.Note)
	}
	if !strings.Contains(s.Note, "reaper deploy") {
		t.Errorf("the note must name the remedy — re-running deploy is what pulls a matching "+
			"artifact: %q", s.Note)
	}
	if renderReaperSkew(s) == "" {
		t.Error("a real skew must render something")
	}
}

// TestMatchingVersionsSayNothing. The check has to be quiet when it has nothing
// to add, or it becomes noise people learn to skip — which is how a real skew
// gets missed.
func TestMatchingVersionsSayNothing(t *testing.T) {
	s := compareReaperVersion("0.117.0", "0.117.0")
	if !s.Matched {
		t.Fatal("identical versions must match")
	}
	if got := renderReaperSkew(s); got != "" {
		t.Errorf("matching versions must render nothing, got %q", got)
	}
}

// TestVersionComparisonIgnoresTheVPrefix: the tag is written without it
// (reaperdeploy.Tags trims it) but a caller or an older deploy may carry it.
// "v0.117.0" and "0.117.0" are the same release and must not read as skew.
func TestVersionComparisonIgnoresTheVPrefix(t *testing.T) {
	for _, pair := range [][2]string{
		{"v0.117.0", "0.117.0"},
		{"0.117.0", "v0.117.0"},
		{" v0.117.0 ", "0.117.0"},
	} {
		if s := compareReaperVersion(pair[0], pair[1]); !s.Matched {
			t.Errorf("compareReaperVersion(%q, %q) should match — same release, different prefix",
				pair[0], pair[1])
		}
	}
}

// TestUnknownDeployedVersionIsNotCalledAMismatch. A reaper deployed before
// `spawn reaper deploy` stamped spawn:version has no tag to read. Unknown is not
// the same as mismatched, and claiming a mismatch there would be a guess.
func TestUnknownDeployedVersionIsNotCalledAMismatch(t *testing.T) {
	s := compareReaperVersion("", "0.117.0")
	if s.Matched {
		t.Error("an unknown deployed version must not be reported as matched")
	}
	if strings.Contains(s.Note, "0.117.0") && !strings.Contains(s.Note, "unknown") {
		t.Errorf("the note should say the version is UNKNOWN rather than implying a mismatch: %q", s.Note)
	}
	if !strings.Contains(s.Note, "redeploy") {
		t.Errorf("it should say how to fix the missing tag: %q", s.Note)
	}
}

// TestDevBuildDoesNotCryWolf. A dev build has no release number, so comparing it
// to a deployed version is meaningless — and reporting skew on every working
// tree would train people to ignore the line.
func TestDevBuildDoesNotCryWolf(t *testing.T) {
	s := compareReaperVersion("0.117.0", "dev")
	if s.Matched {
		t.Error("a dev build cannot be said to match")
	}
	if !strings.Contains(s.Note, "dev build") {
		t.Errorf("the note must explain WHY it isn't comparing: %q", s.Note)
	}
	if strings.Contains(s.Note, "reaper deploy") {
		t.Error("a dev build must not be told to redeploy — there is no release artifact for it")
	}
}

// TestStatusShowsVersionEvenWhenUnknown: the old render omitted the Version line
// entirely when the tag was absent, which reads as "no version concern" rather
// than "we don't know".
func TestStatusShowsVersionEvenWhenUnknown(t *testing.T) {
	out := renderReaperStatus(reaperdeploy.Info{
		Deployed: true, Armed: true, Schedule: "rate(10 minutes)", RuleEnabled: true,
	}, "123456789012", "us-west-2")

	if !strings.Contains(out, "Version:") {
		t.Errorf("status must always show a Version line — omitting it reads as 'no concern' "+
			"rather than 'unknown':\n%s", out)
	}
}
