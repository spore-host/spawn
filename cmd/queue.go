package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spf13/cobra"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
	"github.com/spore-host/spawn/pkg/queue"
	"github.com/spore-host/spawn/pkg/sshkey"
)

var (
	queueOutputDir string
)

var queueCmd = &cobra.Command{
	Use:   "queue",
	Short: "Manage batch job queues",
	Long: `Commands for managing and monitoring batch job queues.

Batch queues execute jobs sequentially on a single instance, with
dependency management and automatic result collection.

Examples:
  # Check queue status on instance
  spawn queue status i-1234567890abcdef0

  # Download queue results
  spawn queue results queue-20260122-140530 --output ./results/
`,
}

var queueStatusCmd = &cobra.Command{
	Use:   "status <instance-id>",
	Short: "Show queue execution status",
	Long: `Show the execution status of a batch queue running on an instance.

Connects to the instance via SSH and reads the queue state file.

Examples:
  spawn queue status i-1234567890abcdef0
`,
	Args: cobra.ExactArgs(1),
	RunE: runQueueStatus,
}

var queueResultsCmd = &cobra.Command{
	Use:   "results <queue-id>",
	Short: "Download queue results from S3",
	Long: `Download all job results from S3 for a completed or running queue.

Results include job outputs, logs, and the final queue state.

Examples:
  # Download to current directory
  spawn queue results queue-20260122-140530

  # Download to specific directory
  spawn queue results queue-20260122-140530 --output ./my-results/
`,
	Args: cobra.ExactArgs(1),
	RunE: runQueueResults,
}

var queueTemplateCmd = &cobra.Command{
	Use:   "template",
	Short: "Manage queue templates",
	Long: `Manage pre-built queue configuration templates.

Templates provide ready-to-use queue configurations for common workflows
with variable substitution for customization.

Available commands:
  list      - List available templates
  show      - Show template details
  generate  - Generate queue config from template
`,
}

var queueTemplateListCmd = &cobra.Command{
	Use:   "list",
	Short: "List available queue templates",
	Long: `List all available queue configuration templates.

Shows template names, descriptions, and required/optional variables.

Examples:
  spawn queue template list
`,
	RunE: runQueueTemplateList,
}

var queueTemplateGenerateCmd = &cobra.Command{
	Use:   "generate <template-name>",
	Short: "Generate queue configuration from template",
	Long: `Generate a queue configuration file from a template with variable substitution.

Variables can be provided via --var flags or use template defaults.

Examples:
  # Generate with defaults, output to file
  spawn queue template generate ml-pipeline --output pipeline.json

  # Provide required variables
  spawn queue template generate ml-pipeline \
    --var INPUT=/data/train.csv \
    --var S3_BUCKET=my-results \
    --output pipeline.json

  # Output to stdout (for piping)
  spawn queue template generate simple-sequential \
    --var S3_BUCKET=results
`,
	Args: cobra.ExactArgs(1),
	RunE: runQueueTemplateGenerate,
}

var queueTemplateShowCmd = &cobra.Command{
	Use:   "show <template-name>",
	Short: "Show template details",
	Long: `Show detailed information about a queue template.

Displays template description, jobs, and all variables with their defaults.

Examples:
  spawn queue template show ml-pipeline
  spawn queue template show etl
`,
	Args: cobra.ExactArgs(1),
	RunE: runQueueTemplateShow,
}

var queueTemplateInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Interactive wizard to create a queue configuration",
	Long: `Launch an interactive wizard to create a custom queue configuration.

Guides you through creating a queue by asking questions about:
- Workflow type and name
- Number of jobs and commands
- Job dependencies
- Timeouts and retry policies
- Result collection
- S3 bucket configuration

Examples:
  spawn queue template init
  spawn queue template init --output my-queue.json
`,
	RunE: runQueueTemplateInit,
}

func init() {
	// Results subcommand flags
	queueResultsCmd.Flags().StringVarP(&queueOutputDir, "output", "o", ".", "Output directory for results")

	// Template generate flags
	queueTemplateGenerateCmd.Flags().StringP("output", "o", "", "Output file (default: stdout)")
	queueTemplateGenerateCmd.Flags().StringToString("var", nil, "Template variables (key=value)")

	// Template init flags
	queueTemplateInitCmd.Flags().StringP("output", "o", "", "Output file (default: queue.json)")

	// Add subcommands
	queueCmd.AddCommand(queueStatusCmd)
	queueCmd.AddCommand(queueResultsCmd)
	queueCmd.AddCommand(queueTemplateCmd)

	// Add template subcommands
	queueTemplateCmd.AddCommand(queueTemplateListCmd)
	queueTemplateCmd.AddCommand(queueTemplateGenerateCmd)
	queueTemplateCmd.AddCommand(queueTemplateShowCmd)
	queueTemplateCmd.AddCommand(queueTemplateInitCmd)

	rootCmd.AddCommand(queueCmd)
}

func runQueueStatus(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	instanceID := args[0]

	fmt.Fprintf(os.Stderr, "\n📊 Queue Status\n")
	fmt.Fprintf(os.Stderr, "   Instance: %s\n\n", instanceID)

	// Get instance details to find public IP/DNS
	fmt.Fprintf(os.Stderr, "Getting instance details...\n")

	cfg, err := spawnconfig.LoadComputeAWSConfig(ctx, region)
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}

	ec2Client := ec2.NewFromConfig(cfg)
	result, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		return fmt.Errorf("failed to describe instance: %w", err)
	}

	if len(result.Reservations) == 0 || len(result.Reservations[0].Instances) == 0 {
		return fmt.Errorf("instance not found: %s", instanceID)
	}

	instance := result.Reservations[0].Instances[0]
	var connectAddr string
	if instance.PublicIpAddress != nil {
		connectAddr = *instance.PublicIpAddress
	} else if instance.PublicDnsName != nil {
		connectAddr = *instance.PublicDnsName
	} else {
		return fmt.Errorf("instance has no public IP or DNS name")
	}

	fmt.Fprintf(os.Stderr, "Connecting to %s...\n", connectAddr)

	// Read state file via SSH
	stateJSON, err := sshReadFile(connectAddr, "/var/lib/spored/queue-state.json")
	if err != nil {
		return fmt.Errorf("failed to read queue state: %w", err)
	}

	// Parse state
	var state struct {
		QueueID   string `json:"queue_id"`
		StartedAt string `json:"started_at"`
		UpdatedAt string `json:"updated_at"`
		Status    string `json:"status"`
		Jobs      []struct {
			JobID           string `json:"job_id"`
			Status          string `json:"status"`
			StartedAt       string `json:"started_at,omitempty"`
			CompletedAt     string `json:"completed_at,omitempty"`
			ExitCode        int    `json:"exit_code,omitempty"`
			Attempt         int    `json:"attempt"`
			PID             int    `json:"pid,omitempty"`
			ErrorMessage    string `json:"error_message,omitempty"`
			ResultsUploaded bool   `json:"results_uploaded"`
		} `json:"jobs"`
	}

	if err := json.Unmarshal([]byte(stateJSON), &state); err != nil {
		return fmt.Errorf("failed to parse queue state: %w", err)
	}

	// Display status
	fmt.Fprintf(os.Stderr, "\nQueue ID:    %s\n", state.QueueID)
	fmt.Fprintf(os.Stderr, "Status:      %s\n", state.Status)
	fmt.Fprintf(os.Stderr, "Started:     %s\n", state.StartedAt)
	fmt.Fprintf(os.Stderr, "Updated:     %s\n", state.UpdatedAt)
	fmt.Fprintf(os.Stderr, "\n")

	// Display jobs
	fmt.Fprintf(os.Stderr, "Jobs:\n")
	fmt.Fprintf(os.Stderr, "%-25s %-12s %-8s %-8s %s\n", "JOB ID", "STATUS", "ATTEMPT", "EXIT", "RESULTS")
	fmt.Fprintf(os.Stderr, "%s\n", strings.Repeat("-", 80))

	for _, job := range state.Jobs {
		exitCode := "-"
		if job.Status == "completed" || job.Status == "failed" {
			exitCode = fmt.Sprintf("%d", job.ExitCode)
		}

		results := "no"
		if job.ResultsUploaded {
			results = "yes"
		}

		pid := ""
		if job.PID > 0 {
			pid = fmt.Sprintf("(PID: %d)", job.PID)
		}

		fmt.Fprintf(os.Stderr, "%-25s %-12s %-8d %-8s %-8s %s\n",
			job.JobID, job.Status, job.Attempt, exitCode, results, pid)

		if job.ErrorMessage != "" {
			fmt.Fprintf(os.Stderr, "  Error: %s\n", job.ErrorMessage)
		}
	}

	fmt.Fprintf(os.Stderr, "\n")
	return nil
}

func runQueueResults(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	queueID := args[0]

	fmt.Fprintf(os.Stderr, "\n📦 Downloading Queue Results\n")
	fmt.Fprintf(os.Stderr, "   Queue ID: %s\n", queueID)
	fmt.Fprintf(os.Stderr, "   Output:   %s\n\n", queueOutputDir)

	// Determine region from queue ID or use default
	downloadRegion := region
	if downloadRegion == "" {
		downloadRegion = "us-east-1"
	}

	// Load AWS config
	cfg, err := spawnconfig.LoadComputeAWSConfig(ctx, downloadRegion)
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}

	s3Client := s3.NewFromConfig(cfg)

	// Determine S3 bucket and prefix
	// This should match what was specified in the queue config
	bucket := fmt.Sprintf("spawn-results-%s", downloadRegion)
	prefix := fmt.Sprintf("queues/%s/", queueID)

	// Create output directory
	if err := os.MkdirAll(queueOutputDir, 0755); err != nil {
		return fmt.Errorf("failed to create output directory: %w", err)
	}

	// List all objects
	fmt.Fprintf(os.Stderr, "Listing results...\n")
	paginator := s3.NewListObjectsV2Paginator(s3Client, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(prefix),
	})

	fileCount := 0
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("failed to list objects: %w", err)
		}

		for _, obj := range page.Contents {
			fileCount++

			// Download object
			localPath := filepath.Join(queueOutputDir, strings.TrimPrefix(*obj.Key, prefix))
			localDir := filepath.Dir(localPath)

			if err := os.MkdirAll(localDir, 0755); err != nil {
				return fmt.Errorf("failed to create directory: %w", err)
			}

			fmt.Fprintf(os.Stderr, "  Downloading: %s\n", strings.TrimPrefix(*obj.Key, prefix))

			getResult, err := s3Client.GetObject(ctx, &s3.GetObjectInput{
				Bucket: aws.String(bucket),
				Key:    obj.Key,
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "    Warning: failed to download: %v\n", err)
				continue
			}

			// Write to file
			outFile, err := os.Create(localPath)
			if err != nil {
				_ = getResult.Body.Close()
				return fmt.Errorf("failed to create file: %w", err)
			}

			_, err = outFile.ReadFrom(getResult.Body)
			_ = getResult.Body.Close()
			_ = outFile.Close()

			if err != nil {
				return fmt.Errorf("failed to write file: %w", err)
			}
		}
	}

	fmt.Fprintf(os.Stderr, "\n✅ Downloaded %d files to %s\n\n", fileCount, queueOutputDir)
	return nil
}

// sshReadFile reads a file from a remote instance via SSH.
// Uses the same key-discovery logic as spawn connect.
func sshReadFile(host, remotePath string) (string, error) {
	// Find a usable SSH key via the shared resolver (spawn-managed keys first,
	// then ~/.ssh defaults). Best-effort: an empty keyPath falls back to the
	// ssh client's own default key selection.
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}
	keyPath, _ := sshkey.Resolve(homeDir, fmt.Sprintf("spawn-key-%s", os.Getenv("USER")))

	args := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=10",
		"-o", "LogLevel=ERROR",
	}
	if keyPath != "" {
		args = append(args, "-i", keyPath)
	}
	args = append(args, fmt.Sprintf("ec2-user@%s", host), "cat "+remotePath)

	out, err := exec.Command("ssh", args...).Output()
	if err != nil {
		return "", fmt.Errorf("ssh to %s failed: %w", host, err)
	}
	return string(out), nil
}

func runQueueTemplateList(cmd *cobra.Command, args []string) error {
	templates, err := queue.ListTemplates()
	if err != nil {
		return fmt.Errorf("list templates: %w", err)
	}

	fmt.Fprintf(os.Stderr, "\nAvailable Queue Templates:\n")
	fmt.Fprintf(os.Stderr, "%s\n", strings.Repeat("━", 50))

	for _, tmpl := range templates {
		fmt.Fprintf(os.Stderr, "\n%s\n", tmpl.Name)
		fmt.Fprintf(os.Stderr, "  %s (%d jobs)\n", tmpl.Description, len(tmpl.Config.Jobs))

		// Show required variables
		var required []string
		var optional []string
		for _, v := range tmpl.Variables {
			if v.Required {
				required = append(required, v.Name)
			} else {
				optional = append(optional, v.Name)
			}
		}

		if len(required) > 0 {
			fmt.Fprintf(os.Stderr, "  Required: %s\n", strings.Join(required, ", "))
		}
		if len(optional) > 0 {
			fmt.Fprintf(os.Stderr, "  Optional: %s\n", strings.Join(optional, ", "))
		}
	}

	fmt.Fprintf(os.Stderr, "\n")
	return nil
}

func runQueueTemplateGenerate(cmd *cobra.Command, args []string) error {
	templateName := args[0]
	output, _ := cmd.Flags().GetString("output")
	vars, _ := cmd.Flags().GetStringToString("var")

	// Load template
	tmpl, err := queue.LoadTemplate(templateName)
	if err != nil {
		return fmt.Errorf("load template: %w", err)
	}

	// Substitute variables
	config, err := tmpl.Substitute(vars)
	if err != nil {
		return fmt.Errorf("substitute variables: %w", err)
	}

	// Marshal to JSON
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	// Output
	if output == "" {
		fmt.Println(string(data))
	} else {
		if err := os.WriteFile(output, data, 0644); err != nil {
			return fmt.Errorf("write output file: %w", err)
		}
		fmt.Fprintf(os.Stderr, "Generated queue configuration: %s\n", output)
	}

	return nil
}

func runQueueTemplateShow(cmd *cobra.Command, args []string) error {
	templateName := args[0]

	tmpl, err := queue.LoadTemplate(templateName)
	if err != nil {
		return fmt.Errorf("load template: %w", err)
	}

	fmt.Fprintf(os.Stderr, "\nTemplate: %s\n", tmpl.Name)
	fmt.Fprintf(os.Stderr, "Description: %s\n\n", tmpl.Description)

	fmt.Fprintf(os.Stderr, "Jobs:\n")
	for i, job := range tmpl.Config.Jobs {
		deps := ""
		if len(job.DependsOn) > 0 {
			deps = fmt.Sprintf(", depends on: %s", strings.Join(job.DependsOn, ", "))
		}
		fmt.Fprintf(os.Stderr, "  %d. %s (timeout: %s%s)\n", i+1, job.JobID, job.Timeout, deps)
	}

	// Required variables
	var required []string
	var optional []string
	for _, v := range tmpl.Variables {
		if v.Required {
			required = append(required, v.Name)
		} else {
			optional = append(optional, fmt.Sprintf("%s (default: %s)", v.Name, v.Default))
		}
	}

	if len(required) > 0 {
		fmt.Fprintf(os.Stderr, "\nRequired Variables:\n")
		for _, v := range required {
			fmt.Fprintf(os.Stderr, "  %s\n", v)
		}
	}

	if len(optional) > 0 {
		fmt.Fprintf(os.Stderr, "\nOptional Variables:\n")
		for _, v := range optional {
			fmt.Fprintf(os.Stderr, "  %s\n", v)
		}
	}

	fmt.Fprintf(os.Stderr, "\n")
	return nil
}

func runQueueTemplateInit(cmd *cobra.Command, args []string) error {
	output, _ := cmd.Flags().GetString("output")
	if output == "" {
		output = "queue.json"
	}

	fmt.Fprintf(os.Stderr, "\n🧙 Queue Configuration Wizard\n\n")
	fmt.Fprintf(os.Stderr, "This wizard will help you create a custom batch queue configuration.\n\n")

	scanner := bufio.NewScanner(os.Stdin)

	// Helper to prompt for input
	prompt := func(question string, defaultVal string) string {
		if defaultVal != "" {
			fmt.Fprintf(os.Stderr, "%s [%s]: ", question, defaultVal)
		} else {
			fmt.Fprintf(os.Stderr, "%s: ", question)
		}
		scanner.Scan()
		val := strings.TrimSpace(scanner.Text())
		if val == "" {
			return defaultVal
		}
		return val
	}

	// Helper to prompt for yes/no
	promptYesNo := func(question string, defaultYes bool) bool {
		defaultStr := "y/N"
		if defaultYes {
			defaultStr = "Y/n"
		}
		fmt.Fprintf(os.Stderr, "%s [%s]: ", question, defaultStr)
		scanner.Scan()
		val := strings.ToLower(strings.TrimSpace(scanner.Text()))
		if val == "" {
			return defaultYes
		}
		return val == "y" || val == "yes"
	}

	// Queue metadata
	queueID := prompt("Queue ID", "my-queue")
	queueName := prompt("Queue name (description)", "My Batch Queue")

	// Jobs
	fmt.Fprintf(os.Stderr, "\n")
	numJobsStr := prompt("Number of jobs", "3")
	numJobs, err := strconv.Atoi(numJobsStr)
	if err != nil || numJobs < 1 {
		return fmt.Errorf("invalid number of jobs: %s", numJobsStr)
	}

	jobs := make([]queue.JobConfig, numJobs)

	fmt.Fprintf(os.Stderr, "\n--- Job Configuration ---\n\n")

	for i := 0; i < numJobs; i++ {
		fmt.Fprintf(os.Stderr, "Job %d:\n", i+1)

		jobID := prompt("  Job ID", fmt.Sprintf("job%d", i+1))
		command := prompt("  Command to execute", "echo 'Hello World'")
		timeout := prompt("  Timeout (e.g., 5m, 1h, 30s)", "10m")

		// Validate timeout format
		if _, err := time.ParseDuration(timeout); err != nil {
			fmt.Fprintf(os.Stderr, "  ⚠️  Invalid timeout format, using default 10m\n")
			timeout = "10m"
		}

		jobs[i] = queue.JobConfig{
			JobID:   jobID,
			Command: command,
			Timeout: timeout,
		}

		// Dependencies
		if i > 0 && promptYesNo("  Add dependency on previous job?", true) {
			jobs[i].DependsOn = []string{jobs[i-1].JobID}
		}

		// Environment variables
		if promptYesNo("  Add environment variables?", false) {
			jobs[i].Env = make(map[string]string)
			for {
				key := prompt("    Variable name (empty to finish)", "")
				if key == "" {
					break
				}
				value := prompt(fmt.Sprintf("    Value for %s", key), "")
				jobs[i].Env[key] = value
			}
		}

		// Retry configuration
		if promptYesNo("  Configure retry?", false) {
			maxAttemptsStr := prompt("    Max attempts", "3")
			maxAttempts, err := strconv.Atoi(maxAttemptsStr)
			if err != nil || maxAttempts < 1 {
				maxAttempts = 3
			}

			backoff := prompt("    Backoff strategy (exponential/fixed)", "exponential")
			if backoff != "exponential" && backoff != "fixed" {
				backoff = "exponential"
			}

			jobs[i].Retry = &queue.RetryConfig{
				MaxAttempts: maxAttempts,
				Backoff:     backoff,
			}
		}

		// Result paths
		if promptYesNo("  Collect result files?", false) {
			var resultPaths []string
			for {
				path := prompt("    File path or glob pattern (empty to finish)", "")
				if path == "" {
					break
				}
				resultPaths = append(resultPaths, path)
			}
			jobs[i].ResultPaths = resultPaths
		}

		fmt.Fprintf(os.Stderr, "\n")
	}

	// Global settings
	fmt.Fprintf(os.Stderr, "--- Global Settings ---\n\n")

	globalTimeout := prompt("Global timeout (max queue execution time)", "2h")
	if _, err := time.ParseDuration(globalTimeout); err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  Invalid timeout format, using default 2h\n")
		globalTimeout = "2h"
	}

	onFailure := "stop"
	if !promptYesNo("Stop queue on first job failure?", true) {
		onFailure = "continue"
	}

	s3Bucket := prompt("S3 bucket for results", "spawn-results-us-east-1")
	s3Prefix := prompt("S3 prefix (optional)", fmt.Sprintf("queues/%s", queueID))

	// Build queue config
	config := &queue.QueueConfig{
		QueueID:        queueID,
		QueueName:      queueName,
		Jobs:           jobs,
		GlobalTimeout:  globalTimeout,
		OnFailure:      onFailure,
		ResultS3Bucket: s3Bucket,
		ResultS3Prefix: s3Prefix,
	}

	// Validate
	fmt.Fprintf(os.Stderr, "\n🔍 Validating configuration...\n")
	if err := queue.ValidateQueue(config); err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}
	fmt.Fprintf(os.Stderr, "✓ Configuration is valid\n\n")

	// Marshal to JSON
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	// Write to file
	if err := os.WriteFile(output, data, 0644); err != nil {
		return fmt.Errorf("write output file: %w", err)
	}

	fmt.Fprintf(os.Stderr, "✅ Queue configuration created: %s\n\n", output)
	fmt.Fprintf(os.Stderr, "To launch this queue:\n")
	fmt.Fprintf(os.Stderr, "  spawn launch --batch-queue %s --instance-type <type> --region <region>\n\n", output)

	return nil
}
