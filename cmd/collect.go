package cmd

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spf13/cobra"
)

var (
	collectSweepID    string
	collectOutputFile string
	collectFormat     string
	collectS3Prefix   string
	collectMetric     string
	collectBestN      int
	collectRegions    string
)

var collectCmd = &cobra.Command{
	Use:   "collect-results",
	Short: "Collect and aggregate results from parameter sweep",
	Long: `Collect results from all instances in a parameter sweep.

This command downloads result files from S3 (uploaded by sweep instances),
aggregates them into a single output file, and optionally identifies the
best performing parameters based on a metric.

Result File Convention:
Instances should upload results to: s3://spawn-results-<account>-<region>/sweeps/<sweep-id>/<index>/results.json

The result file should be a JSON object with metrics, e.g.:
{
  "accuracy": 0.95,
  "loss": 0.12,
  "duration": 120.5,
  "params": {...}
}

Examples:
  # Collect all results to JSON
  spawn collect-results --sweep-id sweep-123 --output results.json

  # Collect to CSV format
  spawn collect-results --sweep-id sweep-123 --output results.csv --format csv

  # Find top 5 runs by accuracy (descending)
  spawn collect-results --sweep-id sweep-123 --metric accuracy --best 5

  # Custom S3 prefix (if instances uploaded to different location)
  spawn collect-results --sweep-id sweep-123 --s3-prefix s3://my-bucket/results/
`,
	RunE: runCollectResults,
}

func init() {
	rootCmd.AddCommand(collectCmd)

	collectCmd.Flags().StringVar(&collectSweepID, "sweep-id", "", "Sweep ID to collect results from (required)")
	collectCmd.Flags().StringVarP(&collectOutputFile, "output", "o", "results.json", "Output file path")
	collectCmd.Flags().StringVar(&collectFormat, "format", "json", "Output format: json, csv, jsonl")
	collectCmd.Flags().StringVar(&collectS3Prefix, "s3-prefix", "", "Custom S3 prefix for results (default: auto-detect)")
	collectCmd.Flags().StringVar(&collectMetric, "metric", "", "Metric to use for ranking results (e.g., accuracy, loss)")
	collectCmd.Flags().IntVar(&collectBestN, "best", 0, "Show only top N results by metric (0 = all)")
	collectCmd.Flags().StringVar(&collectRegions, "regions", "", "Comma-separated list of regions to collect from (default: all)")

	_ = collectCmd.MarkFlagRequired("sweep-id")
}

// SweepResult represents a single result from a sweep instance
type SweepResult struct {
	SweepID      string                 `json:"sweep_id"`
	SweepIndex   int                    `json:"sweep_index"`
	InstanceID   string                 `json:"instance_id"`
	Region       string                 `json:"region,omitempty"`
	Parameters   map[string]interface{} `json:"parameters"`
	Metrics      map[string]interface{} `json:"metrics"`
	DownloadedAt time.Time              `json:"downloaded_at"`
}

func runCollectResults(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	fmt.Printf("📊 Collecting results for sweep: %s\n", collectSweepID)

	// Load AWS config
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}

	// Get sweep record from DynamoDB
	fmt.Println("Fetching sweep metadata from DynamoDB...")
	sweepRecord, err := getSweepRecord(ctx, cfg, collectSweepID)
	if err != nil {
		return fmt.Errorf("failed to get sweep record: %w", err)
	}

	if sweepRecord == nil {
		return fmt.Errorf("sweep not found: %s", collectSweepID)
	}

	fmt.Printf("Sweep: %s (Status: %s)\n", sweepRecord.SweepName, sweepRecord.Status)
	fmt.Printf("Total parameters: %d\n", sweepRecord.TotalParams)
	fmt.Printf("Launched: %d, Failed: %d\n", sweepRecord.Launched, sweepRecord.Failed)

	// Get regions for sweep
	regions := getRegionsForSweep(sweepRecord)

	// Apply region filter if specified
	if collectRegions != "" {
		filterSet := make(map[string]bool)
		for _, r := range strings.Split(collectRegions, ",") {
			filterSet[strings.TrimSpace(r)] = true
		}

		filtered := []string{}
		for _, r := range regions {
			if filterSet[r] {
				filtered = append(filtered, r)
			}
		}
		regions = filtered
		fmt.Printf("Filtering to regions: %v\n", regions)
	}

	if len(regions) == 0 {
		return fmt.Errorf("no regions to collect from")
	}

	// Collect results from all regions (or custom S3 prefix if specified)
	var results []SweepResult
	if collectS3Prefix != "" {
		// Custom prefix overrides region-based collection
		fmt.Printf("Collecting results from: %s\n", collectS3Prefix)
		results, err = downloadSweepResults(ctx, cfg, sweepRecord, collectS3Prefix)
		if err != nil {
			return fmt.Errorf("failed to download results: %w", err)
		}
	} else {
		// Collect from all regional S3 buckets concurrently
		results, err = collectFromMultipleRegions(ctx, cfg, sweepRecord, regions, collectSweepID)
		if err != nil {
			return fmt.Errorf("failed to collect results: %w", err)
		}
	}

	if len(results) == 0 {
		fmt.Println("⚠️  No results found. Make sure instances uploaded results to S3.")
		return nil
	}

	fmt.Printf("✅ Downloaded %d result files\n", len(results))

	// Sort by metric if specified
	if collectMetric != "" {
		sortResultsByMetric(results, collectMetric)
		fmt.Printf("Sorted by metric: %s (descending)\n", collectMetric)

		// Filter to top N if requested
		if collectBestN > 0 && len(results) > collectBestN {
			results = results[:collectBestN]
			fmt.Printf("Keeping top %d results\n", collectBestN)
		}
	}

	// Write results to output file
	fmt.Printf("Writing results to: %s\n", collectOutputFile)
	if err := writeResults(results, collectOutputFile, collectFormat); err != nil {
		return fmt.Errorf("failed to write results: %w", err)
	}

	fmt.Println("✅ Results collection complete")

	// Print summary of best results
	if collectMetric != "" && len(results) > 0 {
		fmt.Println("\n🏆 Top Results:")
		for i, result := range results {
			if i >= 5 {
				break // Show top 5 only
			}
			metricValue := result.Metrics[collectMetric]
			fmt.Printf("  %d. Index %d: %s = %v\n", i+1, result.SweepIndex, collectMetric, metricValue)
		}
	}

	return nil
}

// getSweepRecord retrieves sweep metadata from DynamoDB
func getSweepRecord(ctx context.Context, cfg aws.Config, sweepID string) (*SweepRecord, error) {
	client := dynamodb.NewFromConfig(cfg)

	result, err := client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String("spawn-sweep-orchestration"),
		Key: map[string]types.AttributeValue{
			"sweep_id": &types.AttributeValueMemberS{Value: sweepID},
		},
	})

	if err != nil {
		return nil, err
	}

	if result.Item == nil {
		return nil, nil
	}

	// Parse sweep record (simplified - only fields we need)
	var record SweepRecord
	record.SweepID = sweepID

	if v, ok := result.Item["sweep_name"]; ok {
		if s, ok := v.(*types.AttributeValueMemberS); ok {
			record.SweepName = s.Value
		}
	}

	if v, ok := result.Item["status"]; ok {
		if s, ok := v.(*types.AttributeValueMemberS); ok {
			record.Status = s.Value
		}
	}

	if v, ok := result.Item["account_id"]; ok {
		if s, ok := v.(*types.AttributeValueMemberS); ok {
			record.AccountID = s.Value
		}
	}

	if v, ok := result.Item["region"]; ok {
		if s, ok := v.(*types.AttributeValueMemberS); ok {
			record.Region = s.Value
		}
	}

	if v, ok := result.Item["total_params"]; ok {
		if n, ok := v.(*types.AttributeValueMemberN); ok {
			_, _ = fmt.Sscanf(n.Value, "%d", &record.TotalParams)
		}
	}

	if v, ok := result.Item["launched"]; ok {
		if n, ok := v.(*types.AttributeValueMemberN); ok {
			_, _ = fmt.Sscanf(n.Value, "%d", &record.Launched)
		}
	}

	if v, ok := result.Item["failed"]; ok {
		if n, ok := v.(*types.AttributeValueMemberN); ok {
			_, _ = fmt.Sscanf(n.Value, "%d", &record.Failed)
		}
	}

	if v, ok := result.Item["multi_region"]; ok {
		if b, ok := v.(*types.AttributeValueMemberBOOL); ok {
			record.MultiRegion = b.Value
		}
	}

	// Parse region_status if present
	if v, ok := result.Item["region_status"]; ok {
		if m, ok := v.(*types.AttributeValueMemberM); ok {
			record.RegionStatus = make(map[string]*RegionProgress)
			for region := range m.Value {
				record.RegionStatus[region] = &RegionProgress{}
			}
		}
	}

	return &record, nil
}

// getRegionsForSweep extracts list of regions from sweep record
func getRegionsForSweep(sweepRecord *SweepRecord) []string {
	if !sweepRecord.MultiRegion || len(sweepRecord.RegionStatus) == 0 {
		return []string{sweepRecord.Region}
	}

	// Extract regions from RegionStatus map
	regions := make([]string, 0, len(sweepRecord.RegionStatus))
	for region := range sweepRecord.RegionStatus {
		regions = append(regions, region)
	}

	sort.Strings(regions) // Deterministic ordering
	return regions
}

// collectFromMultipleRegions collects results from multiple regional S3 buckets concurrently
func collectFromMultipleRegions(ctx context.Context, cfg aws.Config, sweepRecord *SweepRecord, regions []string, sweepID string) ([]SweepResult, error) {
	type regionResult struct {
		region  string
		results []SweepResult
		err     error
	}

	resultsChan := make(chan regionResult, len(regions))

	// Launch goroutines to collect from each region
	for _, region := range regions {
		go func(r string) {
			s3Prefix := fmt.Sprintf("s3://spawn-results-%s-%s/sweeps/%s/",
				sweepRecord.AccountID, r, sweepID)

			fmt.Printf("Collecting from %s...\n", r)

			regionResults, err := downloadSweepResults(ctx, cfg, sweepRecord, s3Prefix)
			if err != nil {
				fmt.Fprintf(os.Stderr, "⚠️  Warning: Failed to collect from %s: %v\n", r, err)
				resultsChan <- regionResult{region: r, results: nil, err: err}
				return
			}

			// Mark results with region
			for i := range regionResults {
				regionResults[i].Region = r
			}

			fmt.Printf("✓ Collected %d results from %s\n", len(regionResults), r)
			resultsChan <- regionResult{region: r, results: regionResults, err: nil}
		}(region)
	}

	// Collect results from all regions
	allResults := []SweepResult{}
	for i := 0; i < len(regions); i++ {
		rr := <-resultsChan
		if rr.err == nil && rr.results != nil {
			allResults = append(allResults, rr.results...)
		}
	}

	return allResults, nil
}

// downloadSweepResults downloads result files from S3 for all sweep instances
func downloadSweepResults(ctx context.Context, cfg aws.Config, sweep *SweepRecord, s3Prefix string) ([]SweepResult, error) {
	// Parse S3 prefix: s3://bucket/prefix/
	s3Prefix = strings.TrimPrefix(s3Prefix, "s3://")
	parts := strings.SplitN(s3Prefix, "/", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid S3 prefix: %s", s3Prefix)
	}

	bucket := parts[0]
	prefix := parts[1]

	client := s3.NewFromConfig(cfg)
	results := []SweepResult{}

	// List all objects under prefix
	listResult, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(prefix),
	})

	if err != nil {
		return nil, fmt.Errorf("failed to list S3 objects: %w", err)
	}

	if len(listResult.Contents) == 0 {
		return results, nil
	}

	fmt.Printf("Found %d files in S3\n", len(listResult.Contents))

	// Download each result file
	for _, obj := range listResult.Contents {
		key := *obj.Key

		// Parse index from key (e.g., sweeps/<sweep-id>/0/results.json -> index 0)
		parts := strings.Split(key, "/")
		if len(parts) < 3 {
			continue
		}

		var index int
		if _, err := fmt.Sscanf(parts[len(parts)-2], "%d", &index); err != nil {
			continue // Not a valid index directory
		}

		// Download file
		getResult, err := client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
		})

		if err != nil {
			fmt.Printf("⚠️  Failed to download %s: %v\n", key, err)
			continue
		}

		// Parse JSON result
		var resultData map[string]interface{}
		if err := json.NewDecoder(getResult.Body).Decode(&resultData); err != nil {
			fmt.Printf("⚠️  Failed to parse %s: %v\n", key, err)
			_ = getResult.Body.Close()
			continue
		}
		_ = getResult.Body.Close()

		// Extract parameters and metrics
		params := make(map[string]interface{})
		metrics := make(map[string]interface{})

		for k, v := range resultData {
			if k == "parameters" || k == "params" {
				if p, ok := v.(map[string]interface{}); ok {
					params = p
				}
			} else {
				// Everything else is a metric
				metrics[k] = v
			}
		}

		result := SweepResult{
			SweepID:      sweep.SweepID,
			SweepIndex:   index,
			Region:       sweep.Region,
			Parameters:   params,
			Metrics:      metrics,
			DownloadedAt: time.Now(),
		}

		results = append(results, result)
	}

	return results, nil
}

// sortResultsByMetric sorts results by a metric in descending order (higher is better)
func sortResultsByMetric(results []SweepResult, metric string) {
	sort.Slice(results, func(i, j int) bool {
		vi := results[i].Metrics[metric]
		vj := results[j].Metrics[metric]

		// Try to compare as float64
		fi, okI := toFloat64(vi)
		fj, okJ := toFloat64(vj)

		if okI && okJ {
			return fi > fj // Descending order
		}

		// Fallback: compare as strings
		return fmt.Sprintf("%v", vi) > fmt.Sprintf("%v", vj)
	})
}

// toFloat64 converts interface{} to float64 if possible
func toFloat64(v interface{}) (float64, bool) {
	switch val := v.(type) {
	case float64:
		return val, true
	case float32:
		return float64(val), true
	case int:
		return float64(val), true
	case int64:
		return float64(val), true
	case int32:
		return float64(val), true
	default:
		return 0, false
	}
}

// writeResults writes aggregated results to output file
func writeResults(results []SweepResult, outputFile, format string) error {
	// Create output directory if needed
	dir := filepath.Dir(outputFile)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create output directory: %w", err)
		}
	}

	switch format {
	case "json":
		return writeJSON(results, outputFile)
	case "jsonl":
		return writeJSONLines(results, outputFile)
	case "csv":
		return writeCSV(results, outputFile)
	default:
		return fmt.Errorf("unsupported format: %s (supported: json, jsonl, csv)", format)
	}
}

func writeJSON(results []SweepResult, outputFile string) error {
	f, err := os.Create(outputFile)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	encoder := json.NewEncoder(f)
	encoder.SetIndent("", "  ")
	return encoder.Encode(results)
}

func writeJSONLines(results []SweepResult, outputFile string) error {
	f, err := os.Create(outputFile)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	encoder := json.NewEncoder(f)
	for _, result := range results {
		if err := encoder.Encode(result); err != nil {
			return err
		}
	}
	return nil
}

func writeCSV(results []SweepResult, outputFile string) error {
	f, err := os.Create(outputFile)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	writer := csv.NewWriter(f)
	defer writer.Flush()

	if len(results) == 0 {
		return nil
	}

	// Collect all unique parameter and metric keys
	paramKeys := make(map[string]bool)
	metricKeys := make(map[string]bool)

	for _, result := range results {
		for k := range result.Parameters {
			paramKeys[k] = true
		}
		for k := range result.Metrics {
			metricKeys[k] = true
		}
	}

	// Sort keys for consistent column order
	var paramKeysSorted []string
	for k := range paramKeys {
		paramKeysSorted = append(paramKeysSorted, k)
	}
	sort.Strings(paramKeysSorted)

	var metricKeysSorted []string
	for k := range metricKeys {
		metricKeysSorted = append(metricKeysSorted, k)
	}
	sort.Strings(metricKeysSorted)

	// Write header
	header := []string{"sweep_id", "sweep_index", "instance_id", "region"}
	for _, k := range paramKeysSorted {
		header = append(header, "param_"+k)
	}
	header = append(header, metricKeysSorted...)
	if err := writer.Write(header); err != nil {
		return err
	}

	// Write rows
	for _, result := range results {
		row := []string{
			result.SweepID,
			fmt.Sprintf("%d", result.SweepIndex),
			result.InstanceID,
			result.Region,
		}

		for _, k := range paramKeysSorted {
			row = append(row, fmt.Sprintf("%v", result.Parameters[k]))
		}

		for _, k := range metricKeysSorted {
			row = append(row, fmt.Sprintf("%v", result.Metrics[k]))
		}

		if err := writer.Write(row); err != nil {
			return err
		}
	}

	return nil
}

// SweepRecord is a simplified version for result collection
type SweepRecord struct {
	SweepID      string
	SweepName    string
	Status       string
	AccountID    string
	Region       string
	TotalParams  int
	Launched     int
	Failed       int
	MultiRegion  bool
	RegionStatus map[string]*RegionProgress
}

// RegionProgress tracks per-region sweep progress
type RegionProgress struct {
	Launched     int
	Failed       int
	ActiveCount  int
	NextToLaunch []int
}
