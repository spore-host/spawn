package main

import (
	"fmt"
	"log"
	"strconv"
	"sync"

	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/launcher"
)

// Legacy params: members from a CLI that rendered no bootstrap or tags (#1368).
//
// A spawn CLI at v0.146.0 or older uploads six launch keys per member (ami,
// iam_role, instance_type, key_name, spot, ttl) and nothing else this Lambda
// reads: no userDataParam and no tagsParam. A schedule such a CLI created
// replays those params, as uploaded, on every firing (scheduler-handler's
// downloadParams, then prepareSweep), so it never gains them either. This
// Lambda is shared, hosted infrastructure, and refusing those members would
// break every such sweep and schedule the moment it is redeployed.
//
// So for those params this Lambda renders the member's bootstrap and tags
// itself, with the same builders a CLI launch uses: launcher.BuildUserData,
// whose stub fetches and verifies the body this spawn release published
// (#1274), and spawnaws.BuildLaunchTags. The member runs spored, with its
// in-instance TTL, idle and cost enforcement, and its heartbeat satisfies
// #1313's check. It carries the tags the reaper, `spawn list`, `spawn cost` and
// the dashboard read from any member.
//
// Its inputs are the six keys and the sweep record. Everything else in a row
// is ignored, as v0.146.0's orchestrator ignored it: such a CLI never rendered
// a command, plugins or lifecycle settings for a member, so there are none to
// carry. Rules:
//
//   - A member with no ttl gets the 1h idle timeout a CLI gives any sweep
//     member with no ttl and no idle_timeout. Its idle action is stop, as the
//     CLI's, except on Spot, where it is terminate: this Lambda makes one-time
//     Spot requests, which EC2 cannot stop.
//   - A member with no iam_role gets spored-instance-profile (the fallback in
//     buildRunInstancesInput), not the spawnd-role v0.146.0's orchestrator
//     defaulted to. spawnd-role's policy (scripts/setup-spawnd-iam-role.sh)
//     has no ec2:CreateTags, so spored could never write its heartbeat and
//     #1313 would terminate the member five minutes after launch.
//   - The bootstrap is Linux's. A v0.146.0 CLI did not record the member's OS.
//   - No spawn:price-per-hour: pricing a member needs the Price List API,
//     which this Lambda's role cannot call, and legacy params carry no cost
//     limit for a price to enforce.
//
// Params that carry both keys take the strict path unchanged. Params carrying
// exactly one are refused: every CLI that writes either writes both, so one
// alone is not legacy params but damaged ones, and pairing a CLI's bootstrap
// with tags rendered here (or the reverse) would boot a member whose bootstrap
// reads tags it was not rendered against.

// Version is the spawn release this Lambda was built from, injected at build
// time by scripts/deploy-sweep-orchestrator.sh (-X main.Version=), which
// refuses to deploy anything but a clean release tag whose bootstrap body is
// published. Empty in a plain `go build`.
var Version string

func init() {
	applyVersion()
}

// applyVersion hands Version to pkg/launcher, so the stub in a legacy member's
// user data fetches this release's published body. pkg/launcher cannot read it
// otherwise: this module replaces spawn with ../.., and a replaced spawn names
// no release (launcher.PublishedBodyVersion). With no release, the body is
// inlined, as for any dev build.
func applyVersion() {
	launcher.CallerVersion = Version
}

// legacyBootstrapLogged records the sweeps already logged as launching from
// legacy params, once per sweep in an execution environment.
var legacyBootstrapLogged sync.Map

// withMemberBootstrap returns config with the member's bootstrap and tags: as
// the CLI rendered them when it did, rendered here for legacy params, and an
// error for params carrying only one of the two.
func withMemberBootstrap(state *SweepRecord, config map[string]interface{}, paramIndex int, instanceType string) (map[string]interface{}, error) {
	hasUserData := getStringParam(config, userDataParam, "") != ""
	hasTags := len(getMapParam(config, tagsParam)) > 0
	switch {
	case hasUserData && hasTags:
		return config, nil
	case hasUserData || hasTags:
		present, absent := userDataParam, tagsParam
		if hasTags {
			present, absent = tagsParam, userDataParam
		}
		return nil, fmt.Errorf("param set %d carries %s but no %s; every spawn that renders one renders both, so "+
			"these params are damaged rather than from an older spawn, and it was not launched. Re-run the sweep, "+
			"or re-create the schedule, with a current spawn (#1368)", paramIndex, present, absent)
	}

	cfg := legacyLaunchConfig(state, config, paramIndex, instanceType)
	userData, err := launcher.BuildUserData(cfg, launcher.Options{})
	if err != nil {
		return nil, fmt.Errorf("param set %d: render spored bootstrap: %w; it was not launched", paramIndex, err)
	}
	tags, err := spawnaws.BuildLaunchTags(cfg, state.AWSAccountID, state.UserID, "")
	if err != nil {
		return nil, fmt.Errorf("param set %d: render instance tags: %w; it was not launched", paramIndex, err)
	}
	logLegacyBootstrap(state)

	out := make(map[string]interface{}, len(config)+2)
	for k, v := range config {
		out[k] = v
	}
	out[userDataParam] = userData
	rendered := make(map[string]interface{}, len(tags))
	for _, t := range tags {
		rendered[*t.Key] = *t.Value
	}
	out[tagsParam] = rendered
	return out, nil
}

// legacyLaunchConfig is the launch config a legacy member's bootstrap and tags
// are rendered from: its keys, the sweep record, and the per-member defaults a
// CLI applies. Its iam_role is not among them: buildRunInstancesInput puts it,
// or the spored-instance-profile fallback, on RunInstances, and neither
// builder reads it.
func legacyLaunchConfig(state *SweepRecord, config map[string]interface{}, paramIndex int, instanceType string) spawnaws.LaunchConfig {
	cfg := spawnaws.LaunchConfig{
		InstanceType: instanceType,
		AMI:          getStringParam(config, "ami", ""),
		KeyName:      getStringParam(config, "key_name", ""),
		Spot:         getBoolParam(config, legacySpotParam, false),
		TTL:          getStringParam(config, "ttl", ""),
		Name:         state.SweepName + "-" + strconv.Itoa(paramIndex),
		TargetOS:     "linux",
		Username:     launcher.DefaultUsername,
		SweepID:      state.SweepID,
		SweepName:    state.SweepName,
		SweepIndex:   paramIndex,
		SweepSize:    state.TotalParams,
		SpawnVersion: Version,
	}
	if cfg.TTL == "" {
		cfg.IdleTimeout = "1h"
		cfg.TerminateOnIdle = cfg.Spot
	}
	return cfg
}

// logLegacyBootstrap logs, once per sweep, that its members' bootstrap and
// tags are rendered here.
func logLegacyBootstrap(state *SweepRecord) {
	if _, done := legacyBootstrapLogged.LoadOrStore(state.SweepID, true); done {
		return
	}
	body := launcher.PublishedBodyVersion()
	if body == "" {
		body = "none: the body is inline, this is not a release build"
	}
	log.Printf("Sweep %s: params carry no %s or %s (a spawn CLI at v0.146.0 or older, or a schedule one created), "+
		"so this orchestrator renders its members' spored bootstrap and tags from their ami, instance_type, "+
		"key_name, iam_role, spot and ttl (published bootstrap body: %s). Re-run the sweep, or re-create the "+
		"schedule, with a current spawn to carry its other settings (#1368)", state.SweepID, userDataParam, tagsParam, body)
}
