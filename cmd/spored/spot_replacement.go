package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/spore-host/spawn/pkg/agent"
	"github.com/spore-host/spawn/pkg/provider"
	"github.com/spore-host/spawn/pkg/spotrequest"
	"github.com/spore-host/spawn/pkg/tagprefix"
)

// spotReplacementRetirer is the slice of *provider.EC2Provider the replacement
// check uses, so the loop can be tested with a fake.
type spotReplacementRetirer interface {
	DetectSpotReplacement(ctx context.Context) spotrequest.Detection
	RetireSpotReplacement(ctx context.Context, d spotrequest.Detection, shutdown func(context.Context) error) (spotrequest.RetireOutcome, error)
}

// How often an inconclusive check is retried. At boot the instance profile's
// credentials or EC2's view of the request can lag; a definitive answer either
// way ends the loop at once.
const (
	replacementCheckAttempts = 5
	replacementCheckInterval = 30 * time.Second
	// replacementNotifyGrace lets the fire-and-forget notification (5s send
	// timeout) leave before the OS goes down.
	replacementNotifyGrace = 6 * time.Second
)

// replacementCheck holds the dependencies of checkSpotReplacement.
type replacementCheck struct {
	r spotReplacementRetirer
	// instanceID and region name this instance in the hand-cleanup command,
	// for the case where it can only be stopped (#1080).
	instanceID, region string
	shutdown           func(context.Context) error
	notify             func(ctx context.Context, d spotrequest.Detection)
	sleep              func(ctx context.Context, d time.Duration) bool // false if ctx ended
}

// run checks once at startup (with retries while inconclusive) whether this
// instance is a replacement launched from a leaked spawn persistent Spot request
// (#995), and if so retires it. It returns what it concluded.
//
// Fail safe: only VerdictReplacement acts. Unknown after every attempt does
// nothing, which leaves a real replacement to the TTL reaper's alarmed
// SPOT REPLACEMENT NOT TERMINATED.
func (c replacementCheck) run(ctx context.Context) spotrequest.Detection {
	var d spotrequest.Detection
	for attempt := 1; attempt <= replacementCheckAttempts; attempt++ {
		d = c.r.DetectSpotReplacement(ctx)
		if d.Verdict != spotrequest.VerdictUnknown {
			break
		}
		log.Printf("Spot replacement check inconclusive (attempt %d/%d): %s",
			attempt, replacementCheckAttempts, d.Reason)
		if attempt < replacementCheckAttempts && !c.sleep(ctx, replacementCheckInterval) {
			return d
		}
	}

	switch d.Verdict {
	case spotrequest.VerdictNotReplacement:
		log.Printf("Spot replacement check: not a replacement (%s)", d.Reason)
		return d
	case spotrequest.VerdictUnknown:
		log.Printf("Spot replacement check gave up inconclusive; doing nothing (%s)", d.Reason)
		return d
	}

	log.Printf("==================== SPOT REPLACEMENT DETECTED ====================")
	log.Printf("This instance was launched by spawn's persistent Spot request %s for instance %s,",
		d.RequestID, d.OriginalInstanceID)
	log.Printf("which was terminated outside spawn. It carries no spawn tags (no TTL) and nothing")
	log.Printf("else will end it. spored is cancelling the request and terminating this instance.")
	log.Printf("====================================================================")
	c.notify(ctx, d)
	c.sleep(ctx, replacementNotifyGrace)

	outcome, err := c.r.RetireSpotReplacement(ctx, d, c.shutdown)
	switch {
	case outcome == spotrequest.RetiredTerminated:
		log.Printf("Spot request %s cancelled; this instance is terminating itself", d.RequestID)
	case outcome == spotrequest.RetiredShutdown:
		// The request is cancelled, so nothing more launches, but the instance is
		// not gone: a stopped, untagged instance whose request is cancelled is
		// never started again and nothing ends it (#1080). Say so, with the
		// command, rather than implying it was retired.
		log.Printf("WARNING: Spot request %s cancelled, but %v.", d.RequestID, err)
		log.Printf("WARNING: this role lacks spored's TerminateOnlySelf grant (re-run `spawn launch` once to refresh "+
			"the spored role). End this instance by hand: aws ec2 terminate-instances --region %s --instance-ids %s",
			c.region, c.instanceID)
	default:
		log.Printf("ERROR: could not retire Spot replacement: %v", err)
		log.Printf("ERROR: end it by hand: aws ec2 cancel-spot-instance-requests --region %s --spot-instance-request-ids %s; "+
			"then aws ec2 terminate-instances --region %s --instance-ids %s", c.region, d.RequestID, c.region, c.instanceID)
	}
	return d
}

// sleepCtx waits d, or until ctx ends (false).
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// replacementNotifier sends the notification through the original instance's
// notification settings, which the request carries in its tags: the
// replacement has no tags of its own.
func replacementNotifier(identity *provider.Identity) func(context.Context, spotrequest.Detection) {
	return func(ctx context.Context, d spotrequest.Detection) {
		// Name the original: the bot DMs the users registered for it, and
		// nobody registered this replacement's id (#1007).
		n := agent.NewNotifier(notifyConfigFromTags(d.RequestTags), identity).WithOriginalInstance(d.OriginalInstanceID)
		n.Notify(ctx, "spot_replacement_retired", fmt.Sprintf(
			"replacement of %s launched by leaked Spot request %s; cancelling the request and terminating it",
			d.OriginalInstanceID, d.RequestID))
	}
}

// notifyConfigFromTags reads the notification settings from a set of spawn tags.
func notifyConfigFromTags(tags map[string]string) *provider.Config {
	return &provider.Config{
		NotifyURL:        tags[tagprefix.Tag("notify-url")],
		SlackWorkspaceID: tags[tagprefix.Tag("slack-workspace-id")],
		NotifyCommand:    tags[tagprefix.Tag("notify-command")],
		NotifyPlatform:   tags[tagprefix.Tag("notify-platform")],
		DNSName:          tags[tagprefix.Tag("dns-name")],
		AccountBase36:    tags[tagprefix.Tag("account-base36")],
	}
}
