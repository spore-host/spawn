package pipeline

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Every PipelineStatus must be explicitly classified as terminal or not.
//
// `spawn pipeline launch --wait` polls until IsTerminal returns true, so a status
// that falls through to "not terminal" is an unbounded wait rather than a visible
// error. The switch in IsTerminal has no default for that reason, but Go does not
// require exhaustiveness over a string-typed constant set — nothing stops a new
// status being added and silently treated as non-terminal.
//
// The IsTerminal comment claimed this gate existed before it did. It was written
// while removing StatusCancelled from the terminal list as a deliberate revert,
// and nothing failed — a comment asserting a guarantee that no test enforced.
//
// Discovery, not a list (CLAUDE.md rule 4): the constants come out of the source,
// so a new one is covered the moment it is declared.
func TestEveryPipelineStatusIsClassified(t *testing.T) {
	src, err := os.ReadFile("pipeline.go")
	if err != nil {
		t.Fatalf("read pipeline.go: %v", err)
	}
	body := string(src)

	declared := regexp.MustCompile(`(?m)^\s*(Status[A-Za-z]+)\s+PipelineStatus\s*=`).FindAllStringSubmatch(body, -1)
	if len(declared) < 5 {
		t.Fatalf("found %d PipelineStatus constants; the repo declares at least five, so the "+
			"extraction has broken rather than the set having shrunk", len(declared))
	}

	// The text of IsTerminal, so a constant merely mentioned elsewhere in the file
	// does not count as classified.
	start := strings.Index(body, "func (s PipelineStatus) IsTerminal() bool {")
	if start < 0 {
		t.Fatal("IsTerminal not found — this gate is anchored on a function that no longer exists")
	}
	end := strings.Index(body[start:], "\n}")
	if end < 0 {
		t.Fatal("could not find the end of IsTerminal")
	}
	switchBody := body[start : start+end]

	for _, m := range declared {
		name := m[1]
		if !strings.Contains(switchBody, name) {
			t.Errorf("%s is declared but not named in IsTerminal's switch. An unclassified status "+
				"falls through to false, so `pipeline launch --wait` would poll it forever instead of "+
				"reporting anything (#807). Add it to the terminal or the non-terminal case.", name)
		}
	}
}

// Terminal and non-terminal must be disjoint and complete in behaviour, not just
// present in the source.
func TestIsTerminalAndSucceededAgree(t *testing.T) {
	terminal := map[PipelineStatus]bool{
		StatusCompleted: true,
		StatusFailed:    true,
		StatusCancelled: true,
	}
	for _, s := range []PipelineStatus{
		StatusInitializing, StatusRunning, StatusCompleted, StatusFailed, StatusCancelled,
	} {
		if got := s.IsTerminal(); got != terminal[s] {
			t.Errorf("%s.IsTerminal() = %v, want %v", s, got, terminal[s])
		}
	}

	// Only COMPLETED is success. If StatusFailed ever reported success, `launch
	// --wait && next-step` would run the next step after a failed pipeline, which
	// is the precise thing --wait exists to prevent.
	if !StatusCompleted.Succeeded() {
		t.Error("StatusCompleted does not report success")
	}
	for _, s := range []PipelineStatus{StatusFailed, StatusCancelled, StatusRunning, StatusInitializing} {
		if s.Succeeded() {
			t.Errorf("%s reports success", s)
		}
	}
}
