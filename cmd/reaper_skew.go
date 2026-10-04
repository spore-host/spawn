package cmd

import (
	"fmt"
	"strings"

	"github.com/spore-host/spawn/pkg/buildinfo"
)

// reaperSkew describes how a deployed reaper's version relates to the CLI's.
type reaperSkew struct {
	// Deployed is the version read from the function's spawn:version tag; empty
	// when the tag is absent (a reaper deployed before versions were stamped).
	Deployed string
	// CLI is the running binary's version.
	CLI string
	// Matched is true only when both are known and equal.
	Matched bool
	// Note explains the state in the user's terms; empty when there is nothing
	// worth saying.
	Note string
}

// compareReaperVersion reports whether the deployed reaper matches this CLI
// (spawn#654).
//
// The two are INDEPENDENTLY DEPLOYED and nothing synchronises them: upgrading
// the CLI does not touch a deployed reaper, and `spawn reaper deploy` fetches
// the artifact for whatever version it was asked for. So skew is the normal
// state, not an exception — the concrete case that prompted this is the
// spore.host-operated reaper sitting untouched from 2026-07-31 to 2026-10-04
// while the CLI moved from ~v0.9x to v0.116.0, with nothing anywhere saying so.
//
// Deliberately NOT an error. A reaper one release behind still reaps; the point
// is to make the gap visible rather than to gate on it. The remedy is a re-run
// of `spawn reaper deploy`, which is what actually pulls a matching artifact.
//
// Pure, so the wording is testable without AWS.
func compareReaperVersion(deployed, cli string) reaperSkew {
	s := reaperSkew{
		Deployed: strings.TrimPrefix(strings.TrimSpace(deployed), "v"),
		CLI:      strings.TrimPrefix(strings.TrimSpace(cli), "v"),
	}

	switch {
	case s.Deployed == "":
		// A reaper deployed before `spawn reaper deploy` stamped spawn:version, or
		// one created by something else. Unknown is not the same as mismatched, and
		// saying "mismatch" here would be a guess.
		s.Note = "deployed version unknown (no spawn:version tag) — redeploy to stamp it"
	case buildinfo.IsDev(s.CLI):
		// A dev build has no release number, so any comparison is meaningless.
		// Reporting skew against it would cry wolf on every working tree.
		s.Note = fmt.Sprintf("this is a dev build, so there is nothing meaningful to compare against %s", s.Deployed)
	case s.Deployed == s.CLI:
		s.Matched = true
	default:
		s.Note = fmt.Sprintf("this CLI is %s — they are deployed independently, so re-run "+
			"`spawn reaper deploy` to put %s in the account", s.CLI, s.CLI)
	}
	return s
}

// renderReaperSkew is the status line(s) for the version comparison. Empty when
// the versions agree and there is nothing to report.
func renderReaperSkew(s reaperSkew) string {
	if s.Matched || s.Note == "" {
		return ""
	}
	return fmt.Sprintf("             %s\n", s.Note)
}
