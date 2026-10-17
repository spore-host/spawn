package cmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
)

// #928: `spawn stop` said "TTL countdown will pause until the instance is
// started again". The deadline is absolute — the docs say it "never moves
// across stop/wake cycles", and the reported instance came back with exactly
// its original launch+1h deadline. The message must say the deadline still
// applies, and give it in status's format (local, with UTC).
func TestStopNotice_SaysTheDeadlineStillApplies(t *testing.T) {
	denver, err := time.LoadLocation("America/Denver")
	if err != nil {
		t.Skip("no tzdata")
	}
	now := time.Date(2026, 10, 15, 17, 43, 0, 0, time.UTC)
	inst := stoppedIssueInstance(now)
	inst.State = "running"

	stop := ttlDeadlineNotice(inst, "stop", denver, now)
	if !strings.Contains(stop, "TTL deadline still applies: 2026-10-15 12:42 MDT (18:42 UTC), in 59m") {
		t.Errorf("stop notice = %q, want the deadline in local+UTC and that it still applies", stop)
	}
	start := ttlDeadlineNotice(inst, "start", denver, now.Add(2*time.Minute))
	if !strings.Contains(start, "still applies: 2026-10-15 12:42 MDT (18:42 UTC)") || !strings.Contains(start, "did not extend") {
		t.Errorf("start notice = %q", start)
	}
	for _, s := range []string{stop, start} {
		if strings.Contains(strings.ToLower(s), "pause") || strings.Contains(strings.ToLower(s), "resume") {
			t.Errorf("notice claims the TTL pauses: %q", s)
		}
	}

	// A deadline that elapsed while stopped: starting grants no fresh time
	// (docs/safety: "starting it does not grant fresh time").
	late := ttlDeadlineNotice(inst, "start", time.UTC, now.Add(2*time.Hour))
	if !strings.Contains(late, "has passed") || !strings.Contains(late, "terminate") {
		t.Errorf("past-deadline start notice = %q", late)
	}
	if got := ttlDeadlineNotice(&aws.InstanceInfo{Tags: map[string]string{}}, "stop", time.UTC, now); !strings.Contains(got, "No TTL deadline") {
		t.Errorf("no-deadline notice = %q", got)
	}
}

// stop no longer saves a "remaining TTL", and start no longer writes it back
// over spawn:ttl — which pkg/ttl documents as the total from launch, so the
// overwrite silently changed what the tag meant (#928).
func TestStopStartDoNotTouchTheTTL(t *testing.T) {
	now := time.Now()
	for _, tags := range []map[string]string{stopTags(false, now), stopTags(true, now), startTags(now)} {
		for k := range tags {
			if strings.HasPrefix(k, "spawn:ttl") {
				t.Errorf("stop/start writes %s; the deadline is absolute and must not be rewritten", k)
			}
		}
	}
	if stopTags(true, now)["spawn:last-stop-reason"] != "user-hibernated" || stopTags(false, now)["spawn:last-stop-reason"] != "user-stopped" {
		t.Error("stop reason lost")
	}
}

// ttlPauseClaim matches a sentence saying the TTL pauses or resumes across a
// stop: the claim #928 found in `spawn stop` and `spawn start`.
var ttlPauseClaim = regexp.MustCompile(`(?i)(ttl|deadline|countdown)[^.\n]{0,40}\b(paus|resum)|\b(paus|resum)[^.\n]{0,40}\b(ttl|deadline|countdown)`)

// The CLI and the docs must agree that a stop does not pause the deadline
// (#928; gate rule 5, "when two sources of intent conflict, gate their
// agreement"). Three sources, each discovered rather than listed:
//
//   - every string literal in cmd/'s non-test Go source (the runtime messages);
//   - every Markdown file in docs-gen/ (generated from the commands' help);
//   - every Markdown file in docs/ and the README.
//
// None may claim the TTL pauses, and the stop/start/hibernate reference pages
// must carry the shared statement that it does not.
func TestStopStartAgreeWithTheDocsThatTheDeadlineDoesNotPause(t *testing.T) {
	_, here, _, _ := runtime.Caller(0)
	cmdDir := filepath.Dir(here)
	root := filepath.Dir(cmdDir)

	fset := token.NewFileSet()
	literals := 0
	goFiles, _ := filepath.Glob(filepath.Join(cmdDir, "*.go"))
	for _, path := range goFiles {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			literals++
			v, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if m := ttlPauseClaim.FindString(v); m != "" {
				t.Errorf("%s: user-facing string claims the TTL pauses (%q): %q", fset.Position(lit.Pos()), m, v)
			}
			return true
		})
	}
	if literals == 0 {
		t.Fatal("found no string literals in cmd/ — the scan is broken")
	}

	var docs []string
	for _, pattern := range []string{"docs-gen/*.md", "docs/*.md", "README.md"} {
		m, _ := filepath.Glob(filepath.Join(root, pattern))
		docs = append(docs, m...)
	}
	if len(docs) == 0 {
		t.Fatal("found no docs — the scan is broken")
	}
	for _, path := range docs {
		data, err := os.ReadFile(path) //nolint:gosec // repo docs
		if err != nil {
			t.Fatal(err)
		}
		if m := ttlPauseClaim.FindString(string(data)); m != "" {
			t.Errorf("%s claims the TTL pauses: %q", path, m)
		}
	}

	for _, page := range []string{"stop.md", "start.md", "hibernate.md"} {
		data, err := os.ReadFile(filepath.Join(root, "docs-gen", page)) //nolint:gosec // repo docs
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), ttlDeadlineDoesNotPause) {
			t.Errorf("docs-gen/%s does not say the deadline keeps counting while stopped (run make gen-docs)", page)
		}
	}
}
