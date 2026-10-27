package cmd

import (
	"context"

	"github.com/spore-host/spawn/pkg/aws"
)

// resolveTeamNameFn is resolveTeamName; tests replace it so no DynamoDB call
// is made.
var resolveTeamNameFn = resolveTeamName

// applyLaunchFlagTags merges `spawn launch --tag` and `--team` into cfg.Tags.
//
// Every launch path calls it: the single launch, the parameter sweep (whose
// members, foreground and detached, start from the config it is applied to) and
// the batch queue. They used to be merged in runLaunch only after the sweep and
// batch-queue paths had already been handed off, so both dropped them silently
// (#1356).
//
// A --tag key with the reserved spawn: prefix is refused (parseKVTags). The
// spawn-managed tags buildTags writes take precedence over cfg.Tags anyway.
// --team sets spawn:team-id, and spawn:team-name when the team resolves.
func applyLaunchFlagTags(ctx context.Context, cfg *aws.LaunchConfig) error {
	if len(launchTags) == 0 && launchTeamID == "" {
		return nil
	}
	userTags, err := parseKVTags(launchTags)
	if err != nil {
		return err
	}
	if cfg.Tags == nil {
		cfg.Tags = make(map[string]string, len(userTags)+2)
	}
	for k, v := range userTags {
		cfg.Tags[k] = v
	}
	if launchTeamID != "" {
		cfg.Tags["spawn:team-id"] = launchTeamID
		// Resolve the team name from DynamoDB for the human-readable tag.
		if teamName, err := resolveTeamNameFn(ctx, launchTeamID); err == nil && teamName != "" {
			cfg.Tags["spawn:team-name"] = teamName
		}
	}
	return nil
}
