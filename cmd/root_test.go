package cmd

import (
	"testing"

	"github.com/spore-host/libs/update"
)

// TestStartUpdateCheckSkipsDevBuilds: a dev build must not consult the release
// feed. libs' semver parser reads "dev" as 0.0.0, so the check would report an
// update available on every command from every source build — including builds
// NEWER than the last release, which is the normal state while developing.
//
// The returned channel must still be safe to receive from (closed, yielding nil)
// because Execute() selects on it unconditionally and Result.HasUpdate() is
// nil-safe.
func TestStartUpdateCheckSkipsDevBuilds(t *testing.T) {
	for _, v := range []string{"dev", "dev+dirty"} {
		called := stubCheckAsync(t, nil)
		ch := startUpdateCheck(v)
		if *called {
			t.Errorf("startUpdateCheck(%q) consulted the release feed; a dev build must not", v)
		}
		select {
		case res := <-ch:
			if res != nil {
				t.Errorf("startUpdateCheck(%q) yielded %+v, want nil (no check performed)", v, res)
			}
			if res.HasUpdate() {
				t.Errorf("startUpdateCheck(%q) reported an update available", v)
			}
		default:
			t.Errorf("startUpdateCheck(%q) did not yield immediately; a dev build must "+
				"not wait on the network", v)
		}
	}
}

// TestStartUpdateCheckRunsForRealVersions: the skip must be NARROW. A real
// version still gets checked — otherwise the "fix" would disable the update
// notice for everyone, a regression dressed up as a guard.
//
// This needs the stub to be meaningful. Both branches hand back a channel that
// yields immediately (libs itself short-circuits under SPORE_NO_UPDATE_CHECK), so
// observing the channel alone cannot distinguish skipped from delegated —
// verified by mutation: widening the condition to `true ||` passed the earlier
// version of this test.
func TestStartUpdateCheckRunsForRealVersions(t *testing.T) {
	want := &update.Result{CurrentVersion: "0.97.0", LatestVersion: "0.98.0"}
	called := stubCheckAsync(t, want)

	ch := startUpdateCheck("0.97.0")
	if !*called {
		t.Fatal("startUpdateCheck did not consult the release feed for a real version")
	}
	if got := <-ch; got != want {
		t.Errorf("startUpdateCheck returned %+v, want the delegated result %+v", got, want)
	}
}

// stubCheckAsync replaces the update-feed call for the duration of the test and
// returns a pointer to a flag reporting whether it was invoked.
func stubCheckAsync(t *testing.T, res *update.Result) *bool {
	t.Helper()
	var called bool
	orig := checkAsync
	t.Cleanup(func() { checkAsync = orig })
	checkAsync = func(tool, current string) <-chan *update.Result {
		called = true
		if tool != "spawn" {
			t.Errorf("checkAsync tool = %q, want spawn (the repo the release feed is read from)", tool)
		}
		ch := make(chan *update.Result, 1)
		ch <- res
		close(ch)
		return ch
	}
	return &called
}
