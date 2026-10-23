package taskproto

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The tests in this file reproduce spawn#1179: a container task filled the host
// /tmp, which is where the generated scripts keep their local copies of the
// completion record and .exitcode (recorddir.go). Both writes came out 0 bytes,
// and both empty files were uploaded, so the record every reader parses was
// empty and `spawn task status` failed with "unexpected end of JSON input".
//
// The full disk is simulated by leaving each local record file in exactly the
// state a full disk leaves it — present and empty — and read-only, so the
// script's write to it fails as it would with ENOSPC. That works on Linux and
// macOS without a mount; it does not work as root, who ignores the mode.

// awsUploadCapture is a stand-in `aws` that stores every `s3 cp <src> s3://...`
// upload under dir, named after the destination's last path element. A src of
// "-" is read from stdin, which is how `aws s3 cp -` uploads.
func awsUploadCapture(t *testing.T, binDir, dir string) {
	t.Helper()
	writeFakeExe(t, filepath.Join(binDir, "aws"), `#!/bin/bash
src="${@: -2:1}"; dst="${@: -1}"
case "$dst" in
  s3://*) if [ "$src" = - ]; then cat > "`+dir+`/${dst##*/}"; else cat "$src" > "`+dir+`/${dst##*/}"; fi ;;
esac
exit 0
`)
}

// fillRecordFiles leaves each path present, empty and unwritable, as a write
// to a full disk leaves it, and removes them when the test ends.
func fillRecordFiles(t *testing.T, paths ...string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("simulates a full disk with a read-only file, which root can write")
	}
	for _, p := range paths {
		_ = os.Remove(p)
		if err := os.WriteFile(p, nil, 0o444); err != nil { //nolint:gosec // test fixture: read-only on purpose
			t.Fatalf("create %s: %v", p, err)
		}
	}
	t.Cleanup(func() {
		for _, p := range paths {
			_ = os.Remove(p)
		}
	})
}

// readUpload returns the object the stand-in aws uploaded as name.
func readUpload(t *testing.T, dir, name, output string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("no %s was uploaded: %v\nscript output:\n%s", name, err, output)
	}
	return data
}

// TestWrapperUploadsAValidRecordWhenTheLocalCopyCannotBeWritten: with the local
// record files unwritable, the uploaded completion.json must still be the task's
// real record (a failed command's exit code, state and retry class) and
// .exitcode its exit code. Before #1179 both uploads were 0 bytes.
func TestWrapperUploadsAValidRecordWhenTheLocalCopyCannotBeWritten(t *testing.T) {
	skipOnWindows(t, "runs the Linux wrapper under bash with a stub aws on a POSIX PATH")
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	spec := &TaskSpec{
		TaskID:    "full-tmp-1179",
		Command:   []string{"bash", "-c", "exit 7"},
		Lifecycle: Lifecycle{TTL: "1h"},
	}
	// The pooled job script shares this code; it is the variant that does not
	// also write /tmp/SPAWN_COMPLETE, which this test has no reason to touch.
	w := genPooled(spec, "results-bucket", "us-east-1", false, testRunID)

	scriptPath := filepath.Join(t.TempDir(), "wrapper.sh")
	if err := os.WriteFile(scriptPath, []byte(w), 0o700); err != nil { //nolint:gosec // test fixture, needs +x
		t.Fatalf("write wrapper: %v", err)
	}
	binDir, uploads := t.TempDir(), t.TempDir()
	awsUploadCapture(t, binDir, uploads)
	writeFakeSudo(t, binDir)
	fillRecordFiles(t, CompletionRecordPath(), localPath(exitCodeFileName))

	cmd := exec.Command("bash", scriptPath)
	cmd.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"))
	out, _ := cmd.CombinedOutput()

	data := readUpload(t, uploads, "completion.json", string(out))
	rec, err := ParseCompletionRecord(data)
	if err != nil {
		t.Fatalf("uploaded completion.json is not a record (%v): %q\nscript output:\n%s", err, data, out)
	}
	if rec.TaskID != spec.TaskID || rec.RunID != testRunID {
		t.Errorf("task_id/run_id = %q/%q, want %q/%q", rec.TaskID, rec.RunID, spec.TaskID, testRunID)
	}
	if rec.State != StateFailed || rec.ExitCode != 7 || rec.RetryClass != RetryAppError {
		t.Errorf("state/exit_code/retry_class = %q/%d/%q, want failed/7/app_error\nrecord: %s", rec.State, rec.ExitCode, rec.RetryClass, data)
	}
	if got := strings.TrimSpace(string(readUpload(t, uploads, ".exitcode", string(out)))); got != "7" {
		t.Errorf("uploaded .exitcode = %q, want \"7\"", got)
	}
}

// TestFlushHookUploadsAValidRecordWhenTheLocalCopyCannotBeWritten is the same
// for the terminal-flush hook (spawn#632), which keeps its record on the same
// /tmp: a TTL that fires on a task that filled the disk must still leave a
// failed/ttl_expired record and an exit code of -1.
func TestFlushHookUploadsAValidRecordWhenTheLocalCopyCannotBeWritten(t *testing.T) {
	skipOnWindows(t, "runs the Linux flush hook under bash with a stub aws on a POSIX PATH")
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	_ = os.Remove(CompletionRecordPath()) // the hook's interlock: no wrapper record
	script := genFlush(flushSpec(), "results-bucket", "us-east-1", "run-1179")

	scriptPath := filepath.Join(t.TempDir(), "task-flush.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil { //nolint:gosec // test fixture, needs +x
		t.Fatalf("write flush hook: %v", err)
	}
	binDir, uploads := t.TempDir(), t.TempDir()
	awsUploadCapture(t, binDir, uploads)
	fillRecordFiles(t, localPath(terminalFileName), localPath(exitCodeFileName))

	cmd := exec.Command("bash", scriptPath, string(ExitTTLExpired))
	cmd.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"))
	out, _ := cmd.CombinedOutput()

	data := readUpload(t, uploads, "completion.json", string(out))
	rec, err := ParseCompletionRecord(data)
	if err != nil {
		t.Fatalf("uploaded completion.json is not a record (%v): %q\nhook output:\n%s", err, data, out)
	}
	if rec.TaskID != flushSpec().TaskID || rec.RunID != "run-1179" {
		t.Errorf("task_id/run_id = %q/%q", rec.TaskID, rec.RunID)
	}
	if rec.State != StateFailed || rec.ExitCode != -1 || rec.RetryClass != RetryTTLExpired || rec.TerminalReason != ExitTTLExpired {
		t.Errorf("state/exit_code/retry_class/terminal_reason = %q/%d/%q/%q, want failed/-1/ttl_expired/ttl_expired\nrecord: %s",
			rec.State, rec.ExitCode, rec.RetryClass, rec.TerminalReason, data)
	}
	if got := strings.TrimSpace(string(readUpload(t, uploads, ".exitcode", string(out)))); got != "-1" {
		t.Errorf("uploaded .exitcode = %q, want \"-1\"", got)
	}
}
