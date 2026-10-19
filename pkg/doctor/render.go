package doctor

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// RenderText writes a human-readable report and returns nothing; the caller
// decides the exit code from Report.OK().
func RenderText(w io.Writer, r *Report) {
	for _, c := range r.Checks {
		line := fmt.Sprintf("%s %s", c.Status.Symbol(), oneLine(c.Name))
		if c.Detail != "" {
			line += ": " + oneLine(c.Detail)
		}
		fmt.Fprintln(w, line)
		if (c.Status == Warn || c.Status == Fail) && c.Fix != "" {
			fmt.Fprintf(w, "    → %s\n", oneLine(c.Fix))
		}
	}
	pass, warn, fail, skip := r.Counts()
	fmt.Fprintf(w, "\n%d passed, %d warning(s), %d failed", pass, warn, fail)
	if skip > 0 {
		fmt.Fprintf(w, ", %d skipped", skip)
	}
	fmt.Fprintln(w)
	if r.OK() {
		fmt.Fprintln(w, "\nReady to launch. If this passes, the Quick Start should work as written.")
	} else {
		fmt.Fprintln(w, "\nNot ready — resolve the ✗ items above before launching. If you're on an")
		fmt.Fprintln(w, "institution-managed account, send the IAM baseline to your cloud administrator.")
	}
}

// oneLine folds a multi-line value onto one line. Every line of the text report
// before the summary is a check (status symbol first) or its fix ("    → "),
// which is what a reader, or a script, relies on; a detail carrying its own
// newlines broke that (truffle's three-line --version, #1140).
func oneLine(s string) string {
	if !strings.ContainsAny(s, "\r\n") {
		return s
	}
	var parts []string
	for _, l := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == '\r' }) {
		if l = strings.TrimSpace(l); l != "" {
			parts = append(parts, l)
		}
	}
	return strings.Join(parts, "; ")
}

// RenderJSON writes the report as JSON for automation.
func RenderJSON(w io.Writer, r *Report) error {
	type jsonCheck struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Detail string `json:"detail,omitempty"`
		Fix    string `json:"fix,omitempty"`
	}
	pass, warn, fail, skip := r.Counts()
	out := struct {
		OK      bool           `json:"ok"`
		Summary map[string]int `json:"summary"`
		Checks  []jsonCheck    `json:"checks"`
	}{
		OK:      r.OK(),
		Summary: map[string]int{"pass": pass, "warn": warn, "fail": fail, "skip": skip},
	}
	for _, c := range r.Checks {
		out.Checks = append(out.Checks, jsonCheck{Name: c.Name, Status: c.Status.String(), Detail: c.Detail, Fix: c.Fix})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
