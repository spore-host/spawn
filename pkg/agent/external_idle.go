package agent

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/spore-host/spawn/pkg/taskproto"
)

// ExternalIdle is an idle signal that replaces spored's own when an instance's
// only job is a service spored runs itself (#1515): the egress proxy is idle
// when nothing has been proxied for Timeout, whatever its CPU, network or
// sessions say. Its own traffic (OS updates, the spawn:* tag writes) would
// otherwise keep it busy, and a proxy that a CPU idle check stopped would bill
// its volume and still be found by the next launch.
//
// Reaching Timeout always terminates the instance, through the same terminate
// the TTL uses: a proxy has nothing to come back to, and TTL-always-terminates
// (#72) holds for it as for anything else.
type ExternalIdle struct {
	// Name says what the signal measures, in the log and the terminate reason.
	Name string
	// LastActivity is when the service was last used.
	LastActivity func() time.Time
	// Timeout is how long LastActivity may lie in the past.
	Timeout time.Duration
}

// SetExternalIdle makes e the instance's idle signal, in place of spored's
// own, from the next monitor tick. Call it before Monitor.
func (a *Agent) SetExternalIdle(e ExternalIdle) {
	a.externalIdle.Store(&e)
}

// PutInstanceTags writes tags on this instance through spored's own tag path
// (EC2 CreateTags, which the spored role grants on the instance itself), the
// one the spawn:ready-* and spawn:dns-status tags use. The egress proxy
// publishes its activity with it (#1515 phase 5).
func (a *Agent) PutInstanceTags(ctx context.Context, tags map[string]string) error {
	if a.tagger == nil || a.identity == nil || a.identity.InstanceID == "" {
		return errors.New("no instance to tag")
	}
	return a.tagger.putTags(ctx, a.identity.InstanceID, tags)
}

// externalIdleNow is the clock the external idle check reads.
func (a *Agent) externalIdleNow() time.Time {
	if a.externalIdleClock != nil {
		return a.externalIdleClock()
	}
	return time.Now()
}

// checkExternalIdle terminates the instance once e's activity is more than
// e.Timeout old. Like the TTL's terminate it is retried on every tick until
// the provider accepts it, and announced only once it has.
func (a *Agent) checkExternalIdle(ctx context.Context, e *ExternalIdle) {
	idle := a.externalIdleNow().Sub(e.LastActivity())
	if e.Timeout <= 0 || idle < e.Timeout {
		return
	}
	log.Printf("%s idle for %v (timeout %v): terminating", e.Name, idle.Round(time.Second), e.Timeout)
	// ttl_expired is spore-bot's one "has terminated" event, as for the idle
	// terminate in idleTimeoutReached.
	if err := a.terminate(ctx, e.Name+" idle timeout", taskproto.ExitIdleTimeout); err == nil {
		a.notifier.Notify(ctx, "ttl_expired", e.Name+" idle timeout reached")
	}
}
