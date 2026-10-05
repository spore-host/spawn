package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spore-host/cohort"
	"github.com/spore-host/spawn/pkg/arrayrec"
	"github.com/spore-host/spawn/pkg/audit"
	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/launcher"
	"github.com/spore-host/spawn/pkg/mpicohort"
	"github.com/spore-host/spawn/pkg/platform"
	"github.com/spore-host/spawn/pkg/progress"
	"github.com/spore-host/spawn/pkg/userdata"
)

// generateJobArrayID creates a unique ID for a job array
// Format: {name}-{timestamp}-{random}
// Example: compute-20260113-abc123
func generateJobArrayID(name string) string {
	timestamp := time.Now().Format("20060102")
	// Generate 6-character random suffix (base36: 0-9a-z)
	random := fmt.Sprintf("%06x", time.Now().UnixNano()%0xFFFFFF)
	return fmt.Sprintf("%s-%s-%s", name, timestamp, random)
}

// formatInstanceName applies template substitution for instance names
// Supported variables: {index}, {job-array-name}
// Default template: "{job-array-name}-{index}"
func formatInstanceName(template string, jobArrayName string, index int) string {
	if template == "" {
		template = "{job-array-name}-{index}"
	}

	name := template
	name = strings.ReplaceAll(name, "{index}", fmt.Sprintf("%d", index))
	name = strings.ReplaceAll(name, "{job-array-name}", jobArrayName)

	return name
}

// memberParams carries the job-array knobs the per-member config builder needs
// that don't live on the base LaunchConfig. Threading them explicitly (rather
// than reading package globals) lets `spawn array retry` rebuild members from a
// persisted record without mutating global launch state. runLaunch populates it
// from the CLI globals; retry populates it from the arrayrec.Record.
type memberParams struct {
	name          string // job-array name
	size          int    // requested member count ({total} baked into every member)
	command       string // shared per-member command (may be "")
	instanceNames string // Name template, e.g. "worker-{index}"
	mpi           bool   // MPI cohort (per-index MPI user-data appended)
	efsID         string // when set (or mpi/fsx), storage user-data was appended to base
}

// buildJobArrayMemberConfig clones baseConfig into the per-index LaunchConfig for
// one job-array member: job-array tags, the templated Name, an index-suffixed DNS
// name, and — when MPI/storage is in play — the per-index MPI user-data appended
// to the base script. It is the single source of truth shared by the launch
// (cohort reconciler) and the `array retry` relaunch paths.
func buildJobArrayMemberConfig(baseConfig *aws.LaunchConfig, mp memberParams, jobArrayID string, index int, fsxInfo *aws.FSxInfo) (aws.LaunchConfig, error) {
	instanceConfig := *baseConfig

	instanceConfig.JobArrayID = jobArrayID
	instanceConfig.JobArrayName = mp.name
	instanceConfig.JobArraySize = mp.size
	instanceConfig.JobArrayIndex = index
	instanceConfig.JobArrayCommand = mp.command

	instanceConfig.Name = formatInstanceName(mp.instanceNames, mp.name, index)

	if baseConfig.DNSName != "" {
		instanceConfig.DNSName = fmt.Sprintf("%s-%d", baseConfig.DNSName, index)
	}

	// Append MPI user-data when enabled. Storage mounts (EFS/FSx/attached EBS)
	// are NOT re-appended here — they're already baked into baseConfig.UserData
	// before the user script (#166); re-appending would double-mount. On the
	// retry path mp.mpi is always false and mp.efsID/fsxInfo are unset (storage is
	// already in the persisted base user-data), so this block is skipped there.
	if mp.mpi || mp.efsID != "" || fsxInfo != nil {
		// decodeUserData, NOT a bare base64 decode (spawn#671). encodeUserData
		// gzips, so base64-decoding alone yields COMPRESSED bytes; appending the MPI
		// script to those and re-encoding shipped gzip(gzip(bootstrap) + mpiScript).
		// cloud-init unwraps one layer, finds binary, logs "Unhandled non-multipart
		// (text/x-not-multipart) userdata" and skips everything — so no member ever
		// installed spored, which also meant nothing on-instance enforced spawn:ttl.
		combinedUserData, err := decodeUserData(instanceConfig.UserData)
		if err != nil {
			return aws.LaunchConfig{}, fmt.Errorf("failed to decode base user-data: %w", err)
		}

		if mp.mpi {
			mpiConfig := userdata.MPIConfig{
				Region:              baseConfig.Region,
				JobArrayID:          jobArrayID,
				JobArrayIndex:       index,
				JobArraySize:        mp.size,
				MPIProcessesPerNode: mpiProcessesPerNode,
				MPICommand:          mpiCommand,
				SkipInstall:         mpiSkipInstall,
				EFAEnabled:          efaEnabled,
				// Signal the gate buildUserData declared for this script (#664).
				// Both halves are required: a declared gate nothing signals would
				// stall --command for the full timeout, and a signal with nothing
				// waiting is simply inert.
				ReadyGate: launcher.MPIReadyGate,
			}
			mpiScript, err := userdata.GenerateMPIUserData(mpiConfig)
			if err != nil {
				return aws.LaunchConfig{}, fmt.Errorf("failed to generate MPI user-data: %w", err)
			}
			combinedUserData += "\n" + mpiScript
		}

		instanceConfig.UserData = encodeUserData(combinedUserData)
	}

	return instanceConfig, nil
}

// cohortBudget maps the legacy job-array timeouts onto cohort's per-phase budget.
// Running matches the legacy flat 2-minute running-wait; every field is set
// explicitly so cohort doesn't inject its larger defaults.
func cohortBudget(spec cohortSpec) cohort.PhaseBudget {
	b := cohort.PhaseBudget{
		LaunchAcked:    30 * time.Second, // RunInstances ack
		Running:        2 * time.Minute,  // matches legacy maxWaitTime
		Enrolled:       2 * time.Second,  // nil Enroller → trivially enrolled
		CohortBarrier:  30 * time.Second, // straggler wait
		CohortAssembly: 1 * time.Second,  // plain array: nil Assembler → phase skipped
	}
	if spec.mpi {
		// MPI runs SSM-backed enrollment + assembly, which need minutes on cold
		// boot (SSM agent online + mpirun/EFA readiness), not the 1-2s defaults.
		b.Enrolled = 5 * time.Minute
		b.CohortAssembly = 5 * time.Minute
	}
	return b
}

// maxAZFallbackRungs caps the AZ-fallback chain length. Each capacity-exhausted
// round drains and relaunches the WHOLE cohort in the next AZ, so an unbounded
// chain across every AZ could churn many full launch/drain cycles; 4 covers the
// common "primary AZ out of capacity" case without unbounded churn.
const maxAZFallbackRungs = 4

// ssmPushTimeout bounds a single RunShellScript peers-file push per node.
const ssmPushTimeout = 90 * time.Second

// buildAZChain returns the primary placement rung and its AZ-fallback chain for
// an MPI cohort. The chain is a list of rungs identical except for AvailZone,
// ordered with the operator-selected AZ first (if any). cohort advances the
// shared rung across this chain as a unit on capacity exhaustion.
//
// A FIXED placement group (explicit --placement-group) is AZ-bound, so AZ
// fallback is disabled for it (single-rung). The AUTO case uses
// PlacementGroupPrefix instead of PlacementGroup, so the cohort creates a fresh
// per-AZ group as it advances — fallback stays enabled there.
func buildAZChain(ctx context.Context, awsClient *aws.Client, baseConfig *aws.LaunchConfig, capacity cohort.CapacityModel) (cohort.Rung, []cohort.Rung) {
	primary := cohort.Rung{
		InstanceType:  baseConfig.InstanceType,
		AvailZone:     baseConfig.AvailabilityZone,
		CapacityModel: capacity,
	}

	// Gated: no AZ fallback with a fixed, user-managed placement group (it's
	// bound to one AZ). Auto per-AZ PGs (PlacementGroupPrefix) still get fallback.
	if baseConfig.PlacementGroup != "" {
		return primary, []cohort.Rung{primary}
	}

	// Gated for the same reason: an explicit --subnet-id is AZ-bound, because a
	// subnet exists in exactly one availability zone. Advancing the chain rewrites
	// Placement.AvailabilityZone, and EC2 rejects the launch outright when that
	// contradicts the subnet:
	//
	//	InvalidParameterValue: Value (us-east-1a) for parameter availabilityZone
	//	is invalid. Subnet '...' is in the availability zone us-east-1b
	//
	// This was unreachable until #667, which stopped dropping --subnet-id — so
	// `--subnet-id` with `--mpi`/`--count` had never actually worked. Found by
	// running #667 on real hardware; the unit tests could see the subnet arrive on
	// the config but not that it then contradicted the AZ chain.
	//
	// AvailZone is left as the caller gave it (usually empty, so RunInstances
	// derives the zone from the subnet). An inconsistent explicit --az + subnet
	// pair is preserved rather than rewritten, so it surfaces as EC2's own clear
	// error instead of being silently moved to a zone the caller didn't pick.
	if baseConfig.SubnetID != "" {
		return primary, []cohort.Rung{primary}
	}

	zones, err := awsClient.DescribeAvailabilityZones(ctx, baseConfig.Region)
	if err != nil || len(zones) == 0 {
		// AZ discovery failed — degrade to single-rung rather than fail the launch.
		if err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  AZ fallback unavailable (%v); using a single AZ\n", err)
		}
		return primary, []cohort.Rung{primary}
	}

	// Order: operator-selected AZ first (if set and valid), then the rest.
	ordered := orderAZs(zones, baseConfig.AvailabilityZone)
	if len(ordered) > maxAZFallbackRungs {
		ordered = ordered[:maxAZFallbackRungs]
	}

	chain := make([]cohort.Rung, 0, len(ordered))
	for _, az := range ordered {
		chain = append(chain, cohort.Rung{
			InstanceType:  baseConfig.InstanceType,
			AvailZone:     az,
			CapacityModel: capacity,
		})
	}
	if len(chain) > 1 {
		fmt.Fprintf(os.Stderr, "   AZ fallback enabled across %d zones: %s\n", len(chain), strings.Join(ordered, ", "))
	}
	return chain[0], chain
}

// orderAZs returns zones with preferred first (when non-empty and present),
// preserving the sorted order of the remainder.
func orderAZs(zones []string, preferred string) []string {
	if preferred == "" {
		return zones
	}
	ordered := make([]string, 0, len(zones))
	for _, z := range zones {
		if z == preferred {
			ordered = append(ordered, preferred) // preferred first (only if present)
			break
		}
	}
	for _, z := range zones {
		if z != preferred {
			ordered = append(ordered, z)
		}
	}
	return ordered
}

// cleanupAbandonedPGs deletes the per-AZ cluster placement groups the Actuator
// created for AZs the cohort tried and abandoned during fallback. Best-effort:
// deletion failures are logged, not fatal (an empty PG is free and a reaper can
// sweep it). The PG for keepAZ (the surviving AZ, if any) is retained — it holds
// the live instances. keepAZ == "" (launch failed/drained) deletes them all.
func cleanupAbandonedPGs(ctx context.Context, awsClient *aws.Client, act *mpicohort.Actuator, keepAZ *string) {
	keep := ""
	if keepAZ != nil {
		keep = *keepAZ
	}
	var keepName string
	if keep != "" && act.PlacementGroupPrefix != "" {
		keepName = mpicohort.PlacementGroupName(act.PlacementGroupPrefix, keep)
	}
	for _, name := range act.CreatedPlacementGroups() {
		if name == keepName {
			continue
		}
		// WithRetry (#685): EC2 refuses to delete a group while any instance
		// still references it, and TerminateInstances is asynchronous — so a
		// delete issued immediately after the drain lost the race essentially
		// every time, reporting InvalidPlacementGroup.InUse on every failed MPI
		// launch and leaving the group behind. Nine were found in one region of
		// one account.
		if err := awsClient.DeletePlacementGroupWithRetry(ctx, name); err != nil {
			// Name the exact command rather than just the failure. An empty
			// placement group is free, so this is tidiness, not cost — and a
			// one-paste remedy is worth more here than a longer wait.
			fmt.Fprintf(os.Stderr, "⚠️  could not delete abandoned placement group %s: %v\n"+
				"   finish it with: aws ec2 delete-placement-group --region %s --group-name %s\n",
				name, err, act.Region, name)
		}
	}
}

// drainJobArray terminates every instance carrying the given job-array-id tag.
// Best-effort: used to compensate for cohort NOT draining on assembly failure
// (the members are all live when Assemble runs, so a failed push must not leave a
// billing cluster). Errors are logged, not fatal.
// instanceBelongsToJobArray reports whether in is a member of jobArrayID.
//
// Reads InstanceInfo.JobArrayID, NOT Tags["spawn:job-array-id"] (#683):
// listInstancesInRegion lifts every spawn:* tag it recognises into a named field
// and only writes UNrecognised keys into Tags, so that lookup is always empty and
// the drain it guarded matched nothing, ever.
//
// An empty jobArrayID matches nothing. Without that guard a drain called with a
// missing id would match every plain instance in the account and terminate it.
func instanceBelongsToJobArray(in aws.InstanceInfo, jobArrayID string) bool {
	if jobArrayID == "" {
		return false
	}
	return in.JobArrayID == jobArrayID
}

func drainJobArray(ctx context.Context, awsClient *aws.Client, region, jobArrayID string) {
	insts, err := awsClient.ListInstances(ctx, region, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  drain: list instances failed: %v — RUN `spawn instances list` "+
			"AND TERMINATE BY HAND; a failed cohort's instances are still billing\n", err)
		return
	}

	// Report what was found and done, always. This path is the only thing between
	// a failed cohort and an open-ended bill, and it used to be completely silent
	// on success — so "drained 2" and "drained nothing because the scan came back
	// empty" looked identical from the outside. A real 2-node run ended with one
	// member terminated, one left running, and no indication which had happened
	// (#683).
	var drained, failed int
	for _, in := range insts {
		// in.JobArrayID, NOT in.Tags["spawn:job-array-id"] (#683).
		//
		// listInstancesInRegion lifts the spawn:* tags it knows into named fields
		// in a switch, and only its DEFAULT branch writes into the Tags map. So
		// every recognised tag — spawn:job-array-id among them — is absent from
		// Tags, and the old filter compared "" against the cohort id for every
		// instance and skipped all of them.
		//
		// The drain therefore terminated NOTHING, ever. Confirmed against two live
		// instances: JobArrayID="repro682-..." while Tags["spawn:job-array-id"]=""
		// with 25 other tags present. The single terminate seen in a real failed
		// run came from cohort's own cancellation path, not from here, which is why
		// the culprit member was the one left running and billing.
		if !instanceBelongsToJobArray(in, jobArrayID) {
			continue
		}
		if err := awsClient.Terminate(ctx, region, in.InstanceID); err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "⚠️  drain: terminate %s failed: %v — TERMINATE IT BY HAND\n",
				in.InstanceID, err)
			continue
		}
		drained++
		fmt.Fprintf(os.Stderr, "   drained %s (%s)\n", in.InstanceID, in.State)
	}

	switch {
	case failed > 0:
		fmt.Fprintf(os.Stderr, "⚠️  drain: %d terminated, %d FAILED to terminate and are still "+
			"billing\n", drained, failed)
	case drained == 0:
		// Either nothing launched (fine) or the scan did not see what launched
		// (expensive). The caller cannot tell these apart, so say so rather than
		// implying the former.
		fmt.Fprintf(os.Stderr, "   drain: no instances matched spawn:job-array-id=%s. If the "+
			"cohort got as far as launching, verify with `spawn instances list` — a member the "+
			"scan missed keeps billing.\n", jobArrayID)
	default:
		fmt.Fprintf(os.Stderr, "   drain: terminated %d instance(s)\n", drained)
	}
}

// launchMPICohort launches an MPI cluster as an all-or-nothing cohort
// (NewMPICohort → MinViable=len): a real barrier, leak-free drain, and shared-rung
// AZ fallback. A missing rank makes the cluster useless, so it is never partial.
func launchMPICohort(ctx context.Context, awsClient *aws.Client, baseConfig *aws.LaunchConfig, plat *platform.Platform, prog *progress.Progress, fsxInfo *aws.FSxInfo, auditLog *audit.AuditLogger) error {
	return launchCohort(ctx, awsClient, baseConfig, plat, prog, fsxInfo, auditLog, cohortSpec{mpi: true})
}

// launchPlainArrayCohort launches an independent (embarrassingly-parallel) job
// array through the cohort reconciler as a partial cohort: --min-viable members
// must come up (default 1 = fully independent), but one member's terminal failure
// does not tear down the rest. Per-entity placement/AZ fallback; no assembly.
func launchPlainArrayCohort(ctx context.Context, awsClient *aws.Client, baseConfig *aws.LaunchConfig, plat *platform.Platform, prog *progress.Progress, fsxInfo *aws.FSxInfo, auditLog *audit.AuditLogger) error {
	return launchCohort(ctx, awsClient, baseConfig, plat, prog, fsxInfo, auditLog, cohortSpec{mpi: false})
}

// cohortSpec selects the cohort shape for launchCohort.
type cohortSpec struct {
	mpi bool // true → all-or-nothing MPI cohort; false → partial (independent) array
}

// launchCohort is the shared cohort launch core for both MPI and plain job
// arrays. It builds memberParams from the CLI globals, persists a local launch
// record so `spawn array retry` can relaunch failed indexes later (plain arrays
// only), and reconciles the full 0..count-1 index set.
func launchCohort(ctx context.Context, awsClient *aws.Client, baseConfig *aws.LaunchConfig, plat *platform.Platform, prog *progress.Progress, fsxInfo *aws.FSxInfo, auditLog *audit.AuditLogger, spec cohortSpec) error {
	jobArrayID := generateJobArrayID(jobArrayName)
	createdAt := time.Now()

	mp := memberParams{
		name:          jobArrayName,
		size:          count,
		command:       command,
		instanceNames: instanceNames,
		mpi:           spec.mpi,
		efsID:         efsID,
	}

	// Persist a local launch record for a plain array (MPI is all-or-nothing, so
	// retry is meaningless there). Best-effort and done before reconcile so that
	// even a fully-failed launch is retryable. The base config's UserData already
	// embeds storage mounts + the command, so a retry needs no per-member storage
	// re-append (mp.efsID/mpi are left false on the retry path). Write failure
	// only costs retry-from-this-machine, so warn and continue.
	if !spec.mpi {
		if dir, derr := arrayrec.DefaultDir(); derr == nil {
			rec := arrayrec.Record{
				ArrayID:       jobArrayID,
				Name:          jobArrayName,
				Size:          count,
				Region:        baseConfig.Region,
				Command:       command,
				InstanceNames: instanceNames,
				CreatedAt:     createdAt,
				Base:          *baseConfig,
			}
			if err := arrayrec.Save(dir, rec); err != nil {
				fmt.Fprintf(os.Stderr, "⚠️  Could not save array launch record (retry --failed won't work from this machine): %v\n", err)
			}
		}
	}

	indexes := make([]int, count)
	for i := range indexes {
		indexes[i] = i
	}
	minViableCount := minViable
	return reconcileArrayMembers(ctx, awsClient, baseConfig, plat, prog, fsxInfo, auditLog, spec, mp, jobArrayID, createdAt, indexes, minViableCount)
}

// relaunchArrayMembers is the `spawn array retry` entry point: it reconstructs
// the launch dependencies (AWS client, platform, progress, audit) that runLaunch
// normally builds, rebuilds the base LaunchConfig + memberParams from the
// persisted record, and relaunches only the given failed/missing indexes under
// the ORIGINAL jobArrayID so they regroup with the array. Storage/command are
// already baked into rec.Base.UserData, so mp carries no efs/mpi flags — the
// per-member builder appends nothing extra. minViable = len(indexes): a retry is
// "best effort per index", so one member failing again doesn't fail the batch.
func relaunchArrayMembers(ctx context.Context, rec arrayrec.Record, indexes []int) error {
	awsClient, err := aws.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("init AWS client: %w", err)
	}
	userID, err := awsClient.GetAccountID(ctx)
	if err != nil {
		userID = "unknown"
	}
	auditLog := audit.NewLogger(os.Stderr, userID, uuid.New().String())

	plat, err := platform.Detect()
	if err != nil {
		return fmt.Errorf("detect platform: %w", err)
	}
	prog := progress.NewProgress()

	base := rec.Base
	mp := memberParams{
		name:          rec.Name,
		size:          rec.Size,
		command:       rec.Command,
		instanceNames: rec.InstanceNames,
		// mpi/efsID intentionally left zero: storage + command are already in
		// base.UserData; retry is plain-array only (MPI is all-or-nothing).
	}
	// fsxInfo is nil: any FSx/EFS mount is already embedded in base.UserData, so
	// the per-member builder must not re-append it. createdAt uses the original
	// launch time so relaunched members share their siblings' lineage.
	return reconcileArrayMembers(ctx, awsClient, &base, plat, prog, nil, auditLog, cohortSpec{mpi: false}, mp, rec.ArrayID, rec.CreatedAt, indexes, len(indexes))
}

// reconcileArrayMembers launches a specific set of job-array member indexes
// through the cohort reconciler and renders the result. It is the shared core of
// both the initial launch (indexes = 0..count-1) and `array retry` (indexes =
// the failed/missing subset). Members regroup under the given jobArrayID, so a
// retry's relaunched members join the original array. minViableCount is clamped
// to [1, len(indexes)] by buildCohort.
// memberIdempotencyToken returns the RunInstances ClientToken for one cohort
// member, keyed on the PER-LAUNCH array id (#691).
//
// The token was previously left empty, so cohort fell back to
// Token(cluster, entity, generation) = Token(<job-array-name>, <name>-<index>,
// "g1"). jobArrayID — which carries the date and a random suffix — was passed as
// the CohortID but never reached the token. Two separate `spawn launch`
// invocations sharing a --job-array-name therefore sent the SAME ClientToken per
// index, and within EC2's token-retention window that is wrong in two ways:
//
//   - parameters differ (new user-data, type, AZ) → IdempotentParameterMismatch
//     and the whole cohort fails, as reported 18.5 hours after the first launch;
//   - parameters identical → EC2 returns the ORIGINAL reservation instead of
//     launching, so spawn can report success while handing back instance IDs
//     that are already terminated, or two arrays can share instances.
//
// Re-issuing a token is correct WITHIN one launch — retries and AZ-fallback
// rungs of the same reconcile must not double-launch — and wrong ACROSS
// launches. Keying on jobArrayID gives exactly that: stable within a reconcile,
// distinct between invocations.
//
// `spawn array retry` passes the ORIGINAL rec.ArrayID, so a retried member keeps
// its original token and stays idempotent against its own earlier attempt.
func memberIdempotencyToken(jobArrayID string, id cohort.EntityID) string {
	return cohort.Token(jobArrayID, string(id), "g1")
}

func reconcileArrayMembers(ctx context.Context, awsClient *aws.Client, baseConfig *aws.LaunchConfig, plat *platform.Platform, prog *progress.Progress, fsxInfo *aws.FSxInfo, auditLog *audit.AuditLogger, spec cohortSpec, mp memberParams, jobArrayID string, createdAt time.Time, indexes []int, minViableCount int) error {
	n := len(indexes)
	if n == 0 {
		return fmt.Errorf("no member indexes to launch")
	}

	kind := "job array"
	if spec.mpi {
		kind = "MPI cluster"
	}
	fmt.Fprintf(os.Stderr, "\n🚀 Launching %s via cohort: %s (%d instances)\n", kind, mp.name, n)
	fmt.Fprintf(os.Stderr, "   Job Array ID: %s\n\n", jobArrayID)

	auditLog.LogOperationWithData("launch_job_array", jobArrayID, "initiated",
		map[string]interface{}{
			"job_array_name": mp.name,
			"instance_count": n,
			"instance_type":  baseConfig.InstanceType,
			"region":         baseConfig.Region,
			"mpi":            spec.mpi,
		}, nil)

	// Build the shared placement rung + its AZ-fallback chain. When capacity is
	// exhausted in the current AZ, cohort's collective launch advances the whole
	// cohort to the next rung's AZ as a unit (preserving the placement-group /
	// one-AZ invariant). See buildAZChain.
	capacity := cohort.CapacityOnDemand
	if baseConfig.Spot {
		capacity = cohort.CapacitySpot
	}
	rung, chain := buildAZChain(ctx, awsClient, baseConfig, capacity)

	cfgs := make(map[cohort.EntityID]aws.LaunchConfig, n)
	members := make([]cohort.EntityIntent, 0, n)
	memberIDs := make([]cohort.EntityID, 0, n) // launch-order, for result re-derivation
	for _, i := range indexes {
		cfg, err := buildJobArrayMemberConfig(baseConfig, mp, jobArrayID, i, fsxInfo)
		if err != nil {
			return fmt.Errorf("build member %d config: %w", i, err)
		}
		id := cohort.EntityID(cfg.Name)
		cfgs[id] = cfg
		memberIDs = append(memberIDs, id)
		intent, err := cohort.NewEntityIntent(mp.name, id, "g1", cohort.CohortID(jobArrayID),
			cohort.RungPlacement{Rung: rung, Chain: chain},
			memberIdempotencyToken(jobArrayID, id))
		if err != nil {
			return fmt.Errorf("build member %d intent: %w", i, err)
		}
		members = append(members, intent)
	}

	// Provider seam over the real AWS client. PlacementGroupPrefix (set by
	// runLaunch for the auto-PG case) makes the Actuator create a per-AZ cluster PG
	// on demand as the cohort advances AZs.
	act := &mpicohort.Actuator{
		Client:               awsClient,
		Region:               baseConfig.Region,
		BaseConfig:           *baseConfig,
		Configs:              cfgs,
		PlacementGroupPrefix: baseConfig.PlacementGroupPrefix,
	}
	obs := &mpicohort.Observer{Client: awsClient, Region: baseConfig.Region}

	// Domain seam. MPI runs a real readiness barrier (Enroller: probe mpirun/EFA
	// per node over SSM) and the control-plane SSM Assembler (post-barrier, push
	// the peers file to every node — retiring on-instance self-discovery). A plain
	// array has neither (nil Enroller → trivially enrolled; nil Assembler → partial
	// cohort legal).
	var enr cohort.Enroller
	var asm cohort.Assembler
	if spec.mpi {
		accountBase36 := ""
		if acct, aerr := awsClient.GetAccountID(ctx); aerr == nil {
			accountBase36 = aws.AccountBase36(acct)
		}
		enr = mpicohort.Enroller{
			Client:         awsClient,
			Region:         baseConfig.Region,
			EFAEnabled:     baseConfig.EFAEnabled,
			SkipMPIInstall: mpiSkipInstall,
			Timeout:        ssmPushTimeout,
		}
		asm = mpicohort.NewSSMAssembler(awsClient, baseConfig.Region, accountBase36,
			cohortBudget(spec).CohortAssembly, ssmPushTimeout)
	}
	r := cohort.NewReconciler(act, obs, mpicohort.Classifier{}, enr, asm, nil)

	// Clean up abandoned per-AZ placement groups the Actuator created while
	// advancing the AZ-fallback chain. Only the surviving AZ (keepAZ) holds
	// instances; every other created PG is empty and should be removed. keepAZ
	// stays "" on failure (cohort drained everything), so all get deleted.
	var keepAZ string
	defer cleanupAbandonedPGs(ctx, awsClient, act, &keepAZ)

	c, err := buildCohort(spec, jobArrayID, members, minViableCount)
	if err != nil {
		return fmt.Errorf("build cohort: %w", err)
	}

	prog.Start(fmt.Sprintf("Reconciling %d-instance cohort", n))
	outcome, err := r.Reconcile(ctx, c)
	if err != nil {
		prog.Error("Reconciling cohort", err)
		return fmt.Errorf("cohort reconcile: %w", err)
	}

	if !outcome.Ready {
		// cohort drains survivors on the launch/barrier failure paths — but NOT on
		// assembly failure (the members are all live when Assemble runs). If any
		// member reached the assembly phase, the caller must drain, or the whole
		// cluster is left running and billing. Terminate by job-array-id tag.
		// Drain UNCONDITIONALLY on cohort failure (spawn#671).
		//
		// This used to require ReachedPhase == PhaseCohortAssembly, i.e. only an
		// assembly failure drained. But Phase is ordered and an instance exists and
		// bills from PhaseLaunchAcked onward, so a cohort that dies at `running` or
		// `enrolled` had launched instances and left every one of them running. The
		// reporter's two c8g.48xlarge ($7.66/hr each) went terminal at phase=enrolled
		// and sat up until terminated by hand — which is also why the placement group
		// could not be deleted ("InvalidPlacementGroup.InUse").
		//
		// A phase predicate cannot be made safe here anyway: PhaseLaunchAcked is iota
		// 0, so a member that never launched is indistinguishable from one that did by
		// ReachedPhase alone. drainJobArray filters by the spawn:job-array-id tag, so
		// with nothing launched it is a no-op costing one DescribeInstances — against a
		// missed drain costing $15/hr. Always draining is the only defensible default.
		//
		// cleanupAbandonedPGs is deferred, so it runs after this. Termination is
		// asynchronous, so a PG delete may still race a shutting-down instance; that
		// stays best-effort, since an empty placement group is free.
		fmt.Fprintf(os.Stderr, "⚠️  Cohort failed; draining any launched instances...\n")
		drainJobArray(ctx, awsClient, baseConfig.Region, jobArrayID)

		successCount, failureCount := 0, 0
		var details []string
		for _, id := range memberIDs {
			rec := outcome.Records[id]
			if rec.Succeeded() {
				successCount++
			} else {
				failureCount++
				details = append(details, fmt.Sprintf("%s: %s", id, rec.Summary()))
			}
		}
		prog.Error(fmt.Sprintf("Reconciling %d-instance cohort", n),
			fmt.Errorf("%d/%d members failed", failureCount, n))
		auditLog.LogOperationWithData("launch_job_array", jobArrayID, "failed",
			map[string]interface{}{
				"success_count": successCount,
				"failure_count": failureCount,
			}, fmt.Errorf("%d/%d members failed", failureCount, n))
		return fmt.Errorf("job array launch failed (%d/%d members):\n  %s",
			failureCount, n, strings.Join(details, "\n  "))
	}
	prog.Complete(fmt.Sprintf("Reconciling %d-instance cohort", n))

	auditLog.LogOperationWithData("launch_job_array", jobArrayID, "success",
		map[string]interface{}{"instance_count": n}, nil)

	// The Outcome carries no instance IDs/IPs (cohort Records are state-only), so
	// re-derive the launched set from EC2 by Name and fetch public IPs — the same
	// surface the legacy path rendered.
	prog.Start("Getting public IPs")
	insts, err := awsClient.ListInstances(ctx, baseConfig.Region, "")
	if err != nil {
		prog.Error("Getting public IPs", err)
		return fmt.Errorf("list instances after reconcile: %w", err)
	}
	byName := make(map[string]aws.InstanceInfo, len(insts))
	for _, in := range insts {
		byName[in.Name] = in
	}
	launchedInstances := make([]*aws.LaunchResult, 0, n)
	for _, id := range memberIDs { // launch order
		in, ok := byName[string(id)]
		if !ok {
			continue
		}
		keepAZ = in.AvailabilityZone // all members share one AZ (collective PG invariant)
		publicIP := in.PublicIP
		if publicIP == "" {
			if ip, ipErr := awsClient.GetInstancePublicIP(ctx, baseConfig.Region, in.InstanceID); ipErr == nil {
				publicIP = ip
			}
		}
		launchedInstances = append(launchedInstances, &aws.LaunchResult{
			InstanceID:       in.InstanceID,
			Name:             in.Name,
			Region:           baseConfig.Region,
			PublicIP:         publicIP,
			PrivateIP:        in.PrivateIP,
			AvailabilityZone: in.AvailabilityZone,
			State:            "running",
			LaunchTime:       in.LaunchTime,
		})
	}
	prog.Complete("Getting public IPs")

	return renderJobArrayResult(launchedInstances, baseConfig, mp, jobArrayID, createdAt, plat)
}

// buildCohort constructs the right cohort for the spec: an all-or-nothing MPI
// cohort, or a partial (independent) array whose minViable members must come up.
// minViable is clamped to [1, len(members)].
func buildCohort(spec cohortSpec, jobArrayID string, members []cohort.EntityIntent, minViable int) (cohort.Cohort, error) {
	if spec.mpi {
		return cohort.NewMPICohort(cohort.CohortID(jobArrayID), members, cohortBudget(spec))
	}
	mv := minViable
	if mv < 1 {
		mv = 1
	}
	if mv > len(members) {
		mv = len(members)
	}
	// nil Assembler: a plain array has no collective assembly phase, so a partial
	// cohort is legal (NewPartialCohort rejects a non-nil Assembler).
	return cohort.NewPartialCohort(cohort.CohortID(jobArrayID), members, cohortBudget(spec), mv, nil)
}

// renderJobArrayResult writes the job-array ID to the output-id file and emits
// the success output (JSON array or the human table + management/connect hints).
// Shared by the legacy and cohort launch paths so both render identically.
func renderJobArrayResult(launchedInstances []*aws.LaunchResult, baseConfig *aws.LaunchConfig, mp memberParams, jobArrayID string, createdAt time.Time, plat *platform.Platform) error {
	// Write job array ID to file for workflow integration
	if err := writeOutputID(jobArrayID, outputIDFile); err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  Failed to write job array ID to file: %v\n", err)
	}

	// JSON output mode — always an array, consistent with single-instance path
	if getOutputFormat() == "json" {
		out := make([]map[string]interface{}, len(launchedInstances))
		for i, inst := range launchedInstances {
			out[i] = map[string]interface{}{
				"instance_id":     inst.InstanceID,
				"name":            inst.Name,
				"instance_type":   baseConfig.InstanceType,
				"region":          baseConfig.Region,
				"public_ip":       inst.PublicIP,
				"state":           "running",
				"job_array_name":  mp.name,
				"job_array_id":    jobArrayID,
				"job_array_index": i,
				"job_array_size":  mp.size,
			}
		}
		jsonBytes, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal JSON: %w", err)
		}
		fmt.Println(string(jsonBytes))
		return nil
	}

	// Display success for job array
	fmt.Fprintf(os.Stderr, "\n✅ Job array launched successfully!\n\n")
	fmt.Fprintf(os.Stderr, "Job Array: %s\n", mp.name)
	fmt.Fprintf(os.Stderr, "Array ID:  %s\n", jobArrayID)
	fmt.Fprintf(os.Stderr, "Created:   %s\n", createdAt.Format(time.RFC3339))
	fmt.Fprintf(os.Stderr, "Count:     %d instances\n", len(launchedInstances))
	fmt.Fprintf(os.Stderr, "Region:    %s\n\n", baseConfig.Region)

	// Display table of instances
	fmt.Fprintf(os.Stderr, "Instances:\n")
	fmt.Fprintf(os.Stderr, "%-5s %-20s %-19s %-15s\n", "Index", "Instance ID", "Name", "Public IP")
	fmt.Fprintf(os.Stderr, "%-5s %-20s %-19s %-15s\n", "-----", "--------------------", "-------------------", "---------------")

	for i, inst := range launchedInstances {
		ipDisplay := inst.PublicIP
		if ipDisplay == "" {
			ipDisplay = "(pending)"
		}
		fmt.Fprintf(os.Stderr, "%-5d %-20s %-19s %-15s\n", i, inst.InstanceID, inst.Name, ipDisplay)
	}

	fmt.Fprintf(os.Stderr, "\nManagement:\n")
	fmt.Fprintf(os.Stderr, "  • List:      spawn list --job-array-name %s\n", mp.name)
	fmt.Fprintf(os.Stderr, "  • Terminate: spawn terminate --job-array-name %s\n", mp.name)
	fmt.Fprintf(os.Stderr, "  • Extend:    spawn extend --job-array-name %s --ttl 4h\n", mp.name)

	if len(launchedInstances) > 0 && launchedInstances[0].PublicIP != "" {
		fmt.Fprintf(os.Stderr, "\nConnect to instances:\n")
		for i, inst := range launchedInstances {
			if inst.PublicIP != "" {
				sshCmd := plat.GetSSHCommand("ec2-user", inst.PublicIP)
				fmt.Fprintf(os.Stderr, "  [%d] %s\n", i, sshCmd)
			}
		}
	}

	fmt.Fprintf(os.Stderr, "\n")

	return nil
}
