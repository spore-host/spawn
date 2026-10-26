package aws

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spore-host/spawn/pkg/spotrequest"
	"github.com/spore-host/spawn/pkg/ttl"
	truffleaws "github.com/spore-host/truffle/pkg/aws"
)

// LaunchTagger renders the instance tags [Client.Launch] writes, without
// launching (#1330).
//
// A detached sweep's members are launched by the sweep-orchestrator Lambda, which
// never sees a LaunchConfig. It used to write five tags of its own, so a member
// got none of the spawn:* configuration spored and the bootstrap read: a row's
// command, idle timeout, cost limit and completion action did nothing. The CLI
// now renders each member's tags with this, the code Launch itself uses, and the
// Lambda launches with them, re-anchored at the real launch time by
// [AnchorLaunchTags].
//
// The caller identity and account name are resolved once, and each (region,
// instance type, OS) price once, so a sweep of many members does not repeat the
// lookups per member. Not safe for concurrent use.
type LaunchTagger struct {
	c                          *Client
	accountID, userARN, accSlg string
	pricer                     onDemandPricer // truffle in production
	prices                     map[string]resolvedPrice
}

// resolvedPrice is a memoized resolvePricePerHour outcome. price 0 means no
// price was available; that is only reused for a config with no cost limit,
// which resolvePricePerHour would have accepted too.
type resolvedPrice struct {
	price  float64
	source string
}

// NewLaunchTagger resolves the caller identity and account name the tags carry.
func (c *Client) NewLaunchTagger(ctx context.Context) (*LaunchTagger, error) {
	accountID, userARN, err := c.GetCallerIdentityInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get caller identity: %w", err)
	}
	// Best-effort: empty when unset or not permitted, in which case the DNS
	// updater falls back to base36 (#121).
	slug := slugifyDNSLabel(c.GetAccountName(ctx))
	return &LaunchTagger{c: c, accountID: accountID, userARN: userARN, accSlg: slug,
		pricer: truffleaws.NewClientFromConfig(c.cfg), prices: map[string]resolvedPrice{}}, nil
}

// Tags returns the instance tags Launch would write for lc, launched now. The
// instants in them (see [AnchorLaunchTags]) are this call's.
func (t *LaunchTagger) Tags(ctx context.Context, lc LaunchConfig) ([]types.Tag, error) {
	if err := lc.ValidateDurations(); err != nil {
		return nil, err
	}
	if lc.SpawnVersion == "" {
		lc.SpawnVersion = CallerVersion
	}
	t.c.prepareHibernation(ctx, &lc)
	return t.tags(ctx, &lc)
}

// tags is the part of Launch that turns a prepared config into its tags. It
// resolves lc's on-demand price into lc, as Launch needs it to.
func (t *LaunchTagger) tags(ctx context.Context, lc *LaunchConfig) ([]types.Tag, error) {
	// Windows is priced differently (#1058), so it must be known here. TargetOS
	// is set by the CLI launch path but not by every caller (burst, app, other
	// repos), and "" means Linux. When a cost limit makes the answer matter and
	// TargetOS does not give it, ask the AMI: one DescribeImages, only then.
	windows := launchIsWindowsForPricing(ctx, *lc, t.c.IsWindowsAMI)
	key := fmt.Sprintf("%s|%s|%t", lc.Region, lc.InstanceType, windows)
	if p, ok := t.prices[key]; ok && lc.PricePerHour == 0 && (p.price > 0 || lc.CostLimit <= 0) {
		lc.PricePerHour, lc.PriceSource = p.price, p.source
	} else {
		supplied := lc.PricePerHour != 0
		if err := resolvePricePerHour(ctx, t.pricer, lc, windows); err != nil {
			return nil, err
		}
		if !supplied {
			t.prices[key] = resolvedPrice{price: lc.PricePerHour, source: lc.PriceSource}
		}
	}
	return BuildLaunchTags(*lc, t.accountID, t.userARN, t.accSlg)
}

// launchAnchoredTags are the tags whose values are instants buildTags fixes at
// the launch. [AnchorLaunchTags] rewrites exactly these.
const (
	launchTimeTag       = "spawn:launch-time"
	ttlTag              = "spawn:ttl"
	ttlDeadlineTag      = "spawn:ttl-deadline"
	jobArrayCreatedTag  = "spawn:job-array-created"
	spotValidUntilTagID = spotrequest.ValidUntilTag
)

// AnchorLaunchTags returns tags, rendered by a [LaunchTagger] at some earlier
// time, as they would have been rendered at launchAt: spawn:launch-time is
// launchAt, spawn:ttl-deadline is launchAt plus spawn:ttl, and a persistent Spot
// request's spawn:spot-valid-until and spawn:job-array-created follow. Every
// other tag is unchanged.
//
// The TTL counts from the real launch, not from when the CLI rendered the tags:
// a detached sweep's member can launch hours after `spawn launch` returned, and
// a scheduled one days after `spawn schedule create`.
//
// A spawn:ttl that cannot be parsed is an error, never a member without a
// deadline (#990). With no spawn:ttl there is no deadline, as for any launch.
func AnchorLaunchTags(tags []types.Tag, launchAt time.Time) ([]types.Tag, error) {
	launchAt = launchAt.UTC()
	out := make([]types.Tag, 0, len(tags)+1)
	var deadline time.Time
	for _, tg := range tags {
		if aws.ToString(tg.Key) != ttlTag {
			continue
		}
		d, err := ttl.ParseTTL(aws.ToString(tg.Value))
		if err != nil {
			return nil, fmt.Errorf("spawn:ttl %q cannot be turned into a spawn:ttl-deadline: %w", aws.ToString(tg.Value), err)
		}
		deadline = launchAt.Add(d)
	}
	stamp := func(t time.Time) *string { return aws.String(t.Format(time.RFC3339)) }
	wroteDeadline := false
	for _, tg := range tags {
		k := aws.ToString(tg.Key)
		v := tg.Value
		switch k {
		case launchTimeTag, jobArrayCreatedTag:
			v = stamp(launchAt)
		case ttlDeadlineTag:
			if deadline.IsZero() {
				continue // a deadline with no TTL to derive it from is not this launch's
			}
			v, wroteDeadline = stamp(deadline), true
		case spotValidUntilTagID:
			v = stamp(spotrequest.ValidUntil(deadline, launchAt))
		}
		out = append(out, types.Tag{Key: aws.String(k), Value: v})
	}
	if !deadline.IsZero() && !wroteDeadline {
		out = append(out, types.Tag{Key: aws.String(ttlDeadlineTag), Value: stamp(deadline)})
	}
	return out, nil
}

// BuildLaunchTags is the tag builder [Client.Launch] uses, for a config whose
// price is already resolved and an identity the caller supplies. A
// [LaunchTagger] resolves both and calls it; it is exported so a caller that
// renders tags ahead of a launch can be checked against Launch's own builder.
func BuildLaunchTags(lc LaunchConfig, accountID, userARN, accountNameSlug string) ([]types.Tag, error) {
	return buildTags(lc, accountID, userARN, accountNameSlug)
}

// TagValues is [LaunchTagger.Tags] as key to value; buildTags never repeats a
// key, so nothing is lost.
func (t *LaunchTagger) TagValues(ctx context.Context, lc LaunchConfig) (map[string]string, error) {
	tags, err := t.Tags(ctx, lc)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(tags))
	for _, tg := range tags {
		out[aws.ToString(tg.Key)] = aws.ToString(tg.Value)
	}
	return out, nil
}
