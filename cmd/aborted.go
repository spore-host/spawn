package cmd

import (
	"errors"
	"fmt"
)

// ExitCodeAborted is returned when the user declines a confirmation prompt.
//
// Deliberately NOT 1. `spawn terminate` exiting 0 on an abort meant a script or
// an agent checking the exit code concluded the instance was gone when it was
// still running and still billing — and the only thing distinguishing the two
// was a line on stderr, which the reporter's output filter hid (#737). But
// exiting 1 would be nearly as bad in the other direction: "you said no" and "it
// failed" need different handling, and collapsing them forces a caller to parse
// text to tell a deliberate decline from a broken command.
//
// 75 is EX_TEMPFAIL from sysexits.h, whose meaning — "the user is invited to
// retry" — is exactly right here, and which nothing else in spawn uses.
const ExitCodeAborted = 75

// errAborted is the sentinel every declined prompt returns.
//
// All eleven abort sites used to `return nil`, so every one of them exited 0. The
// cost-safety ones are the reason this matters: terminate (single and job array),
// cleanup, and cancel can each leave real instances running after reporting
// success.
//
// Note this is the OPPOSITE case from #648, which must be preserved: a NAME that
// resolves to nothing is success, because the instance is already gone, which is
// what the caller wanted. An abort is the reverse — the instance is definitely
// still there.
var errAborted = errors.New("aborted by user")

// ErrAborted returns the sentinel, for callers outside this package.
func ErrAborted() error { return errAborted }

// abortedError wraps the sentinel with a per-command message, so the user sees
// what was not done rather than a bare "aborted".
type abortedError struct{ msg string }

func (e *abortedError) Error() string { return e.msg }
func (e *abortedError) Is(target error) bool {
	return target == errAborted
}

// newAborted returns an error that reports as msg and matches errAborted.
func newAborted(format string, args ...interface{}) error {
	return &abortedError{msg: fmt.Sprintf(format, args...)}
}

// isAborted reports whether err is (or wraps) the abort sentinel.
func isAborted(err error) bool { return errors.Is(err, errAborted) }

// exitForError maps an error to spawn's process exit code.
//
// Separate from Execute's own error printing so the mapping is testable: the
// whole point of a distinct code is that a script can branch on it, and a
// mapping only exercised by running the binary is one nobody checks.
func exitForError(err error) int {
	switch {
	case err == nil:
		return 0
	case isAborted(err):
		return ExitCodeAborted
	default:
		return 1
	}
}
