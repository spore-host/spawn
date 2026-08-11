package cmd

import "testing"

// resetResumeFlags restores the package-level resume flag vars to their
// zero values after a test mutates them directly (bypassing cobra's flag
// parsing, since these tests only exercise runResume's own validation, not
// the CLI plumbing).
func resetResumeFlags(t *testing.T) {
	t.Helper()
	orig := struct {
		sweepID    string
		maxConc    int
		maxConcAut bool
		detach     bool
	}{resumeSweepID, resumeMaxConcurrent, resumeMaxConcurrentAuto, resumeDetach}
	t.Cleanup(func() {
		resumeSweepID = orig.sweepID
		resumeMaxConcurrent = orig.maxConc
		resumeMaxConcurrentAuto = orig.maxConcAut
		resumeDetach = orig.detach
	})
}

// TestRunResume_MaxConcurrentAutoAndMaxConcurrentMutuallyExclusive is the
// spawn#494 validation guard: --max-concurrent-auto and --max-concurrent must
// not both be set, matching the pattern launch_sweep.go already has for the
// same two flags. This must be checked BEFORE any file/AWS I/O, which is why
// it's safe to unit-test directly (no Substrate/filesystem needed) — the
// error returns before runResume touches either.
func TestRunResume_MaxConcurrentAutoAndMaxConcurrentMutuallyExclusive(t *testing.T) {
	resetResumeFlags(t)
	resumeSweepID = "does-not-exist"
	resumeMaxConcurrentAuto = true
	resumeMaxConcurrent = 5

	err := runResume(resumeCmd, nil)
	if err == nil {
		t.Fatal("want error when both --max-concurrent-auto and --max-concurrent are set")
	}
}

// TestRunResume_MaxConcurrentAutoRejectedWithDetach is the spawn#494 guard for
// the not-yet-supported detached path (see the doc comment in resume.go for
// why): --max-concurrent-auto + --detach must fail fast with a clear message
// rather than silently ignoring the flag or reaching into detached-mode I/O.
func TestRunResume_MaxConcurrentAutoRejectedWithDetach(t *testing.T) {
	resetResumeFlags(t)
	resumeSweepID = "does-not-exist"
	resumeMaxConcurrentAuto = true
	resumeDetach = true

	err := runResume(resumeCmd, nil)
	if err == nil {
		t.Fatal("want error when --max-concurrent-auto is combined with --detach")
	}
}
