package main

import (
	"log"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spore-host/spawn/pkg/tagprefix"
)

// heartbeatStaleAfter is how far behind spawn:last-heartbeat may fall before the
// reaper reports that spored is not checking in.
//
// spored refreshes the tag every tick, throttled to once a minute to match the
// production monitor interval (#497). Ten minutes is ten missed refreshes — well
// clear of a slow tick, an API throttle, or a tag write losing a race, and far
// short of the reaper's own deadline paths.
const heartbeatStaleAfter = 10 * time.Minute

// heartbeatState is what the reaper can tell about spored on one instance.
type heartbeatState int

const (
	// heartbeatUnknown — no spawn:last-heartbeat tag. Either spored has not
	// completed its first tick yet, or this instance was launched by something
	// that does not install spored at all. A detached sweep row is the second
	// case (#725), which is exactly why this is not treated as stale: a missing
	// tag and a lapsed one mean different things and only one is a symptom.
	heartbeatUnknown heartbeatState = iota
	// heartbeatFresh — spored ticked recently.
	heartbeatFresh
	// heartbeatStale — the tag exists but has not moved. spored ran at least
	// once and has since stopped.
	heartbeatStale
)

// classifyHeartbeat reports what the heartbeat tag says about spored.
//
// This is spawn#682's visibility half. The preventable half shipped in v0.121.0
// as a memory-floor warning; this is the part that makes an already-starved
// instance observable.
//
// The reported failure: a 0.5 GiB instance wedged during a package install and
// was still running at TWICE its 6-minute TTL, with no completion record and no
// signal of any kind, until a human noticed and killed it. From outside it was
// indistinguishable from a long-running job.
//
// The signal needed to detect that already existed and nothing read it. spored
// has written spawn:last-heartbeat every tick since #497 — "an always-on
// liveness signal a caller can poll" — and a search of the tree found zero
// consumers. So this adds no producer and no storage: the gap was a reader.
//
// A STALE heartbeat on a running instance means spored started and stopped,
// which is precisely the #682 shape: the box got too starved to run the loop
// that would have enforced its own TTL. It also means the reaper is now that
// instance's only backstop, which is worth saying out loud in the log that the
// operator reads.
func classifyHeartbeat(tags map[string]string, now time.Time) (heartbeatState, time.Duration) {
	raw := tags[tagprefix.Tag("last-heartbeat")]
	if raw == "" {
		return heartbeatUnknown, 0
	}
	ts, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		// An unparseable stamp is not evidence of staleness — it is evidence of a
		// format change. Reporting it as a dead spored would be a false alarm
		// every cycle until someone noticed the real cause.
		return heartbeatUnknown, 0
	}
	age := now.Sub(ts)
	if age < 0 {
		return heartbeatFresh, 0 // clock skew; not a symptom
	}
	if age > heartbeatStaleAfter {
		return heartbeatStale, age
	}
	return heartbeatFresh, age
}

// noteHeartbeat records, and logs once, an instance whose spored has gone quiet.
//
// Deliberately does NOT reap. A stale heartbeat is a diagnosis, not a deadline:
// the instance's lifetime is still governed by spawn:ttl-deadline and the
// reaper's max-age ceiling, which are the right authorities. Terminating on a
// missed heartbeat would kill a box whose spored is merely wedged while the
// workload runs fine — destroying work to tidy a symptom.
//
// What it changes is that the condition stops being invisible. Before this,
// "spored is not running" looked exactly like "the job is still going".
func (r *reaper) noteHeartbeat(inst ec2types.Instance, region, acctLabel string, now time.Time, sum *Summary) {
	if inst.State == nil || inst.State.Name != ec2types.InstanceStateNameRunning {
		return // only a RUNNING instance should be ticking
	}
	tags := tagMap(inst.Tags)
	if tags[tagprefix.Tag("managed")] != "true" {
		return
	}

	state, age := classifyHeartbeat(tags, now)
	switch state {
	case heartbeatFresh:
		sum.HeartbeatFresh++
	case heartbeatUnknown:
		sum.HeartbeatUnknown++
	case heartbeatStale:
		sum.HeartbeatStale++
		id := aws.ToString(inst.InstanceId)
		name := tags["Name"]
		log.Printf("%s %s (%s) in %s/%s: spored has not checked in for %s — its TTL, idle "+
			"and cost limits are NOT being enforced in-instance, so this reaper is the only "+
			"thing that will stop it (#682)",
			sentinelHeartbeatStale, id, name, acctLabel, region, age.Round(time.Minute))
	}
}
