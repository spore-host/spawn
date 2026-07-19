package cmd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
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
func cohortBudget() cohort.PhaseBudget {
	return cohort.PhaseBudget{
		LaunchAcked:    30 * time.Second, // RunInstances ack
		Running:        2 * time.Minute,  // matches legacy maxWaitTime
		Enrolled:       2 * time.Second,  // nil Enroller → trivially enrolled
		CohortBarrier:  30 * time.Second, // straggler wait
		CohortAssembly: 1 * time.Second,  // nil Assembler → phase skipped
	}
}

// maxAZFallbackRungs caps the AZ-fallback chain length. Each capacity-exhausted
// round drains and relaunches the WHOLE cohort in the next AZ, so an unbounded
// chain across every AZ could churn many full launch/drain cycles; 4 covers the
// common "primary AZ out of capacity" case without unbounded churn.
const maxAZFallbackRungs = 4

// buildAZChain returns the primary placement rung and its AZ-fallback chain for
// an MPI cohort. The chain is a list of rungs identical except for AvailZone,
// ordered with the operator-selected AZ first (if any). cohort advances the
// shared rung across this chain as a unit on capacity exhaustion.
//
// Stage-1 scope: the chain is only built when NO cluster placement group is set.
// A pre-created PG binds to one AZ, so moving AZ mid-fallback would break it;
// that's resolved in a later stage (lazy per-AZ PG). With a PG set, we return a
// single-rung chain (no fallback), matching prior behavior.
func buildAZChain(ctx context.Context, awsClient *aws.Client, baseConfig *aws.LaunchConfig, capacity cohort.CapacityModel) (cohort.Rung, []cohort.Rung) {
	primary := cohort.Rung{
		InstanceType:  baseConfig.InstanceType,
		AvailZone:     baseConfig.AvailabilityZone,
		CapacityModel: capacity,
	}

	// Gated: no AZ fallback while a placement group is in play (Stage 1).
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

// launchJobArrayCohort launches an MPI/job-array as an all-or-nothing cohort via
// the cohort reconciler (--reconciler=cohort). It gains a real barrier and a
// leak-free drain over the hand-rolled launchJobArray; peer discovery stays
// self-organizing on-instance (nil Assembler) and there is no capacity fallback
// yet (single-rung placement). Same signature/output contract as launchJobArray.
func launchJobArrayCohort(ctx context.Context, awsClient *aws.Client, baseConfig *aws.LaunchConfig, plat *platform.Platform, prog *progress.Progress, fsxInfo *aws.FSxInfo, auditLog *audit.AuditLogger) error {
	jobArrayID := generateJobArrayID(jobArrayName)
	createdAt := time.Now()

	fmt.Fprintf(os.Stderr, "\n🚀 Launching job array via cohort: %s (%d instances)\n", jobArrayName, count)
	fmt.Fprintf(os.Stderr, "   Job Array ID: %s\n\n", jobArrayID)

	auditLog.LogOperationWithData("launch_job_array", jobArrayID, "initiated",
		map[string]interface{}{
			"job_array_name": jobArrayName,
			"instance_count": count,
			"instance_type":  baseConfig.InstanceType,
			"region":         baseConfig.Region,
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

	// Provider seam over the real AWS client. nil Enroller (trivially enrolled)
	// and nil Assembler (peer discovery stays on-instance) per the v1 design.
	act := &mpicohort.Actuator{Client: awsClient, Region: baseConfig.Region, BaseConfig: *baseConfig, Configs: cfgs}
	obs := &mpicohort.Observer{Client: awsClient, Region: baseConfig.Region}
	r := cohort.NewReconciler(act, obs, mpicohort.Classifier{}, nil, nil, nil)

	c, err := cohort.NewMPICohort(cohort.CohortID(jobArrayID), members, cohortBudget())
	if err != nil {
		return fmt.Errorf("build MPI cohort: %w", err)
	}

	prog.Start(fmt.Sprintf("Reconciling %d-instance cohort", count))
	outcome, err := r.Reconcile(ctx, c)
	if err != nil {
		prog.Error("Reconciling cohort", err)
		return fmt.Errorf("cohort reconcile: %w", err)
	}

	if !outcome.Ready {
		// cohort already drained survivors — do NOT terminate here.
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
	// surface the legacy path renders.
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

// launchJobArray launches N instances in parallel as a job array
func launchJobArray(ctx context.Context, awsClient *aws.Client, baseConfig *aws.LaunchConfig, plat *platform.Platform, prog *progress.Progress, fsxInfo *aws.FSxInfo, auditLog *audit.AuditLogger) error {
	// Generate unique job array ID
	jobArrayID := generateJobArrayID(jobArrayName)
	createdAt := time.Now()

	fmt.Fprintf(os.Stderr, "\n🚀 Launching job array: %s (%d instances)\n", jobArrayName, count)
	fmt.Fprintf(os.Stderr, "   Job Array ID: %s\n\n", jobArrayID)

	// Log job array launch initiation
	auditLog.LogOperationWithData("launch_job_array", jobArrayID, "initiated",
		map[string]interface{}{
			"job_array_name": jobArrayName,
			"instance_count": count,
			"instance_type":  baseConfig.InstanceType,
			"region":         baseConfig.Region,
		}, nil)

	// Phase 1: Launch all instances in parallel
	prog.Start(fmt.Sprintf("Launching %d instances in parallel", count))

	results := runLaunchBatch(count, func(index int) (*aws.LaunchResult, error) {
		instanceConfig, err := buildJobArrayMemberConfig(baseConfig, jobArrayID, index, fsxInfo)
		if err != nil {
			return nil, err
		}
		return awsClient.Launch(ctx, instanceConfig)
	})

	// Collect results
	launchedInstances := make([]*aws.LaunchResult, 0, count)
	var launchErrors []string
	successCount := 0
	failureCount := 0

	for _, result := range results {
		if result.err != nil {
			launchErrors = append(launchErrors, fmt.Sprintf("Instance %d: %v", result.index, result.err))
			failureCount++
		} else {
			launchedInstances = append(launchedInstances, result.result)
			successCount++
		}
	}

	// Handle partial failures
	if failureCount > 0 {
		prog.Error(fmt.Sprintf("Launching %d instances", count), fmt.Errorf("%d/%d instances failed to launch", failureCount, count))

		auditLog.LogOperationWithData("launch_job_array", jobArrayID, "failed",
			map[string]interface{}{
				"success_count": successCount,
				"failure_count": failureCount,
			}, fmt.Errorf("%d/%d instances failed", failureCount, count))

		// Terminate successfully launched instances
		if successCount > 0 {
			fmt.Fprintf(os.Stderr, "\n⚠️  Cleaning up %d successfully launched instances...\n", successCount)
			for _, inst := range launchedInstances {
				_ = awsClient.Terminate(ctx, baseConfig.Region, inst.InstanceID)
			}
		}

		// Return detailed error
		return fmt.Errorf("job array launch failed: %d/%d instances failed:\n  %s",
			failureCount, count, strings.Join(launchErrors, "\n  "))
	}

	auditLog.LogOperationWithData("launch_job_array", jobArrayID, "success",
		map[string]interface{}{
			"instance_count": successCount,
		}, nil)

	prog.Complete(fmt.Sprintf("Launching %d instances", count))

	// Sort instances by index for consistent display
	sort.Slice(launchedInstances, func(i, j int) bool {
		// Extract index from Name (assumes format: name-{index})
		getName := func(r *aws.LaunchResult) int {
			parts := strings.Split(r.Name, "-")
			if len(parts) > 0 {
				if idx, err := strconv.Atoi(parts[len(parts)-1]); err == nil {
					return idx
				}
			}
			return 0
		}
		return getName(launchedInstances[i]) < getName(launchedInstances[j])
	})

	// Phase 2: Wait for all instances to reach "running" state
	prog.Start("Waiting for all instances to reach running state")
	maxWaitTime := 2 * time.Minute
	checkInterval := 5 * time.Second
	startTime := time.Now()

	allRunning := false
	for time.Since(startTime) < maxWaitTime {
		allRunning = true
		for _, inst := range launchedInstances {
			state, err := awsClient.GetInstanceState(ctx, baseConfig.Region, inst.InstanceID)
			if err != nil || state != "running" {
				allRunning = false
				break
			}
		}

		if allRunning {
			break
		}

		time.Sleep(checkInterval)
	}

	if !allRunning {
		prog.Error("Waiting for instances", fmt.Errorf("timeout waiting for all instances to reach running state"))
		return fmt.Errorf("timeout: not all instances reached running state within %v", maxWaitTime)
	}

	prog.Complete("Waiting for all instances")

	// Phase 3: Get public IPs for all instances
	prog.Start("Getting public IPs")
	for _, inst := range launchedInstances {
		publicIP, err := awsClient.GetInstancePublicIP(ctx, baseConfig.Region, inst.InstanceID)
		if err != nil {
			prog.Error("Getting public IP", err)
			// Non-fatal: continue with other instances
			fmt.Fprintf(os.Stderr, "\n⚠️  Failed to get IP for %s: %v\n", inst.InstanceID, err)
		} else {
			inst.PublicIP = publicIP
		}
	}
	prog.Complete("Getting public IPs")

	// Note: Peer discovery is handled dynamically by spored agent
	// Each agent queries EC2 for all instances with the same spawn:job-array-id tag
	// This avoids AWS tag size limitations (256 char max) and scales to any array size

	return renderJobArrayResult(launchedInstances, baseConfig, jobArrayID, createdAt, plat)
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
