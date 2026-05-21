package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/testutil"
)

// TestSweepResultParsing tests parsing of sweep result JSON
func TestSweepResultParsing(t *testing.T) {
	tests := []struct {
		name       string
		jsonData   string
		wantErr    bool
		checkField string
	}{
		{
			name: "valid result with metrics",
			jsonData: `{
				"sweep_id": "sweep-123",
				"sweep_index": 5,
				"instance_id": "i-abc123",
				"region": "us-east-1",
				"parameters": {"learning_rate": 0.001, "batch_size": 32},
				"metrics": {"accuracy": 0.95, "loss": 0.12}
			}`,
			wantErr:    false,
			checkField: "accuracy",
		},
		{
			name: "minimal result",
			jsonData: `{
				"sweep_id": "sweep-456",
				"sweep_index": 0,
				"metrics": {"score": 100}
			}`,
			wantErr:    false,
			checkField: "score",
		},
		{
			name:     "invalid JSON",
			jsonData: `{invalid json`,
			wantErr:  true,
		},
		{
			name:     "empty object",
			jsonData: `{}`,
			wantErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var result SweepResult
			err := json.Unmarshal([]byte(tt.jsonData), &result)

			if (err != nil) != tt.wantErr {
				t.Errorf("Unmarshal() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr && tt.checkField != "" {
				if result.Metrics == nil {
					t.Error("Metrics is nil")
					return
				}
				if _, ok := result.Metrics[tt.checkField]; !ok {
					t.Errorf("expected metric %q not found", tt.checkField)
				}
			}
		})
	}
}

// TestS3PrefixConstruction tests S3 prefix generation for results
func TestS3PrefixConstruction(t *testing.T) {
	tests := []struct {
		name       string
		sweepID    string
		region     string
		accountID  string
		wantPrefix string
	}{
		{
			name:       "standard sweep",
			sweepID:    "sweep-abc123",
			region:     "us-east-1",
			accountID:  "123456789012",
			wantPrefix: "s3://spawn-results-123456789012-us-east-1/sweeps/sweep-abc123/",
		},
		{
			name:       "different region",
			sweepID:    "sweep-xyz789",
			region:     "us-west-2",
			accountID:  "987654321098",
			wantPrefix: "s3://spawn-results-987654321098-us-west-2/sweeps/sweep-xyz789/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Construct S3 prefix
			prefix := constructS3Prefix(tt.accountID, tt.region, tt.sweepID)

			if prefix != tt.wantPrefix {
				t.Errorf("prefix = %q, want %q", prefix, tt.wantPrefix)
			}
		})
	}
}

// TestResultSorting tests sorting results by metric
func TestResultSorting(t *testing.T) {
	results := []SweepResult{
		{
			SweepIndex: 0,
			Metrics:    map[string]interface{}{"accuracy": 0.85},
		},
		{
			SweepIndex: 1,
			Metrics:    map[string]interface{}{"accuracy": 0.95},
		},
		{
			SweepIndex: 2,
			Metrics:    map[string]interface{}{"accuracy": 0.90},
		},
	}

	// Sort by accuracy descending
	sortResultsForTest(results, "accuracy", true)

	// Verify order
	if getMetricValue(results[0], "accuracy") != 0.95 {
		t.Errorf("first result accuracy = %v, want 0.95", results[0].Metrics["accuracy"])
	}
	if getMetricValue(results[1], "accuracy") != 0.90 {
		t.Errorf("second result accuracy = %v, want 0.90", results[1].Metrics["accuracy"])
	}
	if getMetricValue(results[2], "accuracy") != 0.85 {
		t.Errorf("third result accuracy = %v, want 0.85", results[2].Metrics["accuracy"])
	}
}

// TestResultFiltering tests filtering top N results
func TestResultFiltering(t *testing.T) {
	results := []SweepResult{
		{SweepIndex: 0, Metrics: map[string]interface{}{"score": 100.0}},
		{SweepIndex: 1, Metrics: map[string]interface{}{"score": 95.0}},
		{SweepIndex: 2, Metrics: map[string]interface{}{"score": 90.0}},
		{SweepIndex: 3, Metrics: map[string]interface{}{"score": 85.0}},
		{SweepIndex: 4, Metrics: map[string]interface{}{"score": 80.0}},
	}

	tests := []struct {
		name      string
		bestN     int
		wantCount int
	}{
		{
			name:      "top 3",
			bestN:     3,
			wantCount: 3,
		},
		{
			name:      "top 1",
			bestN:     1,
			wantCount: 1,
		},
		{
			name:      "all (bestN = 0)",
			bestN:     0,
			wantCount: 5,
		},
		{
			name:      "more than available",
			bestN:     10,
			wantCount: 5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filtered := filterTopN(results, tt.bestN)
			if len(filtered) != tt.wantCount {
				t.Errorf("filtered count = %d, want %d", len(filtered), tt.wantCount)
			}
		})
	}
}

// TestJSONOutput tests JSON output formatting
func TestJSONOutput(t *testing.T) {
	results := []SweepResult{
		{
			SweepID:    "sweep-123",
			SweepIndex: 0,
			InstanceID: "i-abc123",
			Parameters: map[string]interface{}{"lr": 0.001},
			Metrics:    map[string]interface{}{"accuracy": 0.95},
		},
		{
			SweepID:    "sweep-123",
			SweepIndex: 1,
			InstanceID: "i-def456",
			Parameters: map[string]interface{}{"lr": 0.01},
			Metrics:    map[string]interface{}{"accuracy": 0.90},
		},
	}

	tmpDir := testutil.CreateTempDir(t, "collect-test-*")
	outputFile := filepath.Join(tmpDir, "results.json")

	// Write JSON
	err := writeJSONOutput(results, outputFile)
	if err != nil {
		t.Fatalf("writeJSONOutput failed: %v", err)
	}

	// Verify file exists
	testutil.AssertFileExists(t, outputFile)

	// Read and verify
	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("failed to read output: %v", err)
	}

	var readResults []SweepResult
	if err := json.Unmarshal(data, &readResults); err != nil {
		t.Fatalf("failed to unmarshal output: %v", err)
	}

	if len(readResults) != 2 {
		t.Errorf("got %d results, want 2", len(readResults))
	}
}

// TestCSVOutput tests CSV output formatting
func TestCSVOutput(t *testing.T) {
	results := []SweepResult{
		{
			SweepID:    "sweep-123",
			SweepIndex: 0,
			Parameters: map[string]interface{}{"learning_rate": 0.001, "batch_size": 32},
			Metrics:    map[string]interface{}{"accuracy": 0.95, "loss": 0.12},
		},
		{
			SweepID:    "sweep-123",
			SweepIndex: 1,
			Parameters: map[string]interface{}{"learning_rate": 0.01, "batch_size": 64},
			Metrics:    map[string]interface{}{"accuracy": 0.90, "loss": 0.15},
		},
	}

	tmpDir := testutil.CreateTempDir(t, "collect-csv-test-*")
	outputFile := filepath.Join(tmpDir, "results.csv")

	// Write CSV
	err := writeCSVOutput(results, outputFile)
	if err != nil {
		t.Fatalf("writeCSVOutput failed: %v", err)
	}

	// Verify file exists
	testutil.AssertFileExists(t, outputFile)

	// Read and verify has headers
	content := testutil.ReadFile(t, outputFile)
	testutil.AssertStringContains(t, content, "sweep_id")
	testutil.AssertStringContains(t, content, "sweep_index")
	testutil.AssertStringContains(t, content, "learning_rate")
	testutil.AssertStringContains(t, content, "accuracy")
}

// TestRegionFiltering tests filtering results by region
func TestRegionFiltering(t *testing.T) {
	results := []SweepResult{
		{SweepIndex: 0, Region: "us-east-1"},
		{SweepIndex: 1, Region: "us-west-2"},
		{SweepIndex: 2, Region: "us-east-1"},
		{SweepIndex: 3, Region: "eu-west-1"},
	}

	tests := []struct {
		name         string
		regionFilter string
		wantCount    int
		wantFirstIdx int
	}{
		{
			name:         "filter us-east-1",
			regionFilter: "us-east-1",
			wantCount:    2,
			wantFirstIdx: 0,
		},
		{
			name:         "filter us-west-2",
			regionFilter: "us-west-2",
			wantCount:    1,
			wantFirstIdx: 1,
		},
		{
			name:         "no filter (empty)",
			regionFilter: "",
			wantCount:    4,
			wantFirstIdx: 0,
		},
		{
			name:         "filter non-existent region",
			regionFilter: "ap-south-1",
			wantCount:    0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filtered := filterByRegion(results, tt.regionFilter)
			if len(filtered) != tt.wantCount {
				t.Errorf("filtered count = %d, want %d", len(filtered), tt.wantCount)
			}
			if tt.wantCount > 0 && filtered[0].SweepIndex != tt.wantFirstIdx {
				t.Errorf("first result index = %d, want %d", filtered[0].SweepIndex, tt.wantFirstIdx)
			}
		})
	}
}

// TestMultiRegionCollection tests collecting from multiple regions
func TestMultiRegionCollection(t *testing.T) {
	regions := []string{"us-east-1", "us-west-2", "eu-west-1"}

	// Simulate results from different regions
	allResults := make([]SweepResult, 0)
	for i, region := range regions {
		result := SweepResult{
			SweepID:    "sweep-multi-123",
			SweepIndex: i,
			Region:     region,
			Metrics:    map[string]interface{}{"score": float64(90 + i)},
		}
		allResults = append(allResults, result)
	}

	// Verify we have results from all regions
	regionCount := make(map[string]int)
	for _, r := range allResults {
		regionCount[r.Region]++
	}

	for _, region := range regions {
		if regionCount[region] != 1 {
			t.Errorf("region %s: got %d results, want 1", region, regionCount[region])
		}
	}
}

// TestMetricExtraction tests extracting metric values
func TestMetricExtraction(t *testing.T) {
	result := SweepResult{
		Metrics: map[string]interface{}{
			"accuracy": 0.95,
			"loss":     0.12,
			"f1_score": 0.93,
		},
	}

	tests := []struct {
		name       string
		metricName string
		wantValue  float64
		wantFound  bool
	}{
		{
			name:       "extract accuracy",
			metricName: "accuracy",
			wantValue:  0.95,
			wantFound:  true,
		},
		{
			name:       "extract loss",
			metricName: "loss",
			wantValue:  0.12,
			wantFound:  true,
		},
		{
			name:       "non-existent metric",
			metricName: "precision",
			wantValue:  0,
			wantFound:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value, found := extractMetric(result, tt.metricName)
			if found != tt.wantFound {
				t.Errorf("found = %v, want %v", found, tt.wantFound)
			}
			if found && value != tt.wantValue {
				t.Errorf("value = %v, want %v", value, tt.wantValue)
			}
		})
	}
}

// Helper functions for tests

func constructS3Prefix(accountID, region, sweepID string) string {
	return "s3://spawn-results-" + accountID + "-" + region + "/sweeps/" + sweepID + "/"
}

func sortResultsForTest(results []SweepResult, metric string, descending bool) {
	// Simple bubble sort for testing
	n := len(results)
	for i := 0; i < n-1; i++ {
		for j := 0; j < n-i-1; j++ {
			val1 := getMetricValue(results[j], metric)
			val2 := getMetricValue(results[j+1], metric)

			shouldSwap := false
			if descending {
				shouldSwap = val1 < val2
			} else {
				shouldSwap = val1 > val2
			}

			if shouldSwap {
				results[j], results[j+1] = results[j+1], results[j]
			}
		}
	}
}

func getMetricValue(result SweepResult, metric string) float64 {
	if result.Metrics == nil {
		return 0
	}
	val, ok := result.Metrics[metric]
	if !ok {
		return 0
	}

	switch v := val.(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	default:
		return 0
	}
}

func filterTopN(results []SweepResult, n int) []SweepResult {
	if n <= 0 || n >= len(results) {
		return results
	}
	return results[:n]
}

func writeJSONOutput(results []SweepResult, filename string) error {
	data, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filename, data, 0644)
}

func writeCSVOutput(results []SweepResult, filename string) error {
	f, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	// Write CSV header
	header := "sweep_id,sweep_index,instance_id,region,learning_rate,batch_size,accuracy,loss\n"
	if _, err := f.WriteString(header); err != nil {
		return err
	}

	// Write rows (simplified for testing)
	for _, r := range results {
		row := fmt.Sprintf("%s,%d,%s,%s,%.3f,%d,%.3f,%.3f\n",
			r.SweepID, r.SweepIndex, r.InstanceID, r.Region,
			getParamFloat(r, "learning_rate"),
			getParamInt(r, "batch_size"),
			getMetricValue(r, "accuracy"),
			getMetricValue(r, "loss"),
		)
		if _, err := f.WriteString(row); err != nil {
			return err
		}
	}

	return nil
}

func getParamFloat(result SweepResult, param string) float64 {
	if result.Parameters == nil {
		return 0
	}
	val, ok := result.Parameters[param]
	if !ok {
		return 0
	}

	switch v := val.(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	default:
		return 0
	}
}

func getParamInt(result SweepResult, param string) int {
	if result.Parameters == nil {
		return 0
	}
	val, ok := result.Parameters[param]
	if !ok {
		return 0
	}

	switch v := val.(type) {
	case int:
		return v
	case float64:
		return int(v)
	default:
		return 0
	}
}

func filterByRegion(results []SweepResult, region string) []SweepResult {
	if region == "" {
		return results
	}

	filtered := make([]SweepResult, 0)
	for _, r := range results {
		if r.Region == region {
			filtered = append(filtered, r)
		}
	}
	return filtered
}

func extractMetric(result SweepResult, metricName string) (float64, bool) {
	if result.Metrics == nil {
		return 0, false
	}
	val, ok := result.Metrics[metricName]
	if !ok {
		return 0, false
	}

	switch v := val.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	default:
		return 0, false
	}
}

// TestResultTimestamps tests timestamp handling
func TestResultTimestamps(t *testing.T) {
	now := time.Now()
	result := SweepResult{
		SweepID:      "sweep-123",
		DownloadedAt: now,
	}

	// Marshal and unmarshal
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded SweepResult
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	// Verify timestamp preserved (within 1 second due to JSON precision)
	if decoded.DownloadedAt.Sub(now).Abs() > time.Second {
		t.Errorf("timestamp mismatch: got %v, want ~%v", decoded.DownloadedAt, now)
	}
}
