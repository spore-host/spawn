package agent

import (
	"context"
	"log"
)

// regionVacatedProbe is the region-vacated check (#260): it reports the region
// vacated when no other spawn-managed instance is running or pending at the
// start of a termination nor at its end, so a rapid relaunch (a job array
// rolling, a sweep starting its next member) between the two is not reported
// as an empty region. An unknown count (-1) counts as "not vacated", so it
// never cries wolf.
//
// Before #1456 the check ran after every other wait and then blocked 60s
// between its two counts, so the last spawn instance in a region was billed an
// extra idle minute before each termination, with or without a notifier to
// tell. Now the first count is taken where the termination is decided, the
// second when terminate reaches its user warning, and the settle window is the
// time between: the --completion-delay grace period (on-complete terminate),
// the pre-stop hook, the task-record flush and Cleanup. Nothing waits for it.
type regionVacatedProbe struct {
	cut    chan struct{} // closed by endSettle: take the second count now
	result chan bool     // buffered; true = vacated
	cancel context.CancelFunc
}

// startRegionVacatedProbe starts the check for the termination being decided,
// unless one is already running or there is no notifier to tell: the check
// exists only for the region_vacated notification, so without one it must cost
// nothing (#1456).
func (a *Agent) startRegionVacatedProbe(ctx context.Context) {
	if a.notifier == nil || a.vacatedProbe != nil {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	p := &regionVacatedProbe{cut: make(chan struct{}), result: make(chan bool, 1), cancel: cancel}
	a.vacatedProbe = p
	go func() {
		p.result <- p.run(ctx, a.provider.CountOtherManagedInstances)
	}()
}

func (p *regionVacatedProbe) run(ctx context.Context, count func(context.Context) int) bool {
	if n := count(ctx); n != 0 {
		return false // others remain, or the count is unknown (-1)
	}
	select {
	case <-p.cut:
	case <-ctx.Done():
	}
	select {
	case <-p.cut:
	default:
		return false // abandoned before the settle window ended
	}
	if n := count(ctx); n != 0 {
		log.Printf("region-vacated: re-check found %d other instance(s); not vacated", n)
		return false
	}
	return true
}

// endSettle ends the settle window: the second count is taken now, during the
// shutdown warning wait. Nil-safe.
func (p *regionVacatedProbe) endSettle() {
	if p != nil {
		close(p.cut)
	}
}

// vacated reports the probe's verdict. It cancels the probe first, so a count
// still in flight (a slow DescribeInstances) returns at once as unknown, and
// the verdict is "not vacated", rather than holding up Terminate. Nil-safe: no
// probe is "not vacated".
func (p *regionVacatedProbe) vacated() bool {
	if p == nil {
		return false
	}
	p.cancel()
	return <-p.result
}
