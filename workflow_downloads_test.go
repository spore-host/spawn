package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A workflow must not run downloaded bytes it has not verified (#885).
//
// The gitleaks step ran `curl ... | tar -xz` and then executed the result: the
// scanner that decides whether secrets are present was itself unverified. The rule
// is about the mechanism, not that one step:
//
//   - downloaded bytes may not be piped straight into tar or a shell — there is no
//     point at which they could have been checked;
//   - a step that downloads a file with curl -o must verify it with sha256sum -c
//     before the step ends.
//
// Steps are found by their `- name:` boundaries and comment lines are skipped, so
// the comment explaining this rule cannot trip it (rule 3).
var (
	curlPiped    = regexp.MustCompile(`curl\b[^#]*\|\s*(sudo\s+)?(tar|sh|bash|zsh)\b`)
	curlToFile   = regexp.MustCompile(`curl\b[^#]*\s-[a-zA-Z]*o\s`)
	sha256Verify = regexp.MustCompile(`sha256sum\s+(-c|--check)\b`)
)

func TestWorkflowsVerifyWhatTheyDownload(t *testing.T) {
	files, err := filepath.Glob(".github/workflows/*.y*ml")
	if err != nil || len(files) == 0 {
		t.Fatalf("found no workflows (%v) — the glob has broken, which would make this pass vacuously", err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f) //nolint:gosec // the repo's own workflows
		if err != nil {
			t.Fatal(err)
		}
		var step []string
		flush := func() {
			// Joined across backslash continuations: the original step put its
			// `| tar` on the line AFTER `curl ... \`, and the first version of this
			// gate read physical lines and passed it.
			text := strings.ReplaceAll(strings.Join(step, "\n"), "\\\n", " ")
			if curlPiped.MatchString(text) {
				t.Errorf("%s: a step pipes a download straight into a shell or tar, so it is "+
					"executed without ever being verified:\n%s", f, text)
			}
			if curlToFile.MatchString(text) && !sha256Verify.MatchString(text) {
				t.Errorf("%s: a step downloads a file with curl -o and never verifies it with "+
					"`sha256sum -c` — pin the expected hash in the step and check it before use:\n%s", f, text)
			}
			step = step[:0]
		}
		for _, line := range strings.Split(string(b), "\n") {
			trim := strings.TrimSpace(line)
			if strings.HasPrefix(trim, "#") {
				continue
			}
			if strings.HasPrefix(trim, "- name:") || strings.HasPrefix(trim, "- uses:") {
				flush()
			}
			step = append(step, line)
		}
		flush()
	}
}
