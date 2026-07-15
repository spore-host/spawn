package launchererr_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/spore-host/spawn/pkg/launcher"
	"github.com/spore-host/spawn/pkg/launchererr"
)

// TestErrPostLaunch_AliasIdentity is the load-bearing invariant of spawn#354:
// launcher.ErrPostLaunch is an alias of launchererr.ErrPostLaunch, so an error
// wrapped by the launcher matches when tested against EITHER — a downstream can
// import the dependency-free leaf and errors.Is still works.
func TestErrPostLaunch_AliasIdentity(t *testing.T) {
	if launcher.ErrPostLaunch != launchererr.ErrPostLaunch {
		t.Fatal("launcher.ErrPostLaunch must be the same sentinel as launchererr.ErrPostLaunch")
	}

	// A launcher-shaped wrap (matches how Provision wraps it) must satisfy
	// errors.Is against the leaf sentinel — the whole point of the extraction.
	wrapped := fmt.Errorf("%w: FSx setup failed, terminated instance i-x", launcher.ErrPostLaunch)
	if !errors.Is(wrapped, launchererr.ErrPostLaunch) {
		t.Error("errors.Is(wrapped, launchererr.ErrPostLaunch) = false; want true (leaf must match a launcher-wrapped error)")
	}
	if !errors.Is(wrapped, launcher.ErrPostLaunch) {
		t.Error("errors.Is(wrapped, launcher.ErrPostLaunch) = false; want true (alias must still match)")
	}

	// A non-post-launch error must NOT match.
	if errors.Is(errors.New("some other failure"), launchererr.ErrPostLaunch) {
		t.Error("unrelated error must not match ErrPostLaunch")
	}
}
