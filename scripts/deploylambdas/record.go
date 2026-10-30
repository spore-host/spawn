package main

import (
	"fmt"
	"io"
	"strings"
)

const (
	outcomeNotAttempted   = "not attempted"
	outcomePrepared       = "pre-flight passed"
	outcomeRefused        = "refused"
	outcomeDryRun         = "dry run"
	outcomeDeployed       = "deployed"
	outcomeCurrent        = "already current"
	outcomeFailed         = "failed, not changed"
	outcomeRolledBack     = "failed, rolled back"
	outcomeRollbackFailed = "failed, ROLLBACK INCOMPLETE"
)

// result is one function's line in the deploy record.
type result struct {
	Function     string   `json:"function"`
	Component    string   `json:"component"`
	Outcome      string   `json:"outcome"`
	BeforeSha    string   `json:"before_code_sha256,omitempty"`
	BuiltSha     string   `json:"built_code_sha256,omitempty"`
	AfterSha     string   `json:"after_code_sha256,omitempty"`
	ConfigDiff   string   `json:"config_diff,omitempty"`
	VersionStamp string   `json:"version_stamp,omitempty"`
	Notes        []string `json:"notes,omitempty"`
	Error        string   `json:"error,omitempty"`
}

type record struct {
	From    string   `json:"from"`
	To      string   `json:"to"`
	DryRun  bool     `json:"dry_run"`
	Results []result `json:"results"`
	Skipped []skip   `json:"not_deployed"`
	Notes   []string `json:"notes,omitempty"`
	Error   string   `json:"error,omitempty"`
}

func orDash(s string) string {
	if s == "" {
		return "–"
	}
	return "`" + s + "`"
}

func cell(s string) string {
	if s == "" {
		return "–"
	}
	return strings.ReplaceAll(s, "|", `\|`)
}

// writeMarkdown renders the record for the job summary and the release PR.
func (r *record) writeMarkdown(w io.Writer) {
	p := func(format string, a ...any) { fmt.Fprintf(w, format, a...) }
	mode := "LIVE"
	if r.DryRun {
		mode = "DRY RUN (nothing was changed)"
	}
	p("## Lambda deploy record: %s..%s, %s\n\n", r.From, r.To, mode)
	p("From `scripts/deploylambdas` (#1445): code-only Lambda changes, deployed by the approval-gated `production` job. How to read it: docs/infra-account.md, \"Deploying code-only changes from CI\".\n\n")
	if r.Error != "" {
		p("**Stopped:** %s\n\n", cell(r.Error))
	}
	if len(r.Results) == 0 {
		p("No Lambda has a code-only change in this release, so nothing was deployed.\n\n")
	} else {
		p("| Function | Result | CodeSha256 before | built | after | Config diff | Version stamp |\n|---|---|---|---|---|---|---|\n")
		for _, x := range r.Results {
			p("| `%s` | %s | %s | %s | %s | %s | %s |\n", x.Function, cell(x.Outcome), orDash(x.BeforeSha), orDash(x.BuiltSha), orDash(x.AfterSha), cell(x.ConfigDiff), cell(x.VersionStamp))
		}
		p("\n")
		for _, x := range r.Results {
			if len(x.Notes) == 0 && x.Error == "" {
				continue
			}
			p("- `%s`:\n", x.Function)
			if x.Error != "" {
				p("  - error: %s\n", x.Error)
			}
			for _, n := range x.Notes {
				p("  - %s\n", n)
			}
		}
		p("\n")
	}
	if len(r.Notes) > 0 {
		for _, n := range r.Notes {
			p("- %s\n", n)
		}
		p("\n")
	}
	if len(r.Skipped) > 0 {
		p("### Not deployed by this job\n\n")
		for _, s := range r.Skipped {
			p("- `%s` (%s): %s\n", s.What, s.Status, s.Reason)
		}
	}
}
