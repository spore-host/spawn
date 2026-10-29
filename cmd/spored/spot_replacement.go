package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/spore-host/spawn/pkg/agent"
	"github.com/spore-host/spawn/pkg/launchconfig"
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
// notification settings: the replacement has no tags of its own, and the
// request carries the original's. Since #1359 the settings themselves are in
// the launch config object the request's spawn:config names, which this
// replacement, booted with a fresh root volume, fetches with fetch (the
// object outlives its instance for this, launchconfig.ObjectMaxDays).
func replacementNotifier(identity *provider.Identity, fetch launchSettingsFetcher) func(context.Context, spotrequest.Detection) {
	return func(ctx context.Context, d spotrequest.Detection) {
		// Name the original: the bot DMs the users registered for it, and
		// nobody registered this replacement's id (#1007).
		n := agent.NewNotifier(notifyConfigFromTags(ctx, d.RequestTags, fetch), identity).WithOriginalInstance(d.OriginalInstanceID)
		n.Notify(ctx, "spot_replacement_retired", fmt.Sprintf(
			"replacement of %s launched by leaked Spot request %s; cancelling the request and terminating it",
			d.OriginalInstanceID, d.RequestID))
	}
}

// launchSettingsFetcher fetches and verifies the launch config object a sha
// names: provider.EC2Provider.FetchLaunchSettings in production.
type launchSettingsFetcher func(ctx context.Context, sha string) (map[string]string, error)

// notifyConfigFromTags reads the notification settings for a set of spawn
// tags: the DNS name and account from the tags, the rest from the launch
// config object their spawn:config names. With no object, or one that cannot
// be fetched (said in the log), the notification goes nowhere, which is what
// an original with no notification settings gets too.
func notifyConfigFromTags(ctx context.Context, tags map[string]string, fetch launchSettingsFetcher) *provider.Config {
	cfg := &provider.Config{
		DNSName:       tags[tagprefix.Tag("dns-name")],
		AccountBase36: tags[tagprefix.Tag("account-base36")],
	}
	sha := tags[tagprefix.Tag(launchconfig.TagSuffix)]
	if sha == "" || fetch == nil {
		return cfg
	}
	settings, err := fetch(ctx, sha)
	if err != nil {
		log.Printf("Warning: the original's launch config object %s could not be read (%v); the retirement is not notified", sha, err)
		return cfg
	}
	cfg.NotifyURL = settings[launchconfig.NotifyURL]
	cfg.SlackWorkspaceID = settings[launchconfig.SlackWorkspaceID]
	cfg.NotifyCommand = settings[launchconfig.NotifyCommand]
	cfg.NotifyPlatform = settings[launchconfig.NotifyPlatform]
	return cfg
}
