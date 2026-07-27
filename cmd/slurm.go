package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/slurm"
	truffleaws "github.com/spore-host/truffle/pkg/aws"
	"gopkg.in/yaml.v3"
)

var (
	slurmOutputFile string
	slurmForceYes   bool
	slurmRegion     string
)

// slurmCostEstimate resolves the region the job would run in and prices it with
// truffle's live pricer (Price List + Spot history), which is the suite's pricing
// authority. spawn keeps no rate table of its own: the one it had drifted to 79%
// over the real p5 rate, and can't express the ~70% swing between regions (#447).
//
// Precedence for the region: --region, then the script's #SPAWN --region, then
// the resolved AWS config region.
func slurmCostEstimate(ctx context.Context, job *slurm.SlurmJob) (*slurm.CostEstimate, error) {
	region := slurmRegion
	if region == "" {
		region = job.SpawnRegion
	}

	awsClient, err := spawnaws.NewClientWithRegion(ctx, region)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize AWS client: %w", err)
	}
	if region == "" {
		region = awsClient.Config().Region
	}
	if region == "" {
		return nil, fmt.Errorf("no region resolved for the cost estimate; pass --region or set one in your AWS config")
	}

	tc := truffleaws.NewClientFromConfig(awsClient.Config())
	// Opt out of truffle's default static fallback. That fallback exists so a Price
	// List outage degrades to an estimate instead of zeroed savings, which is right
	// for a savings percentage but wrong here: this figure is what a user says yes to
	// before spawn launches billable instances. Its table has no entry for newer
	// families, so it answers with a family estimate — hpc7a.96xlarge came back at
	// $0.20/hr against a real $7.20 — and a plausible wrong price is worse than an
	// error. With the Price List pricer alone, an unpriceable type says so.
	tc.SetOnDemandPricer(truffleaws.NewAWSOnDemandPricer(awsClient.Config()))
	return slurm.EstimateCostWithPricer(ctx, job, tc, region)
}

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

	// Cost estimates are priced per-region (GPU On-Demand rates vary by up to 70%
	// between regions), so both commands that quote a cost accept a region.
	for _, c := range []*cobra.Command{slurmEstimateCmd, slurmSubmitCmd} {
		c.Flags().StringVar(&slurmRegion, "region", "", "Region to price the estimate in (default: #SPAWN --region, else your AWS config region)")
	}
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

	// Estimate cost. This also selects the instance type, so the printed specs and
	// the priced type can never disagree.
	est, err := slurmCostEstimate(cmd.Context(), job)
	if err != nil {
		return fmt.Errorf("failed to estimate cost: %w", err)
	}

	// Print cost estimate
	fmt.Fprintf(os.Stderr, "\n📊 Spawn Translation:\n")
	fmt.Fprintf(os.Stderr, "  Instance type:       %s\n", est.InstanceType)
	fmt.Fprintf(os.Stderr, "  Region:              %s\n", est.Region)
	// Specs are shown when the selection table knows the type. A #SPAWN
	// --instance-type override can name any type AWS offers, and the table only
	// covers the ones spawn selects from — so an unknown type omits the spec lines
	// rather than failing the whole estimate. The cost, which is what the command
	// is for, comes from AWS and works for any type.
	if spec, ok := slurm.GetInstanceTypeInfo(est.InstanceType); ok {
		fmt.Fprintf(os.Stderr, "  vCPUs:               %d\n", spec.VCPUs)
		fmt.Fprintf(os.Stderr, "  Memory:              %d MB\n", spec.MemoryMB)
		if spec.GPUs > 0 {
			fmt.Fprintf(os.Stderr, "  GPUs:                %d × %s\n", spec.GPUs, spec.GPUType)
		}
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
	fmt.Fprintf(os.Stderr, "  Instance hours:      %.1f\n", est.InstanceHours)
	// Rates are stated alongside the totals: the hourly number is what a user can
	// check against the AWS console, and it makes clear the total is rate × hours
	// rather than an opaque figure.
	fmt.Fprintf(os.Stderr, "  On-demand cost:      $%.2f  ($%.4f/hr)\n", est.OnDemandCost, est.OnDemandRate)
	if est.SpotUnavailable {
		fmt.Fprintf(os.Stderr, "  Spot cost:           unavailable — no spot price published for %s in %s\n", est.InstanceType, est.Region)
	} else {
		fmt.Fprintf(os.Stderr, "  Spot cost:           $%.2f  ($%.4f/hr, %.0f%% off on-demand)\n",
			est.SpotCost, est.SpotRate, (1-est.SpotRate/est.OnDemandRate)*100)
	}
	fmt.Fprintf(os.Stderr, "  Rates:               live AWS pricing via truffle (%s)\n", est.Region)

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

	// Estimate cost. This gates a real, billable launch, so the figure comes from
	// live AWS pricing — a stale estimate here is what a user says yes to.
	est, err := slurmCostEstimate(cmd.Context(), job)
	if err != nil {
		return fmt.Errorf("failed to estimate cost: %w", err)
	}

	if est.SpotUnavailable {
		fmt.Fprintf(os.Stderr, "\n💰 Estimated cost: $%.2f on-demand (%s in %s, %.1f instance-hours; no spot price published)\n\n",
			est.OnDemandCost, est.InstanceType, est.Region, est.InstanceHours)
	} else {
		fmt.Fprintf(os.Stderr, "\n💰 Estimated cost: $%.2f spot / $%.2f on-demand (%s in %s, %.1f instance-hours)\n\n",
			est.SpotCost, est.OnDemandCost, est.InstanceType, est.Region, est.InstanceHours)
	}

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
