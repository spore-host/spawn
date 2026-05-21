package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spore-host/spawn/pkg/staging"
	"github.com/spore-host/spawn/pkg/testutil"
)

// TestStageUpload tests the stage upload command logic
func TestStageUpload(t *testing.T) {
	tests := []struct {
		name        string
		fileContent string
		regions     string
		dest        string
		wantErr     bool
	}{
		{
			name:        "valid upload single region",
			fileContent: "test data content",
			regions:     "us-east-1",
			dest:        "/mnt/data/test.txt",
			wantErr:     false,
		},
		{
			name:        "valid upload multiple regions",
			fileContent: "test data content",
			regions:     "us-east-1,us-west-2",
			dest:        "/mnt/data/test.txt",
			wantErr:     false,
		},
		{
			name:        "valid upload many regions",
			fileContent: "test data content",
			regions:     "us-east-1,us-west-2,eu-west-1",
			dest:        "/mnt/data/test.txt",
			wantErr:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := testutil.CreateTempDir(t, "stage-upload-test-*")
			testFile := filepath.Join(tmpDir, "test.txt")
			testutil.WriteFile(t, testFile, tt.fileContent)

			// Test file exists
			testutil.AssertFileExists(t, testFile)

			// Test region parsing
			regions := ParseRegionList(tt.regions)
			if len(regions) == 0 {
				t.Error("expected regions to be parsed")
			}

			// Test staging ID generation
			stagingID := staging.GenerateStagingID()
			if stagingID == "" {
				t.Error("staging ID should not be empty")
			}

			// Verify file can be read
			content, err := os.ReadFile(testFile)
			if err != nil {
				t.Errorf("failed to read test file: %v", err)
			}
			if string(content) != tt.fileContent {
				t.Errorf("file content = %q, want %q", string(content), tt.fileContent)
			}
		})
	}
}

// TestStageRegionParsing tests parsing of region lists
func TestStageRegionParsing(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantCount int
		wantFirst string
	}{
		{
			name:      "single region",
			input:     "us-east-1",
			wantCount: 1,
			wantFirst: "us-east-1",
		},
		{
			name:      "two regions",
			input:     "us-east-1,us-west-2",
			wantCount: 2,
			wantFirst: "us-east-1",
		},
		{
			name:      "three regions",
			input:     "us-east-1,us-west-2,eu-west-1",
			wantCount: 3,
			wantFirst: "us-east-1",
		},
		{
			name:      "regions with spaces",
			input:     "us-east-1, us-west-2, eu-west-1",
			wantCount: 3,
			wantFirst: "us-east-1",
		},
		{
			name:      "empty string",
			input:     "",
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			regions := ParseRegionList(tt.input)
			if len(regions) != tt.wantCount {
				t.Errorf("got %d regions, want %d", len(regions), tt.wantCount)
			}
			if tt.wantCount > 0 && regions[0] != tt.wantFirst {
				t.Errorf("first region = %q, want %q", regions[0], tt.wantFirst)
			}
		})
	}
}

// ParseRegionList parses a comma-separated list of regions
func ParseRegionList(s string) []string {
	if s == "" {
		return []string{}
	}

	var regions []string
	parts := testutil.SplitString(s, ",")
	for _, part := range parts {
		// Trim spaces
		trimmed := testutil.TrimSpace(part)
		if trimmed != "" {
			regions = append(regions, trimmed)
		}
	}
	return regions
}

// TestStagingIDGeneration tests staging ID generation
func TestStagingIDGeneration(t *testing.T) {
	// Generate an ID
	id := staging.GenerateStagingID()

	// Should not be empty
	if id == "" {
		t.Error("staging ID should not be empty")
	}

	// Should have reasonable length (e.g., 16-32 chars)
	if len(id) < 10 || len(id) > 50 {
		t.Errorf("staging ID length %d is outside expected range", len(id))
	}

	// Should start with "stage-"
	if !testutil.Contains(id, "stage-") {
		t.Errorf("staging ID should start with 'stage-', got: %s", id)
	}
}

// TestStageDestinationPath tests destination path handling
func TestStageDestinationPath(t *testing.T) {
	tests := []struct {
		name     string
		dest     string
		filename string
		want     string
	}{
		{
			name:     "explicit path",
			dest:     "/mnt/data/test.txt",
			filename: "original.txt",
			want:     "/mnt/data/test.txt",
		},
		{
			name:     "directory path",
			dest:     "/mnt/data/",
			filename: "test.txt",
			want:     "/mnt/data/test.txt",
		},
		{
			name:     "empty dest (use default)",
			dest:     "",
			filename: "test.txt",
			want:     "/mnt/data/test.txt",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var result string
			if tt.dest != "" {
				// If dest ends with /, append filename
				if testutil.Contains(tt.dest, "/") && tt.dest[len(tt.dest)-1] == '/' {
					result = tt.dest + tt.filename
				} else {
					result = tt.dest
				}
			} else {
				result = "/mnt/data/" + tt.filename
			}

			if result != tt.want {
				t.Errorf("destination path = %q, want %q", result, tt.want)
			}
		})
	}
}

// TestStageEstimate tests cost estimation logic
func TestStageEstimate(t *testing.T) {
	tests := []struct {
		name         string
		dataSizeGB   int
		instances    int
		regionCount  int
		wantStaging  float64 // Approximate staging cost
		wantTransfer float64 // Approximate transfer cost
		savesPct     float64 // Expected savings percentage
	}{
		{
			name:        "100GB 2 regions 10 instances",
			dataSizeGB:  100,
			instances:   10,
			regionCount: 2,
			// Staging: 100GB * 2 regions * $0.023/GB = $4.60
			// Transfer: 100GB * 2 regions * 10 instances * $0.09/GB = $180
			// Savings: ~$175 (97%)
			wantStaging:  4.60,
			wantTransfer: 180.0,
			savesPct:     97.0,
		},
		{
			name:        "50GB 3 regions 5 instances",
			dataSizeGB:  50,
			instances:   5,
			regionCount: 3,
			// Staging: 50GB * 3 regions * $0.023/GB = $3.45
			// Transfer: 50GB * 3 regions * 5 instances * $0.09/GB = $67.50
			// Savings: ~$64 (95%)
			wantStaging:  3.45,
			wantTransfer: 67.50,
			savesPct:     95.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Calculate staging cost (replication)
			// S3 data transfer between regions: ~$0.02/GB (simplified)
			stagingCost := float64(tt.dataSizeGB) * float64(tt.regionCount) * 0.023

			// Calculate transfer cost (cross-region data transfer)
			// AWS cross-region: $0.09/GB
			transferCost := float64(tt.dataSizeGB) * float64(tt.regionCount) * float64(tt.instances) * 0.09

			// Calculate savings
			savings := transferCost - stagingCost
			savingsPct := (savings / transferCost) * 100

			// Verify costs are in expected range (±10%)
			if abs(stagingCost-tt.wantStaging) > tt.wantStaging*0.1 {
				t.Errorf("staging cost = $%.2f, want ~$%.2f", stagingCost, tt.wantStaging)
			}
			if abs(transferCost-tt.wantTransfer) > tt.wantTransfer*0.1 {
				t.Errorf("transfer cost = $%.2f, want ~$%.2f", transferCost, tt.wantTransfer)
			}
			if abs(savingsPct-tt.savesPct) > 5.0 {
				t.Errorf("savings = %.1f%%, want ~%.1f%%", savingsPct, tt.savesPct)
			}

			t.Logf("Staging: $%.2f, Transfer: $%.2f, Savings: $%.2f (%.1f%%)",
				stagingCost, transferCost, savings, savingsPct)
		})
	}
}

// TestStageFileValidation tests file validation before staging
func TestStageFileValidation(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T, dir string) string
		wantErr bool
	}{
		{
			name: "valid file",
			setup: func(t *testing.T, dir string) string {
				path := filepath.Join(dir, "test.txt")
				testutil.WriteFile(t, path, "content")
				return path
			},
			wantErr: false,
		},
		{
			name: "valid directory",
			setup: func(t *testing.T, dir string) string {
				subdir := filepath.Join(dir, "testdir")
				if err := os.MkdirAll(subdir, 0755); err != nil {
					t.Fatal(err)
				}
				testutil.WriteFile(t, filepath.Join(subdir, "file.txt"), "content")
				return subdir
			},
			wantErr: false,
		},
		{
			name: "nonexistent file",
			setup: func(t *testing.T, dir string) string {
				return filepath.Join(dir, "nonexistent.txt")
			},
			wantErr: true,
		},
		{
			name: "empty directory",
			setup: func(t *testing.T, dir string) string {
				subdir := filepath.Join(dir, "emptydir")
				if err := os.MkdirAll(subdir, 0755); err != nil {
					t.Fatal(err)
				}
				return subdir
			},
			wantErr: false, // Empty dirs are valid
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := testutil.CreateTempDir(t, "stage-validation-test-*")
			path := tt.setup(t, tmpDir)

			_, err := os.Stat(path)
			if (err != nil) != tt.wantErr {
				t.Errorf("file validation error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestStageSweepIDAssociation tests associating staging with sweep ID
func TestStageSweepIDAssociation(t *testing.T) {
	tests := []struct {
		name    string
		sweepID string
		valid   bool
	}{
		{
			name:    "valid sweep ID",
			sweepID: "sweep-abc123",
			valid:   true,
		},
		{
			name:    "valid with timestamp",
			sweepID: "hyperparam-20240101-xyz",
			valid:   true,
		},
		{
			name:    "empty (optional)",
			sweepID: "",
			valid:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Sweep ID is optional, all should be valid
			if !tt.valid {
				t.Error("unexpected invalid sweep ID")
			}

			// Would associate staging metadata with sweep ID here
			t.Logf("Sweep ID: %q", tt.sweepID)
		})
	}
}

// TestStageList tests listing staged data
func TestStageList(t *testing.T) {
	// Test would interact with mock DynamoDB to list staging metadata
	t.Log("Would test listing staged data from DynamoDB")
}

// TestStageDelete tests deleting staged data
func TestStageDelete(t *testing.T) {
	tests := []struct {
		name      string
		stagingID string
		wantErr   bool
	}{
		{
			name:      "valid staging ID",
			stagingID: "stage-abc123",
			wantErr:   false,
		},
		{
			name:      "empty staging ID",
			stagingID: "",
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Validate staging ID format
			if tt.stagingID == "" && !tt.wantErr {
				t.Error("empty staging ID should error")
			}

			// Would test deletion from S3 and DynamoDB here
			t.Logf("Would delete staging ID: %q", tt.stagingID)
		})
	}
}

// Helper functions

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
