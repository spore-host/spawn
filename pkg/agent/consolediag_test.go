package agent

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// completionFailed decides whether a diagnostic gets copied to the console, so
// it must read the record the bootstrap actually writes — and must never error a
// teardown path on bad input.
func TestCompletionFailed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		record string
		want   bool
		why    string
	}{
		{"explicit failure", `{"status":"failed","exit_code":1}`, true, "the reported case"},
		{"failure, no code", `{"status":"failed"}`, true, "status alone is enough"},
		{"non-zero code, status says completed", `{"status":"completed","exit_code":2}`, true,
			"the code is the more specific fact; the two are written together"},
		{"clean completion", `{"status":"completed","exit_code":0}`, false, "nothing to diagnose"},
		{"status only, completed", `{"status":"completed"}`, false, "nothing to diagnose"},
		{"case-insensitive status", `{"status":"FAILED"}`, true, "do not depend on casing"},
		// Everything below must be false rather than an error: this runs on a
		// terminate path and must not interfere with it.
		{"empty record", ``, false, "a launch without --command writes nothing"},
		{"not JSON at all", `this is not json`, false, "lenient by design"},
		{"truncated JSON", `{"status":"fail`, false, "a half-written file must not panic or error"},
		{"unrelated JSON", `{"other":"value"}`, false, "no status, no code, no opinion"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := completionFailed([]byte(tc.record)); got != tc.want {
				t.Errorf("completionFailed(%q) = %v, want %v — %s", tc.record, got, tc.want, tc.why)
			}
		})
	}
}

// The tail must be the LAST n lines, intact. A truncated first line in the one
// artifact someone is reading to understand a failure is a small but real bug.
func TestTailFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log")

	var sb strings.Builder
	for i := 1; i <= 200; i++ {
		sb.WriteString("line-" + strconv.Itoa(i) + "\n")
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := tailFile(path, 50)
	if err != nil {
		t.Fatalf("tailFile: %v", err)
	}
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 50 {
		t.Fatalf("got %d lines, want 50", len(lines))
	}
	if lines[0] != "line-151" {
		t.Errorf("first line = %q, want line-151 (a mis-seek would truncate it)", lines[0])
	}
	if lines[49] != "line-200" {
		t.Errorf("last line = %q, want line-200", lines[49])
	}
}

// A file shorter than the tail returns all of it, not an error.
func TestTailFileShorterThanN(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	if err := os.WriteFile(path, []byte("a\nb\nc\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := tailFile(path, 50)
	if err != nil {
		t.Fatalf("tailFile: %v", err)
	}
	if got != "a\nb\nc\n" {
		t.Errorf("got %q, want the whole file", got)
	}
}

// A very long line must not lose the tail. bufio.Scanner's default 64 KB token
// limit would error the scan out, and a workload emitting a stack trace or a
// JSON blob on one line is exactly the failure worth diagnosing.
func TestTailFileSurvivesVeryLongLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	long := strings.Repeat("x", 200*1024) // 200 KB on one line
	body := "first\n" + long + "\nlast\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := tailFile(path, 50)
	if err != nil {
		t.Fatalf("tailFile on a 200 KB line: %v", err)
	}
	if !strings.Contains(got, "last") {
		t.Error("the tail lost its last line to a long-line scan error — which is the " +
			"case most worth diagnosing")
	}
}

// A missing file is the COMMON case (a launch with no --command) and must be a
// quiet error, not a warning or a panic.
func TestTailFileMissing(t *testing.T) {
	if _, err := tailFile(filepath.Join(t.TempDir(), "nope"), 50); err == nil {
		t.Error("expected an error for a missing file so the caller can stay quiet")
	}
}
