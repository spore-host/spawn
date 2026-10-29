// The tags written on an instance after RunInstances, and the slots a launch
// keeps free for them (#1355).

package aws

import (
	"slices"
	"sort"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/spore-host/spawn/pkg/costmodel"
	"github.com/spore-host/spawn/pkg/tagprefix"
)

// postLaunchWriter is who writes a post-launch tag.
type postLaunchWriter string

const (
	// writtenBySpored: the agent on the instance (pkg/agent, pkg/provider,
	// cmd/spored).
	writtenBySpored postLaunchWriter = "spored"
	// writtenBySpawn: spawn's own launch code, right after RunInstances.
	writtenBySpawn postLaunchWriter = "spawn"
)

// postLaunchTag is one tag key something writes on an instance after it is
// launched.
type postLaunchTag struct {
	key string
	by  postLaunchWriter
	// writer names the code that writes it.
	writer string
	// onCommand marks a tag written only when someone runs a command (spawn
	// extend, spawn stop/start, spored config set, ...). It is not reserved: the
	// command reports a failed write to whoever ran it, and nothing that limits
	// cost or lifetime is lost without a person seeing it. Every other entry is
	// written by itself, by spored or the launch, with nobody watching.
	onCommand bool
	// applies reports whether the tag can be written on an instance launched
	// with c. nil means on every instance.
	applies func(c LaunchConfig) bool
}

func isSpot(c LaunchConfig) bool { return c.Spot }

// createsEphemeralFSx: an FSx created after the launch (#213), whose mount
// tags spawn writes once the filesystem exists.
func createsEphemeralFSx(c LaunchConfig) bool {
	return c.FSxLustreCreate && c.FSxLustreID == "" && c.FSxPending == ""
}

// mountsPendingFSx: spored waits for a filesystem still being created and
// mounts it (pkg/agent/fsx_mount.go), whether the launch tagged it pending or
// spawn tags it after the launch.
func mountsPendingFSx(c LaunchConfig) bool { return createsEphemeralFSx(c) || c.FSxPending != "" }

func registersDNS(c LaunchConfig) bool { return c.DNSName != "" }

// reportsReadiness: a web app (#590) or a DCV session (#282), whose readiness
// spored records.
func reportsReadiness(c LaunchConfig) bool { return c.AppMode == "web" || c.DCVSessionID != "" }

// isTask: a task launch (pkg/tasklaunch tags spawn:task-id), whose results
// flush spored reports (#1186).
func isTask(c LaunchConfig) bool { return c.Tags["spawn:task-id"] != "" }

// footprintTags are spored's cost footprint tags (pkg/provider
// MeasureCostFootprint): the EBS and public IPv4 cost counted against the cost
// limit, written on every instance.
func footprintTags() []postLaunchTag {
	out := make([]postLaunchTag, 0, len(costmodel.FootprintTags))
	for _, suffix := range costmodel.FootprintTags {
		out = append(out, postLaunchTag{key: tagprefix.Tag(suffix), by: writtenBySpored,
			writer: "pkg/provider MeasureCostFootprint; counted against the cost limit"})
	}
	return out
}

// postLaunchInstanceTags is every tag key written on an instance after
// RunInstances: by spored, by spawn's launch code, and by the commands that
// edit a running instance. It is the single list the launch's tag reserve is
// computed from, and TestEveryPostLaunchTagIsListed (tag_reserve_test.go at
// the repo root) fails when a writer emits a key that is not on it.
//
// The ttl-reaper writes no instance tag: its spawn:reaper-first-seen stamp is
// on security groups, placement groups and Elastic IPs.
var postLaunchInstanceTags = slices.Concat([]postLaunchTag{
	// spored, on every instance.
	{key: "spawn:compute-seconds", by: writtenBySpored, writer: "pkg/agent putComputeSecondsTag; the cost limit's clock across stop/start"},
	{key: "spawn:last-heartbeat", by: writtenBySpored, writer: "pkg/agent writeHeartbeatTag; the reaper's and sweep orchestrator's liveness check"},
	{key: "spawn:spored-version", by: writtenBySpored, writer: "pkg/agent WriteVersionTag (and spawn upgrade-spored)"},
	{key: "spawn:logged-in-count", by: writtenBySpored, writer: "pkg/agent writeSessionCountTag"},
}, footprintTags(), []postLaunchTag{
	// spored, when the launch asked for what the tag reports on.
	{key: "spawn:flush-status", by: writtenBySpored, writer: "pkg/agent reportFlushOutcome", applies: isTask},
	{key: "spawn:dns-status", by: writtenBySpored, writer: "pkg/agent recordDNSStatus", applies: registersDNS},
	{key: "spawn:dns-error", by: writtenBySpored, writer: "pkg/agent recordDNSStatus", applies: registersDNS},
	{key: "spawn:ready-status", by: writtenBySpored, writer: "pkg/agent writeReadyTags, maybeSetupWebReady, maybeSetupDCVAuth", applies: reportsReadiness},
	{key: "spawn:ready-url", by: writtenBySpored, writer: "pkg/agent maybeSetupWebReady, maybeSetupDCVAuth, refreshReadyURL", applies: reportsReadiness},
	{key: "spawn:ready-token", by: writtenBySpored, writer: "pkg/agent maybeSetupWebReady, maybeSetupDCVAuth", applies: reportsReadiness},
	{key: "spawn:fsx-id", by: writtenBySpored, writer: "pkg/agent tagFSxMounted, before it deletes spawn:fsx-pending", applies: mountsPendingFSx},
	{key: "spawn:fsx-mount-point", by: writtenBySpored, writer: "pkg/agent tagFSxMounted", applies: mountsPendingFSx},
	{key: "spawn:fsx-dra-status", by: writtenBySpored, writer: "pkg/agent tagFSxDRAStatus", applies: mountsPendingFSx},
	{key: "spawn:fsx-dra-error", by: writtenBySpored, writer: "pkg/agent tagFSxDRAStatus", applies: mountsPendingFSx},

	// spawn's launch code, right after RunInstances. A persistent request's
	// spawn:original-instance-id (tagOriginalInstance) is on the Spot request,
	// not the instance; the request carries the instance's launch tag set, so
	// the instance's reserve, never less than one slot, covers it.
	{key: SpotPriceTag, by: writtenBySpawn, writer: "pkg/aws recordLaunchSpotPrice", applies: isSpot},
	{key: "spawn:fsx-pending", by: writtenBySpawn, writer: "cmd launchWithProgress, pkg/launcher createAndAttachEphemeralFSx", applies: createsEphemeralFSx},
	{key: "spawn:fsx-s3-import-path", by: writtenBySpawn, writer: "cmd launchWithProgress, pkg/launcher createAndAttachEphemeralFSx", applies: createsEphemeralFSx},
	{key: "spawn:fsx-s3-export-path", by: writtenBySpawn, writer: "cmd launchWithProgress, pkg/launcher createAndAttachEphemeralFSx", applies: createsEphemeralFSx},

	// Commands that edit a running instance (onCommand: not reserved).
	{key: "spawn:ttl", by: writtenBySpawn, onCommand: true, writer: "spawn extend; spored config set ttl"},
	{key: "spawn:ttl-deadline", by: writtenBySpawn, onCommand: true, writer: "spawn extend; spored config set ttl"},
	{key: "spawn:cost-limit", by: writtenBySpawn, onCommand: true, writer: "spawn extend --cost-limit"},
	{key: "spawn:idle-timeout", by: writtenBySpored, onCommand: true, writer: "spored config set idle-timeout"},
	{key: "spawn:on-complete", by: writtenBySpored, onCommand: true, writer: "spored config set on-complete"},
	{key: "spawn:hibernate-on-idle", by: writtenBySpored, onCommand: true, writer: "spored config set hibernate"},
	{key: "spawn:completion-file", by: writtenBySpored, onCommand: true, writer: "spored config set completion-file"},
	{key: "spawn:completion-delay", by: writtenBySpored, onCommand: true, writer: "spored config set completion-delay"},
	{key: "spawn:last-stop-reason", by: writtenBySpawn, onCommand: true, writer: "spawn stop/hibernate (stopTags)"},
	{key: "spawn:last-stop-time", by: writtenBySpawn, onCommand: true, writer: "spawn stop/hibernate (stopTags)"},
	{key: "spawn:last-start-time", by: writtenBySpawn, onCommand: true, writer: "spawn start (startTags)"},
	{key: "spawn:dns-name", by: writtenBySpawn, onCommand: true, writer: "spawn dns register"},
	{key: "spawn:dns-domain", by: writtenBySpawn, onCommand: true, writer: "spawn dns register"},
	{key: "spawn:dns-endpoint", by: writtenBySpawn, onCommand: true, writer: "spawn dns register"},
	{key: "spore:plugin:", by: writtenBySpawn, onCommand: true, writer: "spawn plugin install, one spore:plugin:<name> per plugin (recordPluginProvenanceTag)"},
})

// PostLaunchTagKeys returns every tag key written on an instance after
// RunInstances, by spored, by spawn's launch code or by a command that edits
// a running instance, sorted (#1355). A key ending in ":" is a family, one tag
// per member. A launch keeps slots free for the ones written without anyone
// running a command; see [TagLimitError].
func PostLaunchTagKeys() []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range postLaunchInstanceTags {
		if !seen[t.key] {
			seen[t.key] = true
			out = append(out, t.key)
		}
	}
	sort.Strings(out)
	return out
}

// tagReserve is the slots a launch with c, whose RunInstances tag set is
// tags, keeps free for tags written after it: every key on
// postLaunchInstanceTags that is written without a command, that can be
// written on this instance, and that the launch does not already write (an
// update to an existing key takes no slot). spored and spawn are returned
// separately, each in list order.
func tagReserve(c LaunchConfig, tags []types.Tag) (spored, spawn []string) {
	have := make(map[string]bool, len(tags))
	for _, t := range tags {
		have[aws.ToString(t.Key)] = true
	}
	for _, t := range postLaunchInstanceTags {
		if t.onCommand || have[t.key] || (t.applies != nil && !t.applies(c)) {
			continue
		}
		have[t.key] = true
		if t.by == writtenBySpored {
			spored = append(spored, t.key)
		} else {
			spawn = append(spawn, t.key)
		}
	}
	return spored, spawn
}

// tagReserveSize is len of both of tagReserve's results.
func tagReserveSize(c LaunchConfig, tags []types.Tag) int {
	spored, spawn := tagReserve(c, tags)
	return len(spored) + len(spawn)
}
