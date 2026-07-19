package cmd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spore-host/cohort"
	"github.com/spore-host/spawn/pkg/audit"
	"github.com/spore-host/spawn/pkg/aws"
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

// buildJobArrayMemberConfig clones baseConfig into the per-index LaunchConfig for
// one job-array member: job-array tags, the templated Name, an index-suffixed DNS
// name, and — when MPI/storage is in play — the per-index MPI user-data appended
// to the base script. It is the single source of truth shared by both the legacy
// goroutine loop and the cohort reconciler path.
func buildJobArrayMemberConfig(baseConfig *aws.LaunchConfig, jobArrayID string, index int, fsxInfo *aws.FSxInfo) (aws.LaunchConfig, error) {
	instanceConfig := *baseConfig

	instanceConfig.JobArrayID = jobArrayID
	instanceConfig.JobArrayName = jobArrayName
	instanceConfig.JobArraySize = count
	instanceConfig.JobArrayIndex = index
	instanceConfig.JobArrayCommand = command

	instanceConfig.Name = formatInstanceName(instanceNames, jobArrayName, index)

	if baseConfig.DNSName != "" {
		instanceConfig.DNSName = fmt.Sprintf("%s-%d", baseConfig.DNSName, index)
	}

	// Append MPI user-data when enabled. Storage mounts (EFS/FSx/attached EBS)
	// are NOT re-appended here — they're already baked into baseConfig.UserData
	// before the user script (#166); re-appending would double-mount.
	if mpiEnabled || efsID != "" || fsxInfo != nil {
		baseUserDataBytes, err := base64.StdEncoding.DecodeString(instanceConfig.UserData)
		if err != nil {
			return aws.LaunchConfig{}, fmt.Errorf("failed to decode base user-data: %w", err)
		}
		combinedUserData := string(baseUserDataBytes)

		if mpiEnabled {
			mpiConfig := userdata.MPIConfig{
				Region:              baseConfig.Region,
				JobArrayID:          jobArrayID,
				JobArrayIndex:       index,
				JobArraySize:        count,
				MPIProcessesPerNode: mpiProcessesPerNode,
				MPICommand:          mpiCommand,
				SkipInstall:         mpiSkipInstall,
				EFAEnabled:          efaEnabled,
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
		// MPI runs the SSM control-plane Assembler: wait for SSM online + push the
		// peers file to every node. That needs minutes on cold boot, not 1s.
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
		if err := awsClient.DeletePlacementGroup(ctx, name); err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  could not delete abandoned placement group %s: %v\n", name, err)
		}
	}
}

// drainJobArray terminates every instance carrying the given job-array-id tag.
// Best-effort: used to compensate for cohort NOT draining on assembly failure
// (the members are all live when Assemble runs, so a failed push must not leave a
// billing cluster). Errors are logged, not fatal.
func drainJobArray(ctx context.Context, awsClient *aws.Client, region, jobArrayID string) {
	insts, err := awsClient.ListInstances(ctx, region, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  drain: list instances failed: %v\n", err)
		return
	}
	for _, in := range insts {
		if in.Tags["spawn:job-array-id"] != jobArrayID {
			continue
		}
		if err := awsClient.Terminate(ctx, region, in.InstanceID); err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  drain: terminate %s failed: %v\n", in.InstanceID, err)
		}
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
// arrays. It builds per-index configs + intents (with the AZ-fallback chain),
// constructs the appropriate cohort (all-or-nothing for MPI, partial for a plain
// array), reconciles, and renders the same output contract as before.
func launchCohort(ctx context.Context, awsClient *aws.Client, baseConfig *aws.LaunchConfig, plat *platform.Platform, prog *progress.Progress, fsxInfo *aws.FSxInfo, auditLog *audit.AuditLogger, spec cohortSpec) error {
	jobArrayID := generateJobArrayID(jobArrayName)
	createdAt := time.Now()

	kind := "job array"
	if spec.mpi {
		kind = "MPI cluster"
	}
	fmt.Fprintf(os.Stderr, "\n🚀 Launching %s via cohort: %s (%d instances)\n", kind, jobArrayName, count)
	fmt.Fprintf(os.Stderr, "   Job Array ID: %s\n\n", jobArrayID)

	auditLog.LogOperationWithData("launch_job_array", jobArrayID, "initiated",
		map[string]interface{}{
			"job_array_name": jobArrayName,
			"instance_count": count,
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

	cfgs := make(map[cohort.EntityID]aws.LaunchConfig, count)
	members := make([]cohort.EntityIntent, 0, count)
	memberIDs := make([]cohort.EntityID, count) // index-ordered, for result re-derivation
	for i := 0; i < count; i++ {
		cfg, err := buildJobArrayMemberConfig(baseConfig, jobArrayID, i, fsxInfo)
		if err != nil {
			return fmt.Errorf("build member %d config: %w", i, err)
		}
		id := cohort.EntityID(cfg.Name)
		cfgs[id] = cfg
		memberIDs[i] = id
		intent, err := cohort.NewEntityIntent(jobArrayName, id, "g1", cohort.CohortID(jobArrayID),
			cohort.RungPlacement{Rung: rung, Chain: chain}, "")
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

	// Domain seam. MPI runs the control-plane SSM Assembler: post-barrier it
	// pushes the peers file to every node (retiring on-instance self-discovery).
	// A plain array has no assembly phase (nil Assembler → partial cohort legal).
	// Enroller stays nil here; the SSM readiness probe lands in Stage 5.
	var asm cohort.Assembler
	if spec.mpi {
		accountBase36 := ""
		if acct, aerr := awsClient.GetAccountID(ctx); aerr == nil {
			accountBase36 = aws.AccountBase36(acct)
		}
		asm = mpicohort.NewSSMAssembler(awsClient, baseConfig.Region, accountBase36,
			cohortBudget(spec).CohortAssembly, ssmPushTimeout)
	}
	r := cohort.NewReconciler(act, obs, mpicohort.Classifier{}, nil, asm, nil)

	// Clean up abandoned per-AZ placement groups the Actuator created while
	// advancing the AZ-fallback chain. Only the surviving AZ (keepAZ) holds
	// instances; every other created PG is empty and should be removed. keepAZ
	// stays "" on failure (cohort drained everything), so all get deleted.
	var keepAZ string
	defer cleanupAbandonedPGs(ctx, awsClient, act, &keepAZ)

	c, err := buildCohort(spec, jobArrayID, members)
	if err != nil {
		return fmt.Errorf("build cohort: %w", err)
	}

	prog.Start(fmt.Sprintf("Reconciling %d-instance cohort", count))
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
		assemblyReached := false
		for _, id := range memberIDs {
			if outcome.Records[id].ReachedPhase == cohort.PhaseCohortAssembly {
				assemblyReached = true
				break
			}
		}
		if assemblyReached {
			fmt.Fprintf(os.Stderr, "⚠️  Assembly failed; draining %d launched instances...\n", count)
			drainJobArray(ctx, awsClient, baseConfig.Region, jobArrayID)
		}

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
		prog.Error(fmt.Sprintf("Reconciling %d-instance cohort", count),
			fmt.Errorf("%d/%d members failed", failureCount, count))
		auditLog.LogOperationWithData("launch_job_array", jobArrayID, "failed",
			map[string]interface{}{
				"success_count": successCount,
				"failure_count": failureCount,
			}, fmt.Errorf("%d/%d members failed", failureCount, count))
		return fmt.Errorf("job array launch failed (%d/%d members):\n  %s",
			failureCount, count, strings.Join(details, "\n  "))
	}
	prog.Complete(fmt.Sprintf("Reconciling %d-instance cohort", count))

	auditLog.LogOperationWithData("launch_job_array", jobArrayID, "success",
		map[string]interface{}{"instance_count": count}, nil)

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
	launchedInstances := make([]*aws.LaunchResult, 0, count)
	for _, id := range memberIDs { // index order
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

	return renderJobArrayResult(launchedInstances, baseConfig, jobArrayID, createdAt, plat)
}

// buildCohort constructs the right cohort for the spec: an all-or-nothing MPI
// cohort, or a partial (independent) array whose --min-viable members must come
// up. minViable is clamped to [1, count].
func buildCohort(spec cohortSpec, jobArrayID string, members []cohort.EntityIntent) (cohort.Cohort, error) {
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
func renderJobArrayResult(launchedInstances []*aws.LaunchResult, baseConfig *aws.LaunchConfig, jobArrayID string, createdAt time.Time, plat *platform.Platform) error {
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
				"job_array_name":  jobArrayName,
				"job_array_id":    jobArrayID,
				"job_array_index": i,
				"job_array_size":  count,
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
	fmt.Fprintf(os.Stderr, "Job Array: %s\n", jobArrayName)
	fmt.Fprintf(os.Stderr, "Array ID:  %s\n", jobArrayID)
	fmt.Fprintf(os.Stderr, "Created:   %s\n", createdAt.Format(time.RFC3339))
	fmt.Fprintf(os.Stderr, "Count:     %d instances\n", count)
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
	fmt.Fprintf(os.Stderr, "  • List:      spawn list --job-array-name %s\n", jobArrayName)
	fmt.Fprintf(os.Stderr, "  • Terminate: spawn terminate --job-array-name %s\n", jobArrayName)
	fmt.Fprintf(os.Stderr, "  • Extend:    spawn extend --job-array-name %s --ttl 4h\n", jobArrayName)

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
