package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
)

// spored on detached sweep members (#1323, #1313).
//
// This Lambda used to launch every member with no UserData, so spored never ran
// on one: no in-instance TTL, idle stop, cost limit or completion handling, and a
// member's only bound was the TTL reaper's next cycle, in the regions it covers.
//
// The bootstrap is not built here. The CLI renders it, with the same
// buildUserData the foreground sweep path uses, and ships it in the uploaded
// params under userDataParam (cmd/launch_sweep_detached_bootstrap.go). Its inputs
// live on the CLI's machine (the public key, --plugin/--config plugins,
// --user-data contents) and its stub names the spawn release that built it.
// Rendering it there makes a detached member's user data the foreground
// member's by construction, rather than a second builder that has to be kept
// in step.
//
// The one exception is legacy params, from a CLI that rendered none: this
// Lambda renders their bootstrap itself, with pkg/launcher, for the release it
// was built from (legacy.go, #1368).

// userDataParam is the param key carrying a member's encoded spored user data,
// exactly as RunInstances takes it. A row's own value overrides the defaults'.
const userDataParam = "spawn_user_data"

// sporedBudgetLinux and sporedBudgetWindows are how long a running member may go
// without spawn:last-heartbeat before it is presumed to have no spored and is
// terminated. The same budgets the CLI's foreground fleets use
// (sporedVerifyTimeout in cmd/launch_posthook.go, #1305); Windows also installs
// the AWS CLI from an MSI before it can fetch spored.
const (
	sporedBudgetLinux   = 5 * time.Minute
	sporedBudgetWindows = 10 * time.Minute
)

// clock is the orchestrator's time, a variable so tests can fix it.
var clock = time.Now

// ec2InstancesAPI is what the polling loops ask of EC2: the seam the spored
// verification tests drive. *ec2.Client satisfies it.
type ec2InstancesAPI interface {
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	TerminateInstances(ctx context.Context, in *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
}

// buildRunInstancesInput composes a member's RunInstances request. Shared by the
// single- and multi-region launch paths, so a member's bootstrap, profile and
// tags cannot depend on which one ran.
//
// A member's bootstrap and tags are the CLI's when its params carry them, and
// rendered here for legacy params, which carry neither (withMemberBootstrap,
// #1368). A member that still has no spored user data is refused rather than
// launched: it would run with no in-instance TTL (#1323), and under #1313 it
// would be terminated five minutes later anyway, after billing for them.
func buildRunInstancesInput(state *SweepRecord, params map[string]interface{}, paramIndex int, instanceType string, now time.Time) (*ec2.RunInstancesInput, error) {
	config, err := withMemberBootstrap(state, params, paramIndex, instanceType)
	if err != nil {
		return nil, err
	}
	userData := getStringParam(config, userDataParam, "")
	if userData == "" {
		return nil, fmt.Errorf("param set %d carries no spored bootstrap (%s), so its instance would run with no "+
			"in-instance TTL, idle or cost enforcement; it was not launched (#1323)", paramIndex, userDataParam)
	}

	tags, err := memberTags(state, config, paramIndex, now)
	if err != nil {
		return nil, err
	}

	input := &ec2.RunInstancesInput{
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
		InstanceType: ec2types.InstanceType(instanceType),
		UserData:     aws.String(userData),
		// spored needs the profile's credentials to read its tags and write its
		// heartbeat. The CLI ensures spored-instance-profile and seeds it as
		// iam_role (ensureDetachedInstanceProfile); the fallback is the same name.
		IamInstanceProfile: &ec2types.IamInstanceProfileSpecification{
			Name: aws.String(getStringParam(config, "iam_role", spawnaws.SporedInstanceProfileName)),
		},
		// The member's tags as a foreground launch writes them, on the instance
		// and its volumes, as a foreground launch puts them (#1330).
		TagSpecifications: []ec2types.TagSpecification{
			{ResourceType: ec2types.ResourceTypeInstance, Tags: tags},
			{ResourceType: ec2types.ResourceTypeVolume, Tags: tags},
		},
	}
	if ami := getStringParam(config, "ami", ""); ami != "" {
		input.ImageId = aws.String(ami)
	}
	if keyName := getStringParam(config, "key_name", ""); keyName != "" {
		input.KeyName = aws.String(keyName)
	}
	// The params as uploaded: a legacy member's log line names the spawn that
	// rendered its params, not this Lambda's own spawn:version.
	if err := applyMemberLaunchSettings(input, state, params, paramIndex); err != nil {
		return nil, err
	}
	if err := refuseNestedSpawnProfile(input, paramIndex); err != nil {
		return nil, err
	}
	return input, nil
}

// refuseNestedSpawnProfile refuses a member whose instance profile is a
// nested-spawn role (spawnaws.NestedSpawnRolePrefix, #1372), checked on the
// request as composed, so it holds for CLI-rendered and legacy params alike.
// Such a role may itself call RunInstances and iam:PassRole; `spawn launch
// --nested-spawn` gives it only to a single instance with a --ttl, and this
// Lambda must not hand it to whoever wrote a sweep's params. The CLI refuses
// it before upload, and SpawnSweepCrossAccountRole denies the PassRole; this
// is the check that holds against params written by anything else.
func refuseNestedSpawnProfile(input *ec2.RunInstancesInput, paramIndex int) error {
	p := input.IamInstanceProfile
	if p == nil {
		return nil
	}
	arn := aws.ToString(p.Arn)
	for _, name := range []string{aws.ToString(p.Name), arn[strings.LastIndex(arn, "/")+1:]} {
		if !spawnaws.IsNestedSpawnRole(name) {
			continue
		}
		return fmt.Errorf("param set %d names instance profile %q, a nested-spawn role (%s*): nested spawn isn't "+
			"available for detached sweeps; it was not launched (#1372). Use --nested-spawn on a single launch, "+
			"which requires --ttl", paramIndex, name, spawnaws.NestedSpawnRolePrefix)
	}
	return nil
}

// launchSettingsParam is the param key carrying a member's RunInstances-side
// settings (#1353): its volumes, network, placement, Spot, hibernation,
// reservation, IMDS and CPU options, resolved by the CLI from the member's
// config (spawnaws.LaunchSettings). It must equal detachedLaunchSettingsKey in
// cmd/launch_sweep_detached_settings.go; TestDetachedSweepSeedsEveryKeyTheLambdaReads
// holds the two together.
const launchSettingsParam = "spawn_launch_settings"

// legacySpotParam is the bare Spot flag a CLI older than #1353 uploads, and
// the only RunInstances-side setting this Lambda applied before it.
const legacySpotParam = "spot"

// legacyLogged records the sweeps already logged as launching from legacy
// params, so the line is written once per sweep in an execution environment
// rather than once per member.
var legacyLogged sync.Map

// applyMemberLaunchSettings applies member paramIndex's launch settings to
// input, composed as a foreground launch composes them (spawnaws.LaunchSettings.Apply).
//
// This Lambda used to set only a bare Spot market option here, so everything
// else a member asked RunInstances for was dropped (#1353).
//
// Params without launchSettingsParam come from a CLI older than #1353, or from
// a schedule such a CLI created, whose params are replayed as uploaded for the
// schedule's whole life. They launch as they did before: the bare Spot flag and
// nothing else. This Lambda is shared, hosted infrastructure, and refusing them
// would break every such sweep and schedule when it is redeployed.
//
// Params that do carry settings are held to them: a member whose settings this
// build cannot fully apply (a missing or other schema, a field it does not
// know) is refused rather than launched without them.
func applyMemberLaunchSettings(input *ec2.RunInstancesInput, state *SweepRecord, config map[string]interface{}, paramIndex int) error {
	raw, ok := getRawParam(config, launchSettingsParam)
	if !ok {
		logLegacySettings(state, config)
		if getBoolParam(config, legacySpotParam, false) {
			input.InstanceMarketOptions = &ec2types.InstanceMarketOptionsRequest{
				MarketType: ec2types.MarketTypeSpot,
			}
		}
		return nil
	}
	settings, err := spawnaws.DecodeLaunchSettings(raw)
	if err != nil {
		return fmt.Errorf("param set %d: %w; it was not launched", paramIndex, err)
	}
	if err := settings.Apply(input); err != nil {
		return fmt.Errorf("param set %d: %w; it was not launched", paramIndex, err)
	}
	return nil
}

// logLegacySettings logs, once per sweep, that its members launch from legacy
// params, naming the spawn that rendered them when its tags say.
func logLegacySettings(state *SweepRecord, config map[string]interface{}) {
	if _, done := legacyLogged.LoadOrStore(state.SweepID, true); done {
		return
	}
	version := "not recorded"
	if v, ok := getMapParam(config, tagsParam)["spawn:version"].(string); ok && v != "" {
		version = v
	}
	log.Printf("Sweep %s: params carry no %s (spawn CLI version: %s, older than #1353), so its members launch "+
		"as before, with only the bare spot flag: volume size, network, placement, Spot max price and "+
		"hibernation settings are not carried. Re-run the sweep, or re-create the schedule, with a current spawn "+
		"to carry them", state.SweepID, launchSettingsParam, version)
}

// sporedBudget is how long inst may run without a heartbeat.
func sporedBudget(inst ec2types.Instance) time.Duration {
	if inst.Platform == ec2types.PlatformValuesWindows {
		return sporedBudgetWindows
	}
	return sporedBudgetLinux
}

// sporedOverdue reports whether a member has been running past its spored
// budget with no spawn:last-heartbeat, measured from launchedAt (the launch
// time this Lambda recorded; EC2's LaunchTime when that is unreadable). Only a
// running member counts: a pending one has not booted, and a member spored
// stopped for idleness has already written heartbeats.
func sporedOverdue(inst ec2types.Instance, launchedAt string, now time.Time) bool {
	if inst.State == nil || inst.State.Name != ec2types.InstanceStateNameRunning {
		return false
	}
	for _, t := range inst.Tags {
		if aws.ToString(t.Key) == spawnaws.HeartbeatTag && aws.ToString(t.Value) != "" {
			return false
		}
	}
	start, err := time.Parse(time.RFC3339, launchedAt)
	if err != nil {
		if inst.LaunchTime == nil {
			return false // nothing to measure from; the next poll may know more
		}
		start = *inst.LaunchTime
	}
	return now.Sub(start) > sporedBudget(inst)
}

// terminateUnverifiedMembers terminates every described member whose spored
// never came up within its budget (#1313), the rule the CLI applies to its own
// fleets (#1305). It returns the IDs actually terminated; a member whose
// termination failed is not among them, stays active, and is tried again on the
// next poll. It only reads state, so the region goroutines may call it
// concurrently; recordSporedFailures writes the outcome.
func terminateUnverifiedMembers(ctx context.Context, api ec2InstancesAPI, state *SweepRecord, instances []ec2types.Instance, now time.Time) map[string]time.Duration {
	launchedAt := make(map[string]string, len(state.Instances))
	for _, m := range state.Instances {
		if m.InstanceID != "" {
			launchedAt[m.InstanceID] = m.LaunchedAt
		}
	}
	var ids []string
	budgets := map[string]time.Duration{}
	for _, inst := range instances {
		id := aws.ToString(inst.InstanceId)
		at, member := launchedAt[id]
		if !member || !sporedOverdue(inst, at, now) {
			continue
		}
		ids = append(ids, id)
		budgets[id] = sporedBudget(inst)
	}
	if len(ids) == 0 {
		return nil
	}
	if _, err := api.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: ids}); err != nil {
		log.Printf("ERROR: spored never came up on %v and terminating them failed: %v (retrying next poll; "+
			"the TTL reaper is the backstop)", ids, err)
		return nil
	}
	log.Printf("Terminated %v: spored never came up (no %s within its budget)", ids, spawnaws.HeartbeatTag)
	return budgets
}

// recordSporedFailures marks the members terminateUnverifiedMembers ended as
// failed sweep rows: moved from launched to failed, sweep-wide and per region,
// so completionStatus and the CLI see them as the failures they are.
func recordSporedFailures(state *SweepRecord, killed map[string]time.Duration, now time.Time) {
	if len(killed) == 0 {
		return
	}
	ts := now.Format(time.RFC3339)
	for i := range state.Instances {
		m := &state.Instances[i]
		budget, ok := killed[m.InstanceID]
		if !ok || m.State == "failed" {
			continue
		}
		m.State = "failed"
		m.TerminatedAt = ts
		m.ErrorMessage = fmt.Sprintf("spored never came up: no %s within %s of launch, so the instance had "+
			"no in-instance TTL, idle or cost enforcement; terminated (#1313)", spawnaws.HeartbeatTag, budget)
		m.StateHistory = append(m.StateHistory, StateTransition{Timestamp: ts, State: "terminated"})
		state.Failed++
		if state.Launched > 0 {
			state.Launched--
		}
		if rs := state.RegionStatus[m.Region]; rs != nil {
			rs.Failed++
			if rs.Launched > 0 {
				rs.Launched--
			}
		}
	}
}
