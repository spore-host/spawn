// AWS's 50-tag limit, enforced before a launch rather than by RunInstances (#1336).

package aws

import (
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// TagLimitError is the refusal of a launch whose tags would not fit in AWS's
// per-resource limit (#1336). RunInstances rejects such a launch outright; it
// does not truncate. buildTags returns this instead, and
// [LaunchConfig.ValidateTagCount] returns it before any AWS call, so the launch
// fails in the shell and not after its security groups, IAM role or FSx were
// set up.
//
// It names what the user can drop. The other tags are spawn's lifecycle
// contract with spored (spawn:ttl, spawn:cost-limit, ...), which is never
// dropped to make room: an instance without them bills open-endedly.
type TagLimitError struct {
	// Count is the number of tags the instance would carry, counting the
	// ones spored and spawn add after RunInstances (Spored, PostLaunch).
	Count int
	// Limit is AWS's per-resource tag limit.
	Limit int
	// CustomTags is the number of custom tags (--tag, and the team and Strata
	// tags the CLI adds the same way) in Count.
	CustomTags int
	// SporedSettings are the optional spored settings that each added tags,
	// as "--flag (spawn:key)", in the order they were added.
	SporedSettings []string
	// Spored are the tags spored writes on the instance by itself once it runs
	// (its heartbeat, the compute-seconds the cost limit survives a stop/start
	// with, the cost footprint, ...), counted in Count because the launch keeps
	// a slot free for each (#1355). A write past the limit fails whole, so
	// without them the cost clock resets on every resume.
	Spored []string
	// PostLaunch are the tags spawn writes on the instance after RunInstances
	// (a Spot instance's price, an ephemeral FSx's mount tags), counted in
	// Count because they need room too.
	PostLaunch []string
}

func (e *TagLimitError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "refusing to launch: the instance would carry %d tags and AWS allows %d per resource, "+
		"so RunInstances would reject it (#1336). Drop at least %d of: ", e.Count, e.Limit, e.Count-e.Limit)
	var droppable []string
	if e.CustomTags > 0 {
		droppable = append(droppable, fmt.Sprintf("the %d custom --tag value(s)", e.CustomTags))
	}
	if len(e.SporedSettings) > 0 {
		droppable = append(droppable, "the optional spored settings "+strings.Join(e.SporedSettings, ", "))
	}
	if e.CustomTags+len(e.SporedSettings) < e.Count-e.Limit {
		// Not enough on its own: name what else takes a slot (#1355).
		droppable = append(droppable, "the launch options that each add a spawn: tag or a slot kept for one "+
			"(a job array, webhooks, notifications, storage, DNS, a web app, Spot)")
	}
	b.WriteString(strings.Join(droppable, "; "))
	b.WriteString(". The remaining spawn: tags are what spored enforces the TTL, idle and cost limits from")
	if len(e.Spored) > 0 {
		fmt.Fprintf(&b, ". Counted: %d slot(s) spored needs for the tags it writes once running (%s)",
			len(e.Spored), strings.Join(e.Spored, ", "))
	}
	if len(e.PostLaunch) > 0 {
		sep := ". Counted: "
		if len(e.Spored) > 0 {
			sep = "; and "
		}
		fmt.Fprintf(&b, "%s%d tag(s) spawn writes after launch (%s)", sep, len(e.PostLaunch), strings.Join(e.PostLaunch, ", "))
	}
	return b.String()
}

// sporedSettingFlags maps each tag an optional spored setting writes (#1308)
// to the flag that sets it. The rest of those settings (--idle-cpu-threshold,
// --metrics-port/-bind/-path, --tracing, --tracing-sampling) are in the launch
// config object and take no tag (#1359); --metrics itself stays one, as
// Prometheus' EC2 discovery filters on it.
var sporedSettingFlags = map[string]string{
	"spawn:metrics-enabled": "--metrics",
}

// launchTagLimit is how many tags RunInstances may be given for c, whose tag
// set so far is tags: AWS's limit less the reserve kept for tags written after
// the launch (tagReserve, #1355).
func launchTagLimit(c LaunchConfig, tags []types.Tag) int {
	return awsMaxTagsPerResource - tagReserveSize(c, tags)
}

// checkTagLimit refuses tags, the set before sweep parameters, when it does
// not leave the reserve free. customTags is how many of them came from c.Tags.
func checkTagLimit(c LaunchConfig, tags []types.Tag, customTags int) error {
	spored, post := tagReserve(c, tags)
	if len(tags)+len(spored)+len(post) <= awsMaxTagsPerResource {
		return nil
	}
	var settings []string
	seen := map[string]bool{}
	for _, t := range tags {
		flag, ok := sporedSettingFlags[aws.ToString(t.Key)]
		if !ok {
			continue
		}
		if seen[flag] {
			// --tracing's second tag.
			for i, s := range settings {
				if strings.HasPrefix(s, flag+" (") {
					settings[i] = strings.TrimSuffix(s, ")") + ", " + aws.ToString(t.Key) + ")"
				}
			}
			continue
		}
		seen[flag] = true
		settings = append(settings, flag+" ("+aws.ToString(t.Key)+")")
	}
	return &TagLimitError{
		Count:          len(tags) + len(spored) + len(post),
		Limit:          awsMaxTagsPerResource,
		CustomTags:     customTags,
		SporedSettings: settings,
		Spored:         spored,
		PostLaunch:     post,
	}
}

// Stand-ins for what [Client.Launch] resolves with AWS calls, used by
// ValidateTagCount to count tags before making any.
const (
	preflightAccountID   = "000000000000"
	preflightUserARN     = "arn:aws:iam::000000000000:user/preflight"
	preflightAccountSlug = "preflight"
	preflightPriceSource = "preflight"
)

// ValidateTagCount refuses, with a [*TagLimitError], a launch whose tags would
// not fit in AWS's 50-tag limit, without any AWS call (#1336). Like
// ValidateDurations, a caller that creates anything before [Client.Launch]
// (security groups, IAM, FSx, staged files) calls it first.
//
// It counts the tags Launch would write, assuming the most the AWS lookups
// can add: an account name, and an on-demand price and its source when the
// config has no price yet. Anything else the caller fills in later (an FSx it
// has not created yet, a resolved target OS) it fills in with a stand-in first.
// Sweep parameters are not counted: they are dropped to fit, never refused.
func (c LaunchConfig) ValidateTagCount() error {
	p := c
	p.Parameters = nil
	if p.SpawnVersion == "" {
		p.SpawnVersion = CallerVersion
	}
	if p.PricePerHour == 0 {
		p.PricePerHour, p.PriceSource = 1, preflightPriceSource
	}
	if p.HibernationRequested() {
		p.Hibernate = true
	}
	_, err := buildTags(p, preflightAccountID, preflightUserARN, preflightAccountSlug)
	return err
}

// fitVolumeTags returns the part of add that fits on a volume already carrying
// existing, in order, and the keys of the rest. A key the volume already has
// is overwritten by CreateTags and takes no new slot.
func fitVolumeTags(existing, add []types.Tag) (fit []types.Tag, dropped []string) {
	have := make(map[string]bool, len(existing))
	for _, t := range existing {
		have[aws.ToString(t.Key)] = true
	}
	room := awsMaxTagsPerResource - len(have)
	for _, t := range add {
		k := aws.ToString(t.Key)
		if have[k] {
			fit = append(fit, t)
			continue
		}
		if room <= 0 {
			dropped = append(dropped, k)
			continue
		}
		room--
		have[k] = true
		fit = append(fit, t)
	}
	return fit, dropped
}
