package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	lambdasvc "github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/spf13/cobra"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
	"github.com/spore-host/spawn/pkg/pipeline"
)

var pipelineCmd = &cobra.Command{
	Use:   "pipeline",
	Short: "Manage multi-stage pipelines",
	Long: `Manage multi-stage pipelines with DAG dependencies.

Pipelines allow you to orchestrate complex workflows where each stage runs on
separate instances with different instance types. Stages can depend on each other,
forming a directed acyclic graph (DAG).

Data can be passed between stages via:
- S3 (batch mode): Stage outputs uploaded to S3, downloaded by next stage
- Network streaming (real-time): Direct TCP/gRPC connections between stages`,
}

var validatePipelineCmd = &cobra.Command{
	Use:   "validate <file>",
	Short: "Validate a pipeline definition",
	Long: `Validate a pipeline definition file.

Checks:
- JSON syntax and structure
- Required fields present
- Stage dependencies valid (no circular dependencies)
- Instance types, regions, and other configuration valid`,
	Args: cobra.ExactArgs(1),
	RunE: runValidatePipeline,
}

var graphPipelineCmd = &cobra.Command{
	Use:   "graph <file>",
	Short: "Display pipeline DAG as ASCII art",
	Long: `Display the pipeline dependency graph as ASCII art.

Shows:
- Stage names and instance types
- Dependencies between stages
- Fan-out and fan-in patterns
- Data passing modes (S3 or streaming)`,
	Args: cobra.ExactArgs(1),
	RunE: runGraphPipeline,
}

var launchPipelineCmd = &cobra.Command{
	Use:   "launch <file>",
	Short: "Launch a pipeline",
	Long: `Launch a multi-stage pipeline.

The pipeline definition will be uploaded to S3 and a Lambda orchestrator
will be invoked to manage the pipeline execution.`,
	Args: cobra.ExactArgs(1),
	RunE: runLaunchPipeline,
}

var statusPipelineCmd = &cobra.Command{
	Use:   "status <pipeline-id>",
	Short: "Show pipeline status",
	Long: `Show the current status of a running or completed pipeline.

Displays:
- Overall pipeline status
- Per-stage progress
- Instance information
- Cost tracking`,
	Args: cobra.ExactArgs(1),
	RunE: runStatusPipeline,
}

var collectPipelineCmd = &cobra.Command{
	Use:   "collect <pipeline-id>",
	Short: "Download pipeline results",
	Long: `Download all results from a completed pipeline.

Downloads outputs from all stages to a local directory.`,
	Args: cobra.ExactArgs(1),
	RunE: runCollectPipeline,
}

var listPipelineCmd = &cobra.Command{
	Use:   "list",
	Short: "List all pipelines",
	Long: `List all pipelines for the current user.

Shows pipeline ID, name, status, and cost.`,
	Args: cobra.NoArgs,
	RunE: runListPipeline,
}

var cancelPipelineCmd = &cobra.Command{
	Use:   "cancel <pipeline-id>",
	Short: "Cancel a running pipeline",
	Long: `Cancel a running pipeline and terminate all instances.

Sets the cancellation flag in DynamoDB. The orchestrator Lambda will
terminate all running instances and mark the pipeline as CANCELLED.`,
	Args: cobra.ExactArgs(1),
	RunE: runCancelPipeline,
}

var (
	flagSimpleGraph  bool
	flagGraphStats   bool
	flagJSONOutput   bool // deprecated: use --output json
	flagDetached     bool
	flagWait         bool
	flagRegion       string
	flagOutputDir    string
	flagStage        string
	flagStatusFilter string
	flagCancelYes    bool
)

func init() {
	rootCmd.AddCommand(pipelineCmd)
	pipelineCmd.AddCommand(validatePipelineCmd)
	pipelineCmd.AddCommand(graphPipelineCmd)
	pipelineCmd.AddCommand(launchPipelineCmd)
	pipelineCmd.AddCommand(statusPipelineCmd)
	pipelineCmd.AddCommand(collectPipelineCmd)
	pipelineCmd.AddCommand(listPipelineCmd)
	pipelineCmd.AddCommand(cancelPipelineCmd)
	cancelPipelineCmd.Flags().BoolVarP(&flagCancelYes, "yes", "y", false, "Skip the confirmation prompt")

	// Graph command flags
	graphPipelineCmd.Flags().BoolVar(&flagSimpleGraph, "simple", false, "Show simplified graph")
	graphPipelineCmd.Flags().BoolVar(&flagGraphStats, "stats", false, "Show graph statistics")
	graphPipelineCmd.Flags().BoolVar(&flagJSONOutput, "json", false, "Output as JSON")
	_ = graphPipelineCmd.Flags().MarkDeprecated("json", "use --output json instead")

	// Launch command flags
	launchPipelineCmd.Flags().BoolVar(&flagDetached, "detached", false, "Launch and return immediately")
	launchPipelineCmd.Flags().BoolVar(&flagWait, "wait", false, "Wait for pipeline to complete")
	launchPipelineCmd.Flags().StringVar(&flagRegion, "region", "", "AWS region (default: from AWS config)")

	// Collect command flags
	collectPipelineCmd.Flags().StringVar(&flagOutputDir, "output", "./results", "Output directory for downloaded files")
	collectPipelineCmd.Flags().StringVar(&flagStage, "stage", "", "Download results from specific stage only")

	// List command flags
	listPipelineCmd.Flags().StringVar(&flagStatusFilter, "status", "", "Filter by status (INITIALIZING, RUNNING, COMPLETED, FAILED, CANCELLED)")
	listPipelineCmd.Flags().BoolVar(&flagJSONOutput, "json", false, "Output as JSON")
	_ = listPipelineCmd.Flags().MarkDeprecated("json", "use --output json instead")
}

func runValidatePipeline(cmd *cobra.Command, args []string) error {
	file := args[0]

	// Load pipeline
	p, err := pipeline.LoadPipelineFromFile(file)
	if err != nil {
		return fmt.Errorf("load pipeline: %w", err)
	}

	// Validate
	if err := p.Validate(); err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}

	_, _ = fmt.Fprintf(os.Stdout, "✓ Pipeline is valid\n\n")
	_, _ = fmt.Fprintf(os.Stdout, "Pipeline: %s\n", p.PipelineName)
	_, _ = fmt.Fprintf(os.Stdout, "ID: %s\n", p.PipelineID)
	_, _ = fmt.Fprintf(os.Stdout, "Stages: %d\n", len(p.Stages))
	_, _ = fmt.Fprintf(os.Stdout, "S3 Bucket: %s\n", p.S3Bucket)

	// Show topological order
	order, err := p.GetTopologicalOrder()
	if err != nil {
		return fmt.Errorf("get topological order: %w", err)
	}
	_, _ = fmt.Fprintf(os.Stdout, "\nExecution order:\n")
	for i, stageID := range order {
		_, _ = fmt.Fprintf(os.Stdout, "  %d. %s\n", i+1, stageID)
	}

	// Show features
	_, _ = fmt.Fprintf(os.Stdout, "\nFeatures:\n")
	if p.HasStreamingStages() {
		_, _ = fmt.Fprintf(os.Stdout, "  • Network streaming enabled\n")
	}
	if p.HasEFAStages() {
		_, _ = fmt.Fprintf(os.Stdout, "  • EFA (Elastic Fabric Adapter) enabled\n")
	}
	if p.OnFailure == "stop" {
		_, _ = fmt.Fprintf(os.Stdout, "  • Stops on first failure\n")
	} else {
		_, _ = fmt.Fprintf(os.Stdout, "  • Continues on failure\n")
	}
	if p.MaxCostUSD != nil {
		_, _ = fmt.Fprintf(os.Stdout, "  • Budget limit: $%.2f\n", *p.MaxCostUSD)
	}

	return nil
}

func runGraphPipeline(cmd *cobra.Command, args []string) error {
	file := args[0]

	// Load pipeline
	p, err := pipeline.LoadPipelineFromFile(file)
	if err != nil {
		return fmt.Errorf("load pipeline: %w", err)
	}

	// Validate
	if err := p.Validate(); err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}

	// JSON output
	if flagJSONOutput || getOutputFormat() == "json" {
		stats := p.GetGraphStats()
		data, err := json.MarshalIndent(stats, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal JSON: %w", err)
		}
		_, _ = fmt.Fprintf(os.Stdout, "%s\n", data)
		return nil
	}

	// Show statistics
	if flagGraphStats {
		stats := p.GetGraphStats()
		_, _ = fmt.Fprintf(os.Stdout, "Pipeline Statistics\n")
		_, _ = fmt.Fprintf(os.Stdout, "═══════════════════\n\n")
		_, _ = fmt.Fprintf(os.Stdout, "Total stages:     %d\n", stats["total_stages"])
		_, _ = fmt.Fprintf(os.Stdout, "Total instances:  %d\n", stats["total_instances"])
		_, _ = fmt.Fprintf(os.Stdout, "Max fan-out:      %d\n", stats["max_fan_out"])
		_, _ = fmt.Fprintf(os.Stdout, "Max fan-in:       %d\n", stats["max_fan_in"])
		_, _ = fmt.Fprintf(os.Stdout, "Has streaming:    %v\n", stats["has_streaming"])
		_, _ = fmt.Fprintf(os.Stdout, "Has EFA:          %v\n\n", stats["has_efa"])
		return nil
	}

	// Render graph
	var graph string
	if flagSimpleGraph {
		graph, err = p.RenderSimpleGraph()
	} else {
		graph, err = p.RenderGraph()
	}
	if err != nil {
		return fmt.Errorf("render graph: %w", err)
	}

	_, _ = fmt.Fprintf(os.Stdout, "%s\n", graph)
	return nil
}

func runLaunchPipeline(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	file := args[0]

	// Load and validate pipeline
	p, err := pipeline.LoadPipelineFromFile(file)
	if err != nil {
		return fmt.Errorf("load pipeline: %w", err)
	}

	if err := p.Validate(); err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}

	fmt.Fprintf(os.Stderr, "🚀 Launching pipeline: %s\n", p.PipelineName)
	fmt.Fprintf(os.Stderr, "   Pipeline ID: %s\n", p.PipelineID)
	fmt.Fprintf(os.Stderr, "   Stages: %d\n\n", len(p.Stages))

	// Load AWS config
	region := flagRegion
	if region == "" {
		region = p.Stages[0].Region // Use first stage's region
		if region == "" {
			region = "us-east-1"
		}
	}

	cfg, err := spawnconfig.LoadInfraAWSConfig(ctx, region)
	if err != nil {
		return fmt.Errorf("load AWS config: %w", err)
	}

	// Get user account ID
	stsClient := sts.NewFromConfig(cfg)
	identity, err := stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return fmt.Errorf("get caller identity: %w", err)
	}
	userAccountID := *identity.Account

	// Step 1: Upload pipeline definition to S3
	fmt.Fprintf(os.Stderr, "📤 Uploading pipeline definition to S3...\n")
	s3Client := s3.NewFromConfig(cfg)
	bucketName := fmt.Sprintf("spawn-pipelines-%s", region)
	s3Key := fmt.Sprintf("pipelines/%s/config.json", p.PipelineID)

	pipelineJSON, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal pipeline: %w", err)
	}

	_, err = s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(bucketName),
		Key:         aws.String(s3Key),
		Body:        bytes.NewReader(pipelineJSON),
		ContentType: aws.String("application/json"),
	})
	if err != nil {
		return fmt.Errorf("upload to S3: %w", err)
	}
	fmt.Fprintf(os.Stderr, "✓ Uploaded to s3://%s/%s\n\n", bucketName, s3Key)

	// Step 2: Create DynamoDB record
	fmt.Fprintf(os.Stderr, "💾 Creating pipeline orchestration record...\n")
	dynamoClient := dynamodb.NewFromConfig(cfg)
	tableName := "spawn-pipeline-orchestration"

	// Build initial pipeline state (use map to control DynamoDB attribute names)
	now := time.Now().UTC()
	pipelineState := map[string]interface{}{
		"pipeline_id":      p.PipelineID,
		"pipeline_name":    p.PipelineName,
		"user_id":          userAccountID,
		"created_at":       now,
		"updated_at":       now,
		"status":           "INITIALIZING",
		"cancel_requested": false,
		"s3_config_key":    fmt.Sprintf("s3://%s/%s", bucketName, s3Key),
		"s3_bucket":        p.S3Bucket,
		"s3_prefix":        p.S3Prefix,
		"result_s3_bucket": p.ResultS3Bucket,
		"result_s3_prefix": p.ResultS3Prefix,
		"on_failure":       p.OnFailure,
		"current_cost_usd": 0.0,
		"total_stages":     len(p.Stages),
		"completed_stages": 0,
		"failed_stages":    0,
		"stages":           []interface{}{}, // Empty array for stages
	}
	if p.MaxCostUSD != nil {
		pipelineState["max_cost_usd"] = *p.MaxCostUSD
	}

	item, err := attributevalue.MarshalMap(pipelineState)
	if err != nil {
		return fmt.Errorf("marshal DynamoDB item: %w", err)
	}

	_, err = dynamoClient.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(tableName),
		Item:      item,
	})
	if err != nil {
		return fmt.Errorf("create DynamoDB record: %w", err)
	}
	fmt.Fprintf(os.Stderr, "✓ Created orchestration record\n\n")

	// Step 3: Invoke Lambda orchestrator
	fmt.Fprintf(os.Stderr, "⚡ Invoking pipeline orchestrator Lambda...\n")
	lambdaClient := lambdasvc.NewFromConfig(cfg)
	functionName := "spawn-pipeline-orchestrator"

	payload := map[string]string{
		"pipeline_id": p.PipelineID,
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal Lambda payload: %w", err)
	}

	_, err = lambdaClient.Invoke(ctx, &lambdasvc.InvokeInput{
		FunctionName:   aws.String(functionName),
		InvocationType: lambdatypes.InvocationTypeEvent, // Asynchronous
		Payload:        payloadJSON,
	})
	if err != nil {
		return fmt.Errorf("invoke Lambda: %w", err)
	}
	fmt.Fprintf(os.Stderr, "✓ Lambda orchestrator invoked\n\n")

	fmt.Fprintf(os.Stderr, "✅ Pipeline launched successfully!\n\n")
	fmt.Fprintf(os.Stderr, "Pipeline ID: %s\n\n", p.PipelineID)
	fmt.Fprintf(os.Stderr, "To check status:\n")
	fmt.Fprintf(os.Stderr, "  spawn pipeline status %s\n\n", p.PipelineID)
	fmt.Fprintf(os.Stderr, "To collect results:\n")
	fmt.Fprintf(os.Stderr, "  spawn pipeline collect %s --output ./results/\n", p.PipelineID)

	// Output just the pipeline ID to stdout for scripting
	_, _ = fmt.Fprintf(os.Stdout, "%s\n", p.PipelineID)

	return nil
}

func runStatusPipeline(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	pipelineID := args[0]

	// Load AWS config
	cfg, err := spawnconfig.LoadInfraAWSConfig(ctx, "")
	if err != nil {
		return fmt.Errorf("load AWS config: %w", err)
	}

	// Query DynamoDB for pipeline state
	dynamoClient := dynamodb.NewFromConfig(cfg)
	tableName := "spawn-pipeline-orchestration"

	result, err := dynamoClient.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(tableName),
		Key: map[string]types.AttributeValue{
			"pipeline_id": &types.AttributeValueMemberS{Value: pipelineID},
		},
	})
	if err != nil {
		return fmt.Errorf("query DynamoDB: %w", err)
	}

	if result.Item == nil {
		return fmt.Errorf("pipeline not found: %s", pipelineID)
	}

	// Parse pipeline state
	var state pipeline.PipelineState
	err = attributevalue.UnmarshalMap(result.Item, &state)
	if err != nil {
		return fmt.Errorf("unmarshal pipeline state: %w", err)
	}

	// Display status
	_, _ = fmt.Fprintf(os.Stdout, "Pipeline: %s\n", state.PipelineName)
	_, _ = fmt.Fprintf(os.Stdout, "ID: %s\n", pipelineID)
	_, _ = fmt.Fprintf(os.Stdout, "Status: %s\n", state.Status)
	_, _ = fmt.Fprintf(os.Stdout, "Created: %s\n", state.CreatedAt.Format(time.RFC3339))
	_, _ = fmt.Fprintf(os.Stdout, "Updated: %s\n", state.UpdatedAt.Format(time.RFC3339))
	_, _ = fmt.Fprintf(os.Stdout, "\n")

	// Progress
	_, _ = fmt.Fprintf(os.Stdout, "Progress: %d/%d stages completed", state.CompletedStages, state.TotalStages)
	if state.FailedStages > 0 {
		_, _ = fmt.Fprintf(os.Stdout, " (%d failed)", state.FailedStages)
	}
	_, _ = fmt.Fprintf(os.Stdout, "\n")

	// Cost
	_, _ = fmt.Fprintf(os.Stdout, "Cost: $%.2f", state.CurrentCostUSD)
	if state.MaxCostUSD != nil && *state.MaxCostUSD > 0 {
		_, _ = fmt.Fprintf(os.Stdout, " / $%.2f", *state.MaxCostUSD)
	}
	_, _ = fmt.Fprintf(os.Stdout, "\n\n")

	// Stages table
	if len(state.Stages) > 0 {
		_, _ = fmt.Fprintf(os.Stdout, "STAGE                STATUS       INSTANCES  COST      DURATION\n")
		_, _ = fmt.Fprintf(os.Stdout, "─────────────────────────────────────────────────────────────────\n")
		for _, stage := range state.Stages {
			// Calculate duration
			duration := ""
			if stage.LaunchedAt != nil {
				if stage.CompletedAt != nil {
					duration = formatDurationFromTime(stage.CompletedAt.Sub(*stage.LaunchedAt))
				} else {
					duration = formatDurationFromTime(time.Since(*stage.LaunchedAt))
				}
			}

			instanceStr := "-"
			if len(stage.Instances) > 0 {
				instanceStr = fmt.Sprintf("%d", len(stage.Instances))
			}

			costStr := "-"
			if stage.StageCostUSD > 0 {
				costStr = fmt.Sprintf("$%.2f", stage.StageCostUSD)
			}

			_, _ = fmt.Fprintf(os.Stdout, "%-20s %-12s %-10s %-9s %s\n",
				truncate(stage.StageID, 20), stage.Status, instanceStr, costStr, duration)
		}
	}

	return nil
}

// Helper functions for extracting fields
func getStringField(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func getFloatField(m map[string]interface{}, key string) float64 {
	if v, ok := m[key].(float64); ok {
		return v
	}
	return 0.0
}

func formatDurationFromTime(duration time.Duration) string {
	if duration < time.Minute {
		return fmt.Sprintf("%ds", int(duration.Seconds()))
	} else if duration < time.Hour {
		return fmt.Sprintf("%dm", int(duration.Minutes()))
	}
	return fmt.Sprintf("%.1fh", duration.Hours())
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

func runCollectPipeline(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	pipelineID := args[0]

	fmt.Fprintf(os.Stderr, "📦 Collecting results for pipeline: %s\n", pipelineID)
	fmt.Fprintf(os.Stderr, "   Output directory: %s\n\n", flagOutputDir)

	// Load AWS config
	cfg, err := spawnconfig.LoadInfraAWSConfig(ctx, "")
	if err != nil {
		return fmt.Errorf("load AWS config: %w", err)
	}

	// Query DynamoDB for pipeline state
	dynamoClient := dynamodb.NewFromConfig(cfg)
	tableName := "spawn-pipeline-orchestration"

	result, err := dynamoClient.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(tableName),
		Key: map[string]types.AttributeValue{
			"pipeline_id": &types.AttributeValueMemberS{Value: pipelineID},
		},
	})
	if err != nil {
		return fmt.Errorf("query DynamoDB: %w", err)
	}

	if result.Item == nil {
		return fmt.Errorf("pipeline not found: %s", pipelineID)
	}

	// Parse pipeline state
	var state map[string]interface{}
	err = attributevalue.UnmarshalMap(result.Item, &state)
	if err != nil {
		return fmt.Errorf("unmarshal pipeline state: %w", err)
	}

	// Get S3 result location
	resultBucket := getStringField(state, "result_s3_bucket")
	resultPrefix := getStringField(state, "result_s3_prefix")
	if resultBucket == "" {
		// Fall back to stage output locations
		resultBucket = getStringField(state, "s3_bucket")
		resultPrefix = fmt.Sprintf("%s/stages", getStringField(state, "s3_prefix"))
	}

	if resultBucket == "" {
		return fmt.Errorf("no result bucket configured for pipeline")
	}

	fmt.Fprintf(os.Stderr, "📍 Source: s3://%s/%s\n\n", resultBucket, resultPrefix)

	// Create output directory
	if err := os.MkdirAll(flagOutputDir, 0755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}

	// Download from S3
	s3Client := s3.NewFromConfig(cfg)

	// List objects
	fmt.Fprintf(os.Stderr, "🔍 Listing objects...\n")
	listOutput, err := s3Client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(resultBucket),
		Prefix: aws.String(resultPrefix),
	})
	if err != nil {
		return fmt.Errorf("list S3 objects: %w", err)
	}

	if len(listOutput.Contents) == 0 {
		fmt.Fprintf(os.Stderr, "⚠️  No results found\n")
		return nil
	}

	fmt.Fprintf(os.Stderr, "📥 Downloading %d objects...\n", len(listOutput.Contents))

	// Download each object
	downloadCount := 0
	for _, obj := range listOutput.Contents {
		// Skip directories (keys ending with /)
		if len(*obj.Key) > 0 && (*obj.Key)[len(*obj.Key)-1] == '/' {
			continue
		}

		// Filter by stage if specified
		if flagStage != "" {
			// Check if object key contains the stage ID
			if !contains(*obj.Key, fmt.Sprintf("/stages/%s/", flagStage)) {
				continue
			}
		}

		// Download to local file
		localPath := fmt.Sprintf("%s/%s", flagOutputDir, *obj.Key)
		if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
			return fmt.Errorf("create directory for %s: %w", localPath, err)
		}

		getOutput, err := s3Client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(resultBucket),
			Key:    obj.Key,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Failed to download %s: %v\n", *obj.Key, err)
			continue
		}

		// Write to file
		file, err := os.Create(localPath)
		if err != nil {
			_ = getOutput.Body.Close()
			return fmt.Errorf("create file %s: %w", localPath, err)
		}

		_, err = io.Copy(file, getOutput.Body)
		_ = file.Close()
		_ = getOutput.Body.Close()
		if err != nil {
			return fmt.Errorf("write file %s: %w", localPath, err)
		}

		downloadCount++
		fmt.Fprintf(os.Stderr, "   ✓ %s\n", *obj.Key)
	}

	fmt.Fprintf(os.Stderr, "\n✅ Downloaded %d files to %s\n", downloadCount, flagOutputDir)

	return nil
}

func runListPipeline(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	// Load AWS config
	cfg, err := spawnconfig.LoadInfraAWSConfig(ctx, "")
	if err != nil {
		return fmt.Errorf("load AWS config: %w", err)
	}

	// Get user account ID for filtering
	stsClient := sts.NewFromConfig(cfg)
	identity, err := stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return fmt.Errorf("get caller identity: %w", err)
	}
	userAccountID := *identity.Account

	// Query DynamoDB for all pipelines
	dynamoClient := dynamodb.NewFromConfig(cfg)
	tableName := "spawn-pipeline-orchestration"

	// Scan for all pipelines (or use GSI if exists)
	scanOutput, err := dynamoClient.Scan(ctx, &dynamodb.ScanInput{
		TableName: aws.String(tableName),
	})
	if err != nil {
		return fmt.Errorf("scan DynamoDB: %w", err)
	}

	// Parse results
	var pipelines []map[string]interface{}
	for _, item := range scanOutput.Items {
		var p map[string]interface{}
		if err := attributevalue.UnmarshalMap(item, &p); err != nil {
			continue
		}

		// Filter by user
		if getStringField(p, "user_id") != userAccountID {
			continue
		}

		// Filter by status if specified
		if flagStatusFilter != "" && getStringField(p, "status") != flagStatusFilter {
			continue
		}

		pipelines = append(pipelines, p)
	}

	if len(pipelines) == 0 {
		_, _ = fmt.Fprintf(os.Stdout, "No pipelines found\n")
		return nil
	}

	// JSON output
	if flagJSONOutput || getOutputFormat() == "json" {
		data, err := json.MarshalIndent(pipelines, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal JSON: %w", err)
		}
		_, _ = fmt.Fprintf(os.Stdout, "%s\n", data)
		return nil
	}

	// Table output
	_, _ = fmt.Fprintf(os.Stdout, "PIPELINE ID                    NAME                          STATUS       COST      CREATED\n")
	_, _ = fmt.Fprintf(os.Stdout, "──────────────────────────────────────────────────────────────────────────────────────────────────\n")
	for _, p := range pipelines {
		pipelineID := getStringField(p, "pipeline_id")
		pipelineName := getStringField(p, "pipeline_name")
		status := getStringField(p, "status")
		cost := getFloatField(p, "current_cost_usd")
		createdAt := getStringField(p, "created_at")

		// Format created time
		createdTime := "-"
		if createdAt != "" {
			if t, err := time.Parse(time.RFC3339, createdAt); err == nil {
				createdTime = t.Format("2006-01-02 15:04")
			}
		}

		costStr := fmt.Sprintf("$%.2f", cost)

		_, _ = fmt.Fprintf(os.Stdout, "%-30s %-29s %-12s %-9s %s\n",
			truncate(pipelineID, 30),
			truncate(pipelineName, 29),
			status,
			costStr,
			createdTime)
	}

	return nil
}

func runCancelPipeline(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	pipelineID := args[0]

	if !confirmYes(flagCancelYes, fmt.Sprintf("Cancel pipeline %s and terminate its instances? This cannot be undone.", pipelineID)) {
		fmt.Fprintln(os.Stderr, "Aborted.")
		return nil
	}

	fmt.Fprintf(os.Stderr, "⚠️  Cancelling pipeline: %s\n", pipelineID)

	// Load AWS config
	cfg, err := spawnconfig.LoadInfraAWSConfig(ctx, "")
	if err != nil {
		return fmt.Errorf("load AWS config: %w", err)
	}

	// Set cancellation flag in DynamoDB
	dynamoClient := dynamodb.NewFromConfig(cfg)
	tableName := "spawn-pipeline-orchestration"

	_, err = dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(tableName),
		Key: map[string]types.AttributeValue{
			"pipeline_id": &types.AttributeValueMemberS{Value: pipelineID},
		},
		UpdateExpression: aws.String("SET cancel_requested = :true, updated_at = :now"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":true": &types.AttributeValueMemberBOOL{Value: true},
			":now":  &types.AttributeValueMemberS{Value: time.Now().UTC().Format(time.RFC3339)},
		},
	})
	if err != nil {
		return fmt.Errorf("update DynamoDB: %w", err)
	}

	fmt.Fprintf(os.Stderr, "✓ Cancellation requested\n\n")
	fmt.Fprintf(os.Stderr, "The orchestrator Lambda will terminate all running instances.\n")
	fmt.Fprintf(os.Stderr, "This may take a few minutes.\n\n")
	fmt.Fprintf(os.Stderr, "To check status:\n")
	fmt.Fprintf(os.Stderr, "  spawn pipeline status %s\n", pipelineID)

	return nil
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 || indexString(s, substr) >= 0)
}

func indexString(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
