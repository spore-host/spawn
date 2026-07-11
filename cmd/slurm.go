package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/slurm"
	"gopkg.in/yaml.v3"
)

var (
	slurmOutputFile string
	slurmForceYes   bool
)

// slurmCmd represents the slurm command
var slurmCmd = &cobra.Command{
	Use:   "slurm",
	Short: "Slurm batch script interpreter for cloud migration",
	Long: `Parse and convert Slurm batch scripts to spawn parameter files.

This enables HPC users to migrate existing Slurm workflows to the cloud
with minimal changes. Supports array jobs, MPI jobs, and GPU jobs.

Examples:
  # Convert Slurm script to spawn parameters
  spawn slurm convert job.sbatch --output params.yaml

  # Estimate cost before running
  spawn slurm estimate job.sbatch

  # Convert and submit in one step
  spawn slurm submit job.sbatch --spot
`,
}

// slurmConvertCmd converts a Slurm script to spawn parameters
var slurmConvertCmd = &cobra.Command{
	Use:   "convert <script.sbatch>",
	Short: "Convert Slurm batch script to spawn parameter file",
	Long: `Parse a Slurm batch script and convert it to spawn parameter format.

The generated parameter file can be reviewed and edited before launching.

Supported Slurm directives:
  --array=N-M          → Parameter sweep with M-N+1 tasks
  --time=HH:MM:SS      → TTL for each instance
  --mem=XGB            → Memory requirement for instance selection
  --cpus-per-task=N    → CPU requirement for instance selection
  --gres=gpu:N         → GPU requirement and instance selection
  --nodes=N            → Multi-node MPI job (requires --mpi flag)
  --job-name=NAME      → Instance name prefix

Custom #SPAWN directives (optional):
  #SPAWN --instance-type=TYPE  → Override instance type selection
  #SPAWN --region=REGION       → Override region
  #SPAWN --spot=true           → Enable spot instances
  #SPAWN --ami=AMI_ID          → Override AMI

Example:
  spawn slurm convert train.sbatch --output params.yaml
  spawn launch --params params.yaml
`,
	Args: cobra.ExactArgs(1),
	RunE: runSlurmConvert,
}

// slurmEstimateCmd estimates the cost of running a Slurm script
var slurmEstimateCmd = &cobra.Command{
	Use:   "estimate <script.sbatch>",
	Short: "Estimate cost of running Slurm batch script on spawn",
	Long: `Parse a Slurm batch script and estimate the cloud cost.

Provides a cost comparison between institutional cluster (free but queued)
and cloud (paid but immediate).

Example:
  spawn slurm estimate train.sbatch
`,
	Args: cobra.ExactArgs(1),
	RunE: runSlurmEstimate,
}

// slurmSubmitCmd converts and submits a Slurm script
var slurmSubmitCmd = &cobra.Command{
	Use:   "submit <script.sbatch>",
	Short: "Convert and submit Slurm batch script to spawn",
	Long: `Parse a Slurm batch script, convert to spawn parameters, and launch immediately.

This is a convenience command that combines 'convert' and 'launch' in one step.
For complex jobs, consider using 'convert' first to review the generated parameters.

Example:
  spawn slurm submit train.sbatch --spot --yes
`,
	Args: cobra.ExactArgs(1),
	RunE: runSlurmSubmit,
}

func init() {
	rootCmd.AddCommand(slurmCmd)
	slurmCmd.AddCommand(slurmConvertCmd)
	slurmCmd.AddCommand(slurmEstimateCmd)
	slurmCmd.AddCommand(slurmSubmitCmd)

	// Convert flags
	slurmConvertCmd.Flags().StringVar(&slurmOutputFile, "output-file", "", "Output parameter file (default: stdout)")
	// Deprecated alias for --output-file (shadowed the root -o/--output format flag).
	slurmConvertCmd.Flags().StringVarP(&slurmOutputFile, "output", "o", "", "Output parameter file (default: stdout)")
	_ = slurmConvertCmd.Flags().MarkDeprecated("output", "use --output-file instead")

	// Submit flags
	slurmSubmitCmd.Flags().BoolVarP(&slurmForceYes, "yes", "y", false, "Skip confirmation prompt")
}

func runSlurmConvert(cmd *cobra.Command, args []string) error {
	scriptPath := args[0]

	// Parse Slurm script
	fmt.Fprintf(os.Stderr, "Parsing Slurm script: %s\n", scriptPath)
	job, err := slurm.ParseSlurmScript(scriptPath)
	if err != nil {
		return fmt.Errorf("failed to parse Slurm script: %w", err)
	}

	// Print job summary
	printJobSummary(job)

	// Convert to spawn parameters
	fmt.Fprintf(os.Stderr, "\nConverting to spawn parameter format...\n")
	config, err := slurm.ConvertToSpawn(job)
	if err != nil {
		return fmt.Errorf("failed to convert to spawn format: %w", err)
	}

	// Marshal to YAML
	yamlData, err := yaml.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal YAML: %w", err)
	}

	// Write output
	if slurmOutputFile != "" {
		fmt.Fprintf(os.Stderr, "Writing parameters to: %s\n", slurmOutputFile)
		if err := os.WriteFile(slurmOutputFile, yamlData, 0644); err != nil {
			return fmt.Errorf("failed to write output file: %w", err)
		}
		fmt.Fprintf(os.Stderr, "\n✅ Conversion complete!\n")
		fmt.Fprintf(os.Stderr, "\nTo launch: spawn launch --params %s\n", slurmOutputFile)
	} else {
		// Write to stdout
		fmt.Println(string(yamlData))
	}

	return nil
}

func runSlurmEstimate(cmd *cobra.Command, args []string) error {
	scriptPath := args[0]

	// Parse Slurm script
	fmt.Fprintf(os.Stderr, "Parsing Slurm script: %s\n\n", scriptPath)
	job, err := slurm.ParseSlurmScript(scriptPath)
	if err != nil {
		return fmt.Errorf("failed to parse Slurm script: %w", err)
	}

	// Print job summary
	printJobSummary(job)

	// Select instance type
	instanceType, err := slurm.SelectInstanceType(job)
	if err != nil {
		return fmt.Errorf("failed to select instance type: %w", err)
	}

	// Get instance info
	spec, ok := slurm.GetInstanceTypeInfo(instanceType)
	if !ok {
		return fmt.Errorf("unknown instance type: %s", instanceType)
	}

	// Estimate cost
	estimatedCost, err := slurm.EstimateCost(job)
	if err != nil {
		return fmt.Errorf("failed to estimate cost: %w", err)
	}

	// Print cost estimate
	fmt.Fprintf(os.Stderr, "\n📊 Spawn Translation:\n")
	fmt.Fprintf(os.Stderr, "  Instance type:       %s (spot)\n", instanceType)
	fmt.Fprintf(os.Stderr, "  vCPUs:               %d\n", spec.VCPUs)
	fmt.Fprintf(os.Stderr, "  Memory:              %d MB\n", spec.MemoryMB)
	if spec.GPUs > 0 {
		fmt.Fprintf(os.Stderr, "  GPUs:                %d × %s\n", spec.GPUs, spec.GPUType)
	}

	if job.IsArrayJob() {
		fmt.Fprintf(os.Stderr, "  Total tasks:         %d\n", job.GetTotalTasks())
		if job.Array.MaxRunning > 0 {
			fmt.Fprintf(os.Stderr, "  Max concurrent:      %d\n", job.Array.MaxRunning)
		}
	} else if job.IsMPIJob() {
		fmt.Fprintf(os.Stderr, "  MPI nodes:           %d\n", job.Nodes)
		fmt.Fprintf(os.Stderr, "  Tasks per node:      %d\n", job.TasksPerNode)
		fmt.Fprintf(os.Stderr, "  Total MPI ranks:     %d\n", job.Nodes*job.TasksPerNode)
	}

	fmt.Fprintf(os.Stderr, "\n💰 Cost Estimate:\n")
	fmt.Fprintf(os.Stderr, "  Estimated cost:      $%.2f (spot pricing)\n", estimatedCost)
	fmt.Fprintf(os.Stderr, "  On-demand cost:      $%.2f (if spot unavailable)\n", estimatedCost/0.3)

	fmt.Fprintf(os.Stderr, "\n⚡ Time Savings:\n")
	fmt.Fprintf(os.Stderr, "  Cluster queue time:  2-24 hours (typical)\n")
	fmt.Fprintf(os.Stderr, "  Spawn launch time:   <5 minutes\n")
	fmt.Fprintf(os.Stderr, "  Time saved:          Immediate launch, no queue wait\n")

	fmt.Fprintf(os.Stderr, "\n📝 Next Steps:\n")
	fmt.Fprintf(os.Stderr, "  1. Review the estimate above\n")
	fmt.Fprintf(os.Stderr, "  2. Convert: spawn slurm convert %s --output params.yaml\n", scriptPath)
	fmt.Fprintf(os.Stderr, "  3. Launch:  spawn launch --params params.yaml\n")
	fmt.Fprintf(os.Stderr, "\n  Or submit directly: spawn slurm submit %s --yes\n", scriptPath)

	return nil
}

func runSlurmSubmit(cmd *cobra.Command, args []string) error {
	scriptPath := args[0]

	// Parse Slurm script
	fmt.Fprintf(os.Stderr, "Parsing Slurm script: %s\n", scriptPath)
	job, err := slurm.ParseSlurmScript(scriptPath)
	if err != nil {
		return fmt.Errorf("failed to parse Slurm script: %w", err)
	}

	// Print job summary
	printJobSummary(job)

	// Estimate cost
	estimatedCost, err := slurm.EstimateCost(job)
	if err != nil {
		return fmt.Errorf("failed to estimate cost: %w", err)
	}

	fmt.Fprintf(os.Stderr, "\n💰 Estimated cost: $%.2f (spot pricing)\n\n", estimatedCost)

	// Confirm unless --yes flag
	if !slurmForceYes {
		fmt.Fprintf(os.Stderr, "Do you want to proceed? [y/N] ")
		var response string
		_, _ = fmt.Scanln(&response)
		if response != "y" && response != "Y" && response != "yes" {
			fmt.Fprintf(os.Stderr, "Cancelled.\n")
			return nil
		}
	}

	// Convert to spawn parameters
	fmt.Fprintf(os.Stderr, "Converting to spawn parameter format...\n")
	config, err := slurm.ConvertToSpawn(job)
	if err != nil {
		return fmt.Errorf("failed to convert to spawn format: %w", err)
	}

	// Write to temporary file
	tmpFile, err := os.CreateTemp("", "spawn-slurm-*.yaml")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	defer func() { _ = os.Remove(tmpFile.Name()) }()

	yamlData, err := yaml.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal YAML: %w", err)
	}

	if _, err := tmpFile.Write(yamlData); err != nil {
		return fmt.Errorf("failed to write temp file: %w", err)
	}
	_ = tmpFile.Close()

	// Hand off to spawn launch by invoking the current binary with
	// the converted param file. This keeps launch logic in one place and
	// respects all global flags (--output, --lang, etc.) already parsed.
	fmt.Fprintf(os.Stderr, "Launching via spawn...\n\n")

	self, err := os.Executable()
	if err != nil {
		self = "spawn"
	}

	launchArgs := []string{"launch", "--param-file", tmpFile.Name()}
	if slurmForceYes {
		launchArgs = append(launchArgs, "--yes")
	}
	// Propagate the spot flag if set
	if spot {
		launchArgs = append(launchArgs, "--spot")
	}

	launchCmd := exec.CommandContext(context.Background(), self, launchArgs...) // nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	launchCmd.Stdin = os.Stdin
	launchCmd.Stdout = os.Stdout
	launchCmd.Stderr = os.Stderr
	return launchCmd.Run()
}

// printJobSummary prints a summary of the parsed Slurm job
func printJobSummary(job *slurm.SlurmJob) {
	fmt.Fprintf(os.Stderr, "\n📋 Slurm Job Analysis:\n")
	if job.JobName != "" {
		fmt.Fprintf(os.Stderr, "  Job name:            %s\n", job.JobName)
	}
	if job.Partition != "" {
		fmt.Fprintf(os.Stderr, "  Partition:           %s\n", job.Partition)
	}

	if job.IsArrayJob() {
		fmt.Fprintf(os.Stderr, "  Job type:            Array job\n")
		fmt.Fprintf(os.Stderr, "  Array range:         %d-%d", job.Array.Start, job.Array.End)
		if job.Array.Step > 1 {
			fmt.Fprintf(os.Stderr, ":%d", job.Array.Step)
		}
		if job.Array.MaxRunning > 0 {
			fmt.Fprintf(os.Stderr, " (max %d concurrent)", job.Array.MaxRunning)
		}
		fmt.Fprintf(os.Stderr, "\n")
		fmt.Fprintf(os.Stderr, "  Total tasks:         %d\n", job.GetTotalTasks())
	} else if job.IsMPIJob() {
		fmt.Fprintf(os.Stderr, "  Job type:            MPI job\n")
		fmt.Fprintf(os.Stderr, "  Nodes:               %d\n", job.Nodes)
		fmt.Fprintf(os.Stderr, "  Tasks per node:      %d\n", job.TasksPerNode)
		fmt.Fprintf(os.Stderr, "  Total MPI ranks:     %d\n", job.Nodes*job.TasksPerNode)
	} else {
		fmt.Fprintf(os.Stderr, "  Job type:            Single task\n")
	}

	if job.TimeLimit > 0 {
		fmt.Fprintf(os.Stderr, "  Time limit:          %s\n", job.TimeLimit)
	}
	if job.MemoryMB > 0 {
		if job.MemoryMB >= 1024 {
			fmt.Fprintf(os.Stderr, "  Memory:              %d GB\n", job.MemoryMB/1024)
		} else {
			fmt.Fprintf(os.Stderr, "  Memory:              %d MB\n", job.MemoryMB)
		}
	}
	if job.CPUsPerTask > 0 {
		fmt.Fprintf(os.Stderr, "  CPUs per task:       %d\n", job.CPUsPerTask)
	}
	if job.GPUs > 0 {
		fmt.Fprintf(os.Stderr, "  GPUs:                %d", job.GPUs)
		if job.GPUType != "" {
			fmt.Fprintf(os.Stderr, " × %s", job.GPUType)
		}
		fmt.Fprintf(os.Stderr, "\n")
	}
}
