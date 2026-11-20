// Package lastmilespawn holds the read-only pieces the spawn CLI's lazy last
// mile is built from (github.com/spore-host/libs/lastmile, libs#78, part of
// the leave-no-trace epic spore-host/spore-host#752).
//
// Nothing here runs by itself. The last mile is wired in the spawn CLI only
// (cmd/lastmile.go, the root command's post-run hook), so it never runs
// inside the programs that import spawn's packages (lagotto, calque,
// spore-host-mcp). This package is where its AWS calls live, as the cmd
// package delegates every AWS call to a pkg/ package (#326/#327):
//
//   - AccountObserver: a middleware that notes the account a
//     GetCallerIdentity reports, so the last mile makes no STS call of its
//     own.
//   - ReadOnlyLifecycle: the lifecycle table's reads, with every write
//     refused, and one refusal by IAM ending the asking.
//   - Residue, Patterns and ResidueChecker: what the self-hosted reaper's
//     self-removal leaves once its one-shot jobs have run (spawn#1655): the
//     jobs' invoke role and their schedule group.
//   - NewReportOnlyDeleter: re-checks a plan, deletes nothing.
//   - InfraSignalsReadable: whether the infra account's activity tables can
//     be read, one input of the FullVisibility decision.
//
// Everything is report-only: no function here writes to AWS.
package lastmilespawn

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

// ErrReportOnly is what every write and delete made through this package
// returns: the last mile is report-only until libs#79 and spawn#1657.
var ErrReportOnly = errors.New("spawn's last mile is report-only: it writes and deletes nothing")

// NotAsked starts the error of a read a Latch answered itself, after an
// earlier read was refused by IAM. A logger may drop lines containing it:
// the first refusal says everything.
const NotAsked = "not asked: an earlier"

// NotPermitted reports whether msg is about a request IAM refused.
func NotPermitted(msg string) bool {
	for _, s := range []string{"AccessDenied", "UnauthorizedOperation", "not authorized to perform"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// Latch remembers the first request IAM refused, so that a caller without a
// grant costs one denied request per run rather than one per region (which
// also keeps CloudTrail quiet). A throttle or any other error does not latch.
type Latch struct {
	// What names what was refused, in the not-asked error ("read of the
	// spore-host-lifecycle table").
	What string

	mu     sync.Mutex
	denied error
}

// Note records err if it is a refusal and none is recorded yet.
func (l *Latch) Note(err error) {
	if err == nil || !NotPermitted(err.Error()) {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.denied == nil {
		l.denied = err
	}
}

// Err is nil until a refusal was noted, then an error starting NotAsked that
// wraps it.
func (l *Latch) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.denied == nil {
		return nil
	}
	what := l.What
	if what == "" {
		what = "request"
	}
	return fmt.Errorf("%s %s was refused (%w)", NotAsked, what, l.denied)
}
