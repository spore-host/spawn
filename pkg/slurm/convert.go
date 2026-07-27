package slurm

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// SpawnConfig represents the spawn parameter file format
type SpawnConfig struct {
	Defaults map[string]interface{}   `yaml:"defaults" json:"defaults"`
	Params   []map[string]interface{} `yaml:"params" json:"params"`
}

// ConvertToSpawn converts a Slurm job to spawn parameter format
func ConvertToSpawn(job *SlurmJob) (*SpawnConfig, error) {
	config := &SpawnConfig{
		Defaults: make(map[string]interface{}),
		Params:   []map[string]interface{}{},
	}

	// Select instance type based on requirements
	instanceType, err := SelectInstanceType(job)
	if err != nil {
		return nil, fmt.Errorf("failed to select instance type: %w", err)
	}

	// Set defaults
	config.Defaults["instance_type"] = instanceType

	// Set TTL from time limit
	if job.TimeLimit > 0 {
		config.Defaults["ttl"] = formatDuration(job.TimeLimit)
	}

	// Set region if specified
	if job.SpawnRegion != "" {
		config.Defaults["region"] = job.SpawnRegion
	}

	// Set spot if specified
	if job.SpawnSpot {
		config.Defaults["spot"] = true
	}

	// Set AMI if specified
	if job.SpawnAMI != "" {
		config.Defaults["ami"] = job.SpawnAMI
	}

	// Handle array jobs
	if job.IsArrayJob() {
		return convertArrayJob(job, config)
	}

	// Handle MPI jobs
	if job.IsMPIJob() {
		return convertMPIJob(job, config)
	}

	// Handle single-task job
	return convertSingleJob(job, config)
}

// convertArrayJob converts a Slurm array job to spawn parameters
func convertArrayJob(job *SlurmJob, config *SpawnConfig) (*SpawnConfig, error) {
	// Generate parameter sets for each array task
	for i := job.Array.Start; i <= job.Array.End; i += job.Array.Step {
		param := make(map[string]interface{})
		param["index"] = i

		// Generate script with SLURM environment variables
		script := generateArrayTaskScript(job, i)
		param["script"] = script

		// Set job name with array index
		if job.JobName != "" {
			param["name"] = fmt.Sprintf("%s-%d", job.JobName, i)
		}

		config.Params = append(config.Params, param)
	}

	// Handle max concurrent with launch settings
	if job.Array.MaxRunning > 0 {
		config.Defaults["max_concurrent"] = job.Array.MaxRunning
	}

	return config, nil
}

// convertMPIJob converts a multi-node MPI job to spawn parameters
func convertMPIJob(job *SlurmJob, config *SpawnConfig) (*SpawnConfig, error) {
	// Add MPI-specific settings
	config.Defaults["mpi"] = true
	config.Defaults["count"] = job.Nodes

	// Add single parameter set with the script
	param := make(map[string]interface{})

	// Generate script with SLURM environment variables
	script := generateMPIScript(job)
	param["script"] = script

	if job.JobName != "" {
		param["name"] = job.JobName
	}

	config.Params = append(config.Params, param)

	return config, nil
}

// convertSingleJob converts a single-task job to spawn parameters
func convertSingleJob(job *SlurmJob, config *SpawnConfig) (*SpawnConfig, error) {
	param := make(map[string]interface{})

	// Generate script with SLURM environment variables
	script := generateSingleTaskScript(job)
	param["script"] = script

	if job.JobName != "" {
		param["name"] = job.JobName
	}

	config.Params = append(config.Params, param)

	return config, nil
}

// generateArrayTaskScript generates the script for an array task
func generateArrayTaskScript(job *SlurmJob, taskID int) string {
	var sb strings.Builder

	// Set SLURM environment variables
	sb.WriteString("#!/bin/bash\n")
	sb.WriteString("# Spawn-generated script from Slurm batch file\n")
	sb.WriteString("# Original file: ")
	sb.WriteString(job.FilePath)
	sb.WriteString("\n\n")

	// Export SLURM-compatible environment variables
	sb.WriteString("# SLURM environment variables\n")
	fmt.Fprintf(&sb, "export SLURM_ARRAY_TASK_ID=%d\n", taskID)
	sb.WriteString("export SLURM_ARRAY_JOB_ID=${SPAWN_SWEEP_ID:-unknown}\n")
	sb.WriteString("export SLURM_JOB_ID=${SPAWN_INSTANCE_ID:-unknown}\n")
	fmt.Fprintf(&sb, "export SLURM_ARRAY_TASK_MIN=%d\n", job.Array.Start)
	fmt.Fprintf(&sb, "export SLURM_ARRAY_TASK_MAX=%d\n", job.Array.End)
	fmt.Fprintf(&sb, "export SLURM_ARRAY_TASK_STEP=%d\n", job.Array.Step)

	if job.JobName != "" {
		fmt.Fprintf(&sb, "export SLURM_JOB_NAME=%s\n", job.JobName)
	}
	if job.WorkingDir != "" {
		fmt.Fprintf(&sb, "cd %s\n", job.WorkingDir)
	}
	sb.WriteString("\n")

	// Add the original script body
	sb.WriteString("# Original script body\n")
	sb.WriteString(job.ScriptBody)

	return sb.String()
}

// generateMPIScript generates the script for an MPI job
func generateMPIScript(job *SlurmJob) string {
	var sb strings.Builder

	// Set SLURM environment variables
	sb.WriteString("#!/bin/bash\n")
	sb.WriteString("# Spawn-generated MPI script from Slurm batch file\n")
	sb.WriteString("# Original file: ")
	sb.WriteString(job.FilePath)
	sb.WriteString("\n\n")

	// Export SLURM-compatible environment variables
	sb.WriteString("# SLURM environment variables\n")
	sb.WriteString("export SLURM_JOB_ID=${SPAWN_INSTANCE_ID:-unknown}\n")
	fmt.Fprintf(&sb, "export SLURM_NNODES=%d\n", job.Nodes)
	fmt.Fprintf(&sb, "export SLURM_NTASKS=%d\n", job.Nodes*job.TasksPerNode)
	fmt.Fprintf(&sb, "export SLURM_NTASKS_PER_NODE=%d\n", job.TasksPerNode)

	if job.JobName != "" {
		fmt.Fprintf(&sb, "export SLURM_JOB_NAME=%s\n", job.JobName)
	}
	if job.WorkingDir != "" {
		fmt.Fprintf(&sb, "cd %s\n", job.WorkingDir)
	}
	sb.WriteString("\n")

	// Add the original script body
	sb.WriteString("# Original script body\n")
	sb.WriteString(job.ScriptBody)

	return sb.String()
}

// generateSingleTaskScript generates the script for a single task
func generateSingleTaskScript(job *SlurmJob) string {
	var sb strings.Builder

	// Set SLURM environment variables
	sb.WriteString("#!/bin/bash\n")
	sb.WriteString("# Spawn-generated script from Slurm batch file\n")
	sb.WriteString("# Original file: ")
	sb.WriteString(job.FilePath)
	sb.WriteString("\n\n")

	// Export SLURM-compatible environment variables
	sb.WriteString("# SLURM environment variables\n")
	sb.WriteString("export SLURM_JOB_ID=${SPAWN_INSTANCE_ID:-unknown}\n")

	if job.JobName != "" {
		fmt.Fprintf(&sb, "export SLURM_JOB_NAME=%s\n", job.JobName)
	}
	if job.WorkingDir != "" {
		fmt.Fprintf(&sb, "cd %s\n", job.WorkingDir)
	}
	sb.WriteString("\n")

	// Add the original script body
	sb.WriteString("# Original script body\n")
	sb.WriteString(job.ScriptBody)

	return sb.String()
}

// formatDuration formats a time.Duration as a string for spawn TTL
func formatDuration(d time.Duration) string {
	// Convert to hours, minutes, seconds
	hours := int(d.Hours())
	minutes := int(d.Minutes()) % 60
	seconds := int(d.Seconds()) % 60

	if hours > 0 && minutes == 0 && seconds == 0 {
		return fmt.Sprintf("%dh", hours)
	}
	if hours > 0 && seconds == 0 {
		return fmt.Sprintf("%dh%dm", hours, minutes)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh%dm%ds", hours, minutes, seconds)
	}
	if minutes > 0 && seconds == 0 {
		return fmt.Sprintf("%dm", minutes)
	}
	if minutes > 0 {
		return fmt.Sprintf("%dm%ds", minutes, seconds)
	}
	return fmt.Sprintf("%ds", seconds)
}

// TotalInstanceHours returns the billable instance-hours the job implies: task
// count × time limit for an array job (respecting --array's concurrency limit),
// nodes × time limit for MPI, or one instance's time limit otherwise.
//
// This is the part of a cost estimate that depends only on the Slurm script, so
// it is separated from the $/hr rate — the rate comes from AWS (see
// [EstimateCostWithPricer]) and needs credentials, while this does not.
func TotalInstanceHours(job *SlurmJob) float64 {
	if job.IsArrayJob() {
		// Array job: each task runs for time limit
		numTasks := job.GetTotalTasks()
		hours := job.TimeLimit.Hours()

		// If max concurrent specified, calculate wall time
		if job.Array.MaxRunning > 0 {
			// Tasks run in batches
			batches := float64(numTasks) / float64(job.Array.MaxRunning)
			wallTime := batches * hours
			return float64(job.Array.MaxRunning) * wallTime
		}
		// All tasks run concurrently
		return float64(numTasks) * hours
	}
	if job.IsMPIJob() {
		// MPI job: N nodes × time limit
		return float64(job.Nodes) * job.TimeLimit.Hours()
	}
	// Single job
	return job.TimeLimit.Hours()
}

// Pricer supplies the hourly rate for an instance type in a region. It is
// satisfied by truffle's *aws.Client (Client.HourlyRate), which queries the live
// AWS Price List and Spot history and caches the result.
//
// This is an interface rather than a direct dependency so pkg/slurm stays
// unit-testable without AWS, and so the estimate has exactly one source of truth
// for money. Rates are NOT kept in this package: the hardcoded table that used to
// supply them drifted badly enough to overstate p5 by 79% (#447), and On-Demand
// GPU rates vary by up to 70% between regions, which a single number cannot
// represent at all.
type Pricer interface {
	// HourlyRate returns the $/hr for instanceType in region under the given
	// purchase model ("on-demand" or "spot").
	HourlyRate(ctx context.Context, instanceType, region, model string) (float64, error)
}

// CostEstimate is a Slurm job's projected cost, with the rates it was computed
// from so a caller can show what the number is based on.
type CostEstimate struct {
	InstanceType    string
	Region          string
	InstanceHours   float64
	OnDemandRate    float64 // $/hr, live
	SpotRate        float64 // $/hr, live; 0 when no spot price is published
	OnDemandCost    float64
	SpotCost        float64 // 0 when SpotRate is 0
	SpotUnavailable bool    // true when no spot price could be read for the type
}

// EstimateCostWithPricer estimates the cost of running a Slurm job on spawn,
// taking both rates from pricer (i.e. from AWS) rather than from any table in
// this repo.
//
// region is where the job would actually run — GPU On-Demand rates differ by as
// much as 70% across regions, so a region-less estimate is not meaningful for the
// accelerator types this command is most often used for.
//
// A missing spot price is reported in the result rather than returned as an
// error: newly-launched accelerator types often have no published spot price, and
// the On-Demand figure is still the answer to "what would this cost".
func EstimateCostWithPricer(ctx context.Context, job *SlurmJob, pricer Pricer, region string) (*CostEstimate, error) {
	instanceType, err := SelectInstanceType(job)
	if err != nil {
		return nil, err
	}

	hours := TotalInstanceHours(job)

	onDemand, err := pricer.HourlyRate(ctx, instanceType, region, "on-demand")
	if err != nil {
		return nil, fmt.Errorf("on-demand rate for %s in %s: %w", instanceType, region, err)
	}

	est := &CostEstimate{
		InstanceType:  instanceType,
		Region:        region,
		InstanceHours: hours,
		OnDemandRate:  onDemand,
		OnDemandCost:  hours * onDemand,
	}

	// Spot is the default the slurm commands quote, but it is a real market rate,
	// not a fixed fraction of On-Demand. The old flat 70%-off assumption was wrong
	// in both directions — measured 2026-07-27 in us-east-1, the actual discount
	// ranged from 38% (p5.48xlarge) to 62% (c5.4xlarge).
	spot, spotErr := pricer.HourlyRate(ctx, instanceType, region, "spot")
	if spotErr != nil || spot <= 0 {
		est.SpotUnavailable = true
		return est, nil
	}
	est.SpotRate = spot
	est.SpotCost = hours * spot

	return est, nil
}
