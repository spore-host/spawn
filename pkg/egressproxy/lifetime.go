package egressproxy

import (
	"fmt"
	"time"

	"github.com/spore-host/spawn/pkg/ttl"
)

// How long a proxy lives (#1515, owner decision 2026-11-03).
//
// The TTL reaper force-terminates every spawn instance older than its max-age
// ceiling, measured from launch, with no tag exemption by design. A proxy
// cannot outlive it, so it does not try: it is launched with a hard deadline
// some margin inside the ceiling, carried in the ordinary spawn:ttl and
// spawn:ttl-deadline tags, which spored and the reaper both already enforce.
// A proxy is never patched in place; the next launch after it nears its
// deadline boots a fresh one.
const (
	// MaxLifeMargin is how far inside the reaper's ceiling a proxy's deadline
	// is, so spored's own terminate, not the reaper's, is what ends it.
	MaxLifeMargin = 24 * time.Hour
	// MaxLife is the longest a proxy lives: the reaper's default ceiling
	// ([ttl.DefaultReaperMaxAge], itself gated against the reaper's constant)
	// less [MaxLifeMargin]. Six days with today's seven-day ceiling.
	MaxLife = ttl.DefaultReaperMaxAge - MaxLifeMargin
	// DefaultIdle is how long a proxy may go with nothing proxied before it
	// terminates: three days.
	DefaultIdle = 72 * time.Hour
	// ReuseMargin is how much life a proxy must have left to be reused. One
	// nearer its deadline is replaced, so a launch's bootstrap never leans on
	// a proxy about to go; the old one keeps serving until its own deadline.
	ReuseMargin = 12 * time.Hour
	// Port is the port spawn's proxies listen on.
	Port = 3128
)

// ValidateIdle refuses an idle period that is not positive or that is longer
// than lifetime, the proxy's whole life (launch to deadline): the deadline
// would always end it first, so the idle setting would be a promise nothing
// keeps.
func ValidateIdle(idle, lifetime time.Duration) error {
	if idle <= 0 {
		return fmt.Errorf("the egress proxy idle period must be positive, not %v", idle)
	}
	if idle > lifetime {
		return fmt.Errorf("the egress proxy idle period %v is longer than the proxy's life of %v, "+
			"which ends at its deadline whatever the idle period says; use at most %v", idle, lifetime, lifetime)
	}
	return nil
}
