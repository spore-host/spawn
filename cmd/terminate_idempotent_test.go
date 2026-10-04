package cmd

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestResolveInstanceNotFoundIsIdentifiable: `terminate` must be able to tell
// "no such instance" from "the lookup failed" without matching error strings,
// which is what the ErrInstanceNotFound sentinel is for (spawn#648).
func TestResolveInstanceNotFoundIsIdentifiable(t *testing.T) {
	notFound := &notFoundError{"no instance found with name: nf-03461ad6fc36"}
	if !errors.Is(notFound, ErrInstanceNotFound) {
		t.Error("a not-found resolution must match ErrInstanceNotFound under errors.Is")
	}
	// An unrelated failure must NOT match, or terminate would exit 0 on a genuine
	// AWS error and a caller would believe the instance was gone.
	other := fmt.Errorf("failed to describe instances: AccessDenied")
	if errors.Is(other, ErrInstanceNotFound) {
		t.Error("an unrelated lookup failure must not match ErrInstanceNotFound")
	}
}

// TestNotFoundErrorKeepsTheHumanMessage. Wrapping the sentinel with %w would
// append its text and produce "no instance found with name: x: instance not
// found". The typed error exists only to keep the CLI output clean, so pin that.
func TestNotFoundErrorKeepsTheHumanMessage(t *testing.T) {
	const want = "no instance found with name: nf-03461ad6fc36"
	if got := (&notFoundError{want}).Error(); got != want {
		t.Errorf("Error() = %q, want %q — the sentinel must not leak into the message", got, want)
	}
}

// TestTerminateTreatsAlreadyGoneAsSuccess is the behaviour change, asserted
// against the source because terminateSingle needs a live AWS client.
//
// `terminate`'s goal is "this instance is not running". Three states already
// satisfy it — never existed, already terminated, already shutting down — and
// all three used to exit 1. nf-spawn's cleanup then reported "the instance may
// still be running and billing until its TTL" when nothing had been created,
// which is the one sentence a user cannot ignore. The already-terminated and
// shutting-down cases are if anything more common, because on_complete=terminate
// means a successful task's instance is usually gone before an executor cleans up.
func TestTerminateTreatsAlreadyGoneAsSuccess(t *testing.T) {
	src, err := os.ReadFile("terminate.go")
	if err != nil {
		t.Fatalf("read terminate.go: %v", err)
	}
	s := string(src)

	fn := s[strings.Index(s, "func terminateSingle("):]
	fn = fn[:strings.Index(fn, "\nfunc ")]

	// Not-found opts in via the sentinel, and returns nil.
	if !strings.Contains(fn, "errors.Is(err, ErrInstanceNotFound)") {
		t.Error("terminateSingle must recognise ErrInstanceNotFound rather than string-matching")
	}

	for _, state := range []string{"terminated", "shutting-down"} {
		idx := strings.Index(fn, `instance.State == "`+state+`"`)
		if idx < 0 {
			t.Errorf("no branch for state %q", state)
			continue
		}
		branch := fn[idx:]
		if end := strings.Index(branch, "\n\t}"); end > 0 {
			branch = branch[:end]
		}
		if strings.Contains(branch, "return fmt.Errorf") {
			t.Errorf("state %q still returns an error; terminate is idempotent, so an instance "+
				"that is already gone must exit 0 — an executor's cleanup reads a non-zero exit "+
				"as \"it may still be running and billing\"", state)
		}
		if !strings.Contains(branch, "return nil") {
			t.Errorf("state %q must return nil", state)
		}
	}
}

// TestOnlyTerminateOptsIntoNotFoundSuccess guards the blast radius.
//
// resolveInstance is shared by ~10 commands — connect, dns, config, extend,
// task, service, plugin — where a non-existent instance genuinely IS an error
// ("connect to a box that isn't there" must fail). Only terminate may treat it
// as success, so assert no other command starts swallowing it.
func TestOnlyTerminateOptsIntoNotFoundSuccess(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	use := regexp.MustCompile(`errors\.Is\([^,]+,\s*ErrInstanceNotFound\)`)
	allowed := map[string]bool{"terminate.go": true}

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, readErr := os.ReadFile(name)
		if readErr != nil {
			continue
		}
		if use.Match(b) && !allowed[name] {
			t.Errorf("%s treats a missing instance as a special case. Only `terminate` may do "+
				"that (its goal is \"not running\"); for every other command a non-existent "+
				"instance is a real error. If this is deliberate, add it to the allow-list "+
				"with a reason.", name)
		}
	}
}

// TestUnknownInstanceIDStillFails is the counterweight to terminate's
// idempotence, and it exists because CI caught me widening the change too far.
//
// test/e2e's negative matrix asserts `spawn terminate i-doesnotexist -y` exits
// non-zero. My first version made ALL not-found resolutions succeed, which broke
// it — and the test was right. An instance ID is opaque and AWS-assigned: one
// that matches nothing is a typo, not a resource someone already cleaned up, and
// exiting 0 would let a user or script believe they stopped a still-billing
// instance. Cost control is the project's existential concern, so that case
// stays an error.
//
// A caller-assigned NAME is the opposite: its absence after cleanup is the
// expected steady state, and that is the case actually reported in #648
// (nf-spawn's `nf-03461ad6fc36`).
func TestUnknownInstanceIDStillFails(t *testing.T) {
	src, err := os.ReadFile("utils.go")
	if err != nil {
		t.Fatalf("read utils.go: %v", err)
	}
	s := string(src)

	// Inspect the RETURN STATEMENT for each case by splitting on statements, not
	// by index arithmetic — the first version used strings.LastIndex and panicked
	// on -1 when the phrase appeared before any `return`.
	idStmt, nameStmt := "", ""
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "return nil,") {
			continue
		}
		if strings.Contains(t, "not found (must be spawn-managed)") {
			idStmt = t
		}
		if strings.Contains(t, "no instance found with name:") {
			nameStmt = t
		}
	}

	if idStmt == "" {
		t.Fatal("the unknown-instance-ID error is gone")
	}
	if strings.Contains(idStmt, "notFoundError") {
		t.Errorf("an unknown instance ID must NOT carry ErrInstanceNotFound — `spawn terminate "+
			"i-doesnotexist` would exit 0, letting a typo read as a successful termination. "+
			"test/e2e's negative matrix pins this.\n  got: %s", idStmt)
	}

	if nameStmt == "" {
		t.Fatal("the unknown-name error is gone")
	}
	if !strings.Contains(nameStmt, "notFoundError") {
		t.Errorf("an unknown NAME must carry ErrInstanceNotFound so terminate can treat it as "+
			"success — that is the case reported in #648\n  got: %s", nameStmt)
	}
}
