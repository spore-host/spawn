package main

import (
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
)

// A detached member's instance tags (#1330).
//
// This Lambda used to write five tags of its own: spawn:sweep-id,
// spawn:sweep-index, Name, spawn:managed and, for a row with a ttl,
// spawn:ttl-deadline. spored and the bootstrap read everything else from tags
// it never wrote, so a member's command (spawn:command), its spawn:param:*
// environment, idle timeout, cost limit and completion action all did nothing,
// and a member with no ttl had no bound at all.
//
// The tags are not built here. The CLI renders them per member with
// aws.LaunchTagger, the code a foreground launch uses, after the foreground's
// per-member defaults (the 1h idle timeout for a member with no ttl among them),
// and ships them under tagsParam (cmd/launch_sweep_detached_bootstrap.go). This
// Lambda re-anchors their launch instants at the real launch, so a member's TTL
// counts from when it started rather than from when the CLI ran, and sets the
// few it owns. Legacy params, from a CLI that rendered none, get theirs from
// the same tag builder here (legacy.go, #1368).

// tagsParam is the param key carrying a member's rendered tags: a JSON object,
// key to value.
const tagsParam = "spawn_tags"

// getMapParam returns params[key] as a JSON object, or nil.
func getMapParam(params map[string]interface{}, key string) map[string]interface{} {
	m, _ := params[key].(map[string]interface{})
	return m
}

// memberTags is the tag set member paramIndex launches with at now: the CLI's,
// with spawn:launch-time, spawn:ttl-deadline and the other launch instants
// anchored at now (spawnaws.AnchorLaunchTags), and the tags this Lambda owns set
// from the sweep record: which sweep (a schedule's runs get their ID only when
// the schedule fires) and which member, and spawn:managed, the rail the TTL
// reaper's terminate is conditioned on.
//
// Legacy params' tags are rendered before this, by withMemberBootstrap
// (#1368). A member still without tags is refused rather than launched with
// too few: it would run its bootstrap with no command and none of its limits.
func memberTags(state *SweepRecord, config map[string]interface{}, paramIndex int, now time.Time) ([]ec2types.Tag, error) {
	raw := getMapParam(config, tagsParam)
	if len(raw) == 0 {
		return nil, fmt.Errorf("param set %d carries no instance tags (%s), so its instance would run none of its "+
			"command, idle timeout, cost limit or completion settings; it was not launched (#1330)", paramIndex, tagsParam)
	}
	owned := map[string]string{
		"spawn:managed":     "true",
		"spawn:sweep-id":    state.SweepID,
		"spawn:sweep-name":  state.SweepName,
		"spawn:sweep-index": strconv.Itoa(paramIndex),
	}
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	tags := make([]ec2types.Tag, 0, len(raw)+len(owned))
	for _, k := range keys {
		v, ok := raw[k].(string)
		if !ok {
			return nil, fmt.Errorf("param set %d: instance tag %q is not a string", paramIndex, k)
		}
		if o, mine := owned[k]; mine {
			v = o
		}
		tags = append(tags, ec2types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	for _, k := range []string{"spawn:managed", "spawn:sweep-id", "spawn:sweep-name", "spawn:sweep-index"} {
		if _, present := raw[k]; !present {
			tags = append(tags, ec2types.Tag{Key: aws.String(k), Value: aws.String(owned[k])})
		}
	}
	anchored, err := spawnaws.AnchorLaunchTags(tags, now)
	if err != nil {
		return nil, fmt.Errorf("param set %d: %w; it was not launched", paramIndex, err)
	}
	// The CLI refuses a member over the limit when it renders the tags
	// (#1336); this is the backstop for params it did not render, since
	// RunInstances would reject the member anyway.
	if len(anchored) > maxInstanceTags {
		return nil, fmt.Errorf("param set %d carries %d instance tags, over AWS's %d per resource, so RunInstances "+
			"would reject it; it was not launched (#1336)", paramIndex, len(anchored), maxInstanceTags)
	}
	return anchored, nil
}

// maxInstanceTags is AWS's per-resource tag limit.
const maxInstanceTags = 50
