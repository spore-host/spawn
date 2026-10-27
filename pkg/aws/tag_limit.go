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
	// ones spawn adds itself after RunInstances (PostLaunch).
	Count int
	// Limit is AWS's per-resource tag limit.
	Limit int
	// CustomTags is the number of custom tags (--tag, and the team and Strata
	// tags the CLI adds the same way) in Count.
	CustomTags int
	// SporedSettings are the optional spored settings that each added tags,
	// as "--flag (spawn:key)", in the order they were added.
	SporedSettings []string
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
	if len(droppable) == 0 {
		droppable = append(droppable, "the launch options that each add a spawn: tag (webhooks, notifications, storage, DNS)")
	}
	b.WriteString(strings.Join(droppable, "; "))
	b.WriteString(". The remaining spawn: tags are what spored enforces the TTL, idle and cost limits from")
	if len(e.PostLaunch) > 0 {
		fmt.Fprintf(&b, ". Counted: %d tag(s) spawn writes after launch (%s)", len(e.PostLaunch), strings.Join(e.PostLaunch, ", "))
	}
	return b.String()
}

// sporedSettingFlags maps each tag an optional spored setting writes (#1308)
// to the flag that sets it. --tracing writes two.
var sporedSettingFlags = map[string]string{
	"spawn:idle-cpu":         "--idle-cpu-threshold",
	"spawn:metrics-enabled":  "--metrics",
	"spawn:metrics-port":     "--metrics-port",
	"spawn:metrics-bind":     "--metrics-bind",
	"spawn:metrics-path":     "--metrics-path",
	"spawn:tracing-enabled":  "--tracing",
	"spawn:tracing-exporter": "--tracing",
	"spawn:tracing-sampling": "--tracing-sampling",
}

// postLaunchTags are the instance tags spawn's own launch code writes after
// RunInstances, which the tag set RunInstances gets must leave room for:
//
//   - a Spot instance's spawn:spot-price-per-hour (recordLaunchSpotPrice),
//     and on a persistent request spawn:original-instance-id, which the TTL
//     reaper uses to tell a replacement apart (tagOriginalInstance). The
//     request carries the same tag set as the instance, so one slot covers
//     both.
//   - an ephemeral FSx created after the launch (#213): spawn:fsx-pending and
//     its mount point and S3 paths, written by UpdateInstanceTags once the
//     filesystem exists (launchWithProgress, launcher.Provision). A failed
//     write there deletes the filesystem and terminates the instance.
func postLaunchTags(c LaunchConfig) []string {
	var out []string
	if c.Spot {
		out = append(out, SpotPriceTag)
	}
	if c.FSxLustreCreate && c.FSxLustreID == "" && c.FSxPending == "" {
		out = append(out, "spawn:fsx-pending", "spawn:fsx-mount-point", "spawn:fsx-s3-import-path", "spawn:fsx-s3-export-path")
	}
	return out
}

// launchTagLimit is how many tags RunInstances may be given for c: AWS's limit
// less the ones spawn writes after the launch.
func launchTagLimit(c LaunchConfig) int {
	return awsMaxTagsPerResource - len(postLaunchTags(c))
}

// checkTagLimit refuses tags, the set before sweep parameters, when it does
// not fit. customTags is how many of them came from c.Tags.
func checkTagLimit(c LaunchConfig, tags []types.Tag, customTags int) error {
	post := postLaunchTags(c)
	if len(tags)+len(post) <= awsMaxTagsPerResource {
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
		Count:          len(tags) + len(post),
		Limit:          awsMaxTagsPerResource,
		CustomTags:     customTags,
		SporedSettings: settings,
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
