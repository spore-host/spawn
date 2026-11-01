package progress

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// shownBy prints err on a fresh progress tracker, as a failed step does.
func shownBy(t *testing.T, err error) {
	t.Helper()
	p := NewProgress()
	p.SetErrorOutput(io.Discard)
	p.Start("step")
	p.Error("step", err)
}

// #1473: what the root printer prints of a returned error is decided by
// which printed error the returned one carries (identity), not by its text.
func TestUnshownMatchesThePrintedErrorByIdentity(t *testing.T) {
	inner := errors.New("api error InvalidSubnet: subnet-0abc is in another VPC (#1473 identity)")
	shownBy(t, inner)

	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"the printed error itself": {inner, ""},
		"wrapped in context": {fmt.Errorf("failed to create FSx filesystem: %w", inner),
			"failed to create FSx filesystem: " + ShownRef},
		"wrapped twice": {fmt.Errorf("launch failed after instance i-1 was created; terminated it: %w", fmt.Errorf("failed to create FSx filesystem: %w", inner)),
			"launch failed after instance i-1 was created; terminated it: failed to create FSx filesystem: " + ShownRef},
		"in the middle": {fmt.Errorf("tagging failed (%w) AND deleting failed: boom", inner),
			"tagging failed (" + ShownRef + ") AND deleting failed: boom"},
		"joined": {errors.Join(errors.New("first"), inner), "first\n" + ShownRef},
		// The same words, a different error: nothing printed it.
		"same text, another error": {errors.New(inner.Error()), inner.Error()},
		"same text inside another error": {fmt.Errorf("ctx: %w", errors.New(inner.Error())),
			"ctx: " + inner.Error()},
		// Carried, but not verbatim: printed whole rather than lose any of it.
		"carried, reformatted": {fmt.Errorf("ctx: %q: %w", "x", wrapsQuietly{inner}),
			`ctx: "x": reformatted`},
	} {
		t.Run(name, func(t *testing.T) {
			if got := Unshown(tc.err); got != tc.want {
				t.Errorf("Unshown = %q, want %q", got, tc.want)
			}
		})
	}
}

// wrapsQuietly carries an error without including its text.
type wrapsQuietly struct{ err error }

func (w wrapsQuietly) Error() string { return "reformatted" }
func (w wrapsQuietly) Unwrap() error { return w.err }

// A printed error that itself wraps an earlier printed one is replaced whole,
// not around the inner one.
func TestUnshownReplacesTheOutermostPrintedError(t *testing.T) {
	inner := errors.New("inner failure (#1473 nested)")
	outer := fmt.Errorf("step context: %w", inner)
	shownBy(t, inner)
	shownBy(t, outer)
	if got, want := Unshown(fmt.Errorf("caller: %w", outer)), "caller: "+ShownRef; got != want {
		t.Errorf("Unshown = %q, want %q", got, want)
	}
}

// A quiet tracker (-o json) prints nothing, so it records nothing and the
// root printer prints the whole error.
func TestQuietProgressRecordsNothing(t *testing.T) {
	err := errors.New("quiet failure (#1473)")
	p := NewQuietProgress()
	var out strings.Builder
	p.SetErrorOutput(&out)
	p.Start("step")
	p.Error("step", err)
	if out.Len() != 0 {
		t.Fatalf("quiet tracker printed %q", out.String())
	}
	if got := Unshown(fmt.Errorf("ctx: %w", err)); got != "ctx: "+err.Error() {
		t.Errorf("Unshown = %q, want the whole error", got)
	}
}
