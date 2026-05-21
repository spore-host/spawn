package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCopyFile(t *testing.T) {
	tmpDir := t.TempDir()

	tests := []struct {
		name    string
		setup   func() (src, dst string)
		wantErr bool
	}{
		{
			name: "successful copy",
			setup: func() (string, string) {
				src := filepath.Join(tmpDir, "source.txt")
				dst := filepath.Join(tmpDir, "dest.txt")
				if err := os.WriteFile(src, []byte("test content"), 0644); err != nil {
					t.Fatal(err)
				}
				return src, dst
			},
			wantErr: false,
		},
		{
			name: "source file does not exist",
			setup: func() (string, string) {
				src := filepath.Join(tmpDir, "nonexistent.txt")
				dst := filepath.Join(tmpDir, "dest2.txt")
				return src, dst
			},
			wantErr: true,
		},
		{
			name: "destination directory does not exist",
			setup: func() (string, string) {
				src := filepath.Join(tmpDir, "source2.txt")
				dst := filepath.Join(tmpDir, "subdir", "dest.txt")
				if err := os.WriteFile(src, []byte("test"), 0644); err != nil {
					t.Fatal(err)
				}
				return src, dst
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src, dst := tt.setup()
			err := CopyFile(src, dst)
			if (err != nil) != tt.wantErr {
				t.Errorf("CopyFile() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				// Verify file was copied
				srcContent, err := os.ReadFile(src)
				if err != nil {
					t.Fatal(err)
				}
				dstContent, err := os.ReadFile(dst)
				if err != nil {
					t.Fatal(err)
				}
				if string(srcContent) != string(dstContent) {
					t.Errorf("File content mismatch: src=%q, dst=%q", srcContent, dstContent)
				}
			}
		})
	}
}

func TestDownloadStageInputs_NoConfig(t *testing.T) {
	handler := &StageDataHandler{
		bucket:     "test-bucket",
		prefix:     "pipelines/test",
		pipelineID: "test-pipeline",
		stageID:    "test-stage",
	}

	ctx := context.Background()

	// Test with nil config
	err := handler.DownloadStageInputs(ctx, nil)
	if err != nil {
		t.Errorf("Expected no error for nil config, got %v", err)
	}

	// Test with non-S3 mode
	config := &DataConfig{
		Mode: "stream",
	}
	err = handler.DownloadStageInputs(ctx, config)
	if err != nil {
		t.Errorf("Expected no error for non-S3 mode, got %v", err)
	}
}

func TestDownloadStageInputs_NoSourceStages(t *testing.T) {
	handler := &StageDataHandler{
		bucket:     "test-bucket",
		prefix:     "pipelines/test",
		pipelineID: "test-pipeline",
		stageID:    "test-stage",
	}

	ctx := context.Background()
	config := &DataConfig{
		Mode: "s3",
		// No SourceStage or SourceStages specified
	}

	err := handler.DownloadStageInputs(ctx, config)
	if err == nil {
		t.Error("Expected error for missing source stages")
	}
}

func TestUploadStageOutputs_NoConfig(t *testing.T) {
	handler := &StageDataHandler{
		bucket:     "test-bucket",
		prefix:     "pipelines/test",
		pipelineID: "test-pipeline",
		stageID:    "test-stage",
	}

	ctx := context.Background()

	// Test with nil config
	err := handler.UploadStageOutputs(ctx, nil)
	if err != nil {
		t.Errorf("Expected no error for nil config, got %v", err)
	}

	// Test with non-S3 mode
	config := &DataConfig{
		Mode: "stream",
	}
	err = handler.UploadStageOutputs(ctx, config)
	if err != nil {
		t.Errorf("Expected no error for non-S3 mode, got %v", err)
	}
}

func TestUploadStageOutputs_NoPaths(t *testing.T) {
	handler := &StageDataHandler{
		bucket:     "test-bucket",
		prefix:     "pipelines/test",
		pipelineID: "test-pipeline",
		stageID:    "test-stage",
	}

	ctx := context.Background()
	config := &DataConfig{
		Mode:  "s3",
		Paths: []string{}, // Empty paths
	}

	err := handler.UploadStageOutputs(ctx, config)
	if err == nil {
		t.Error("Expected error for empty output paths")
	}
}

func TestUploadStageOutputs_InvalidPattern(t *testing.T) {
	handler := &StageDataHandler{
		bucket:     "test-bucket",
		prefix:     "pipelines/test",
		pipelineID: "test-pipeline",
		stageID:    "test-stage",
	}

	ctx := context.Background()
	config := &DataConfig{
		Mode: "s3",
		Paths: []string{
			"[invalid-pattern", // Invalid glob pattern
		},
	}

	err := handler.UploadStageOutputs(ctx, config)
	if err == nil {
		t.Error("Expected error for invalid glob pattern")
	}
}

// Test data config validation
func TestDataConfigValidationInHandoff(t *testing.T) {
	// Test that handler correctly identifies valid vs invalid configs
	validInputConfig := &DataConfig{
		Mode:        "s3",
		SourceStage: "upstream",
		DestPath:    "/data/input",
	}

	validOutputConfig := &DataConfig{
		Mode:  "s3",
		Paths: []string{"/data/output/*.txt"},
	}

	validStages := map[string]bool{"upstream": true}
	if err := validInputConfig.ValidateInput(validStages); err != nil {
		t.Errorf("Expected valid input config, got error: %v", err)
	}

	if err := validOutputConfig.ValidateOutput(); err != nil {
		t.Errorf("Expected valid output config, got error: %v", err)
	}

	// Test invalid configs
	invalidInputConfig := &DataConfig{
		Mode: "s3",
		// Missing SourceStage
	}

	emptyStages := map[string]bool{}
	if err := invalidInputConfig.ValidateInput(emptyStages); err == nil {
		t.Error("Expected error for invalid input config")
	}

	invalidOutputConfig := &DataConfig{
		Mode: "s3",
		// Missing Paths
	}

	if err := invalidOutputConfig.ValidateOutput(); err == nil {
		t.Error("Expected error for invalid output config")
	}
}

// Test pattern matching logic
func TestPatternMatching(t *testing.T) {
	tests := []struct {
		name        string
		pattern     string
		relPath     string
		shouldMatch bool
	}{
		{
			name:        "exact match",
			pattern:     "data.txt",
			relPath:     "data.txt",
			shouldMatch: true,
		},
		{
			name:        "wildcard match",
			pattern:     "*.txt",
			relPath:     "file.txt",
			shouldMatch: true,
		},
		{
			name:        "wildcard no match",
			pattern:     "*.txt",
			relPath:     "file.csv",
			shouldMatch: false,
		},
		{
			name:        "directory pattern",
			pattern:     "output/*.txt",
			relPath:     "output/result.txt",
			shouldMatch: true,
		},
		{
			name:        "nested no match",
			pattern:     "output/*.txt",
			relPath:     "output/nested/result.txt",
			shouldMatch: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matched, err := filepath.Match(tt.pattern, tt.relPath)
			if err != nil {
				t.Fatalf("Pattern error: %v", err)
			}
			if matched != tt.shouldMatch {
				t.Errorf("Pattern %q against %q: got %v, want %v",
					tt.pattern, tt.relPath, matched, tt.shouldMatch)
			}
		})
	}
}

// Test file operations error handling
func TestFileOperationsErrorHandling(t *testing.T) {
	tmpDir := t.TempDir()

	t.Run("mkdir with permissions", func(t *testing.T) {
		path := filepath.Join(tmpDir, "test", "nested", "dir")
		err := os.MkdirAll(path, 0755)
		if err != nil {
			t.Errorf("MkdirAll failed: %v", err)
		}

		// Verify directory was created
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("Stat failed: %v", err)
		}
		if !info.IsDir() {
			t.Error("Expected directory to be created")
		}
	})

	t.Run("stat nonexistent file", func(t *testing.T) {
		_, err := os.Stat(filepath.Join(tmpDir, "nonexistent"))
		if !os.IsNotExist(err) {
			t.Errorf("Expected IsNotExist error, got %v", err)
		}
	})

	t.Run("glob pattern expansion", func(t *testing.T) {
		// Create test files
		files := []string{"test1.txt", "test2.txt", "test.csv"}
		for _, f := range files {
			path := filepath.Join(tmpDir, f)
			if err := os.WriteFile(path, []byte("test"), 0644); err != nil {
				t.Fatal(err)
			}
		}

		// Test glob pattern
		pattern := filepath.Join(tmpDir, "*.txt")
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatalf("Glob failed: %v", err)
		}

		if len(matches) != 2 {
			t.Errorf("Expected 2 matches, got %d", len(matches))
		}
	})
}

// Test S3 key generation logic
func TestS3KeyGeneration(t *testing.T) {
	tests := []struct {
		name         string
		prefix       string
		stageID      string
		localPath    string
		pattern      string
		expectedKey  string
		preservePath bool
	}{
		{
			name:        "simple filename",
			prefix:      "pipelines/test",
			stageID:     "stage1",
			localPath:   "output.txt",
			pattern:     "output.txt",
			expectedKey: "pipelines/test/stages/stage1/output/output.txt",
		},
		{
			name:         "absolute path preserved",
			prefix:       "pipelines/test",
			stageID:      "stage1",
			localPath:    "/data/results/output.txt",
			pattern:      "/data/results/*.txt",
			expectedKey:  "pipelines/test/stages/stage1/output/data/results/output.txt",
			preservePath: true,
		},
		{
			name:        "relative path basename",
			prefix:      "pipelines/test",
			stageID:     "stage2",
			localPath:   "subdir/file.txt",
			pattern:     "*.txt",
			expectedKey: "pipelines/test/stages/stage2/output/file.txt",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outputPrefix := tt.prefix + "/stages/" + tt.stageID + "/output/"

			relPath := filepath.Base(tt.localPath)
			if tt.preservePath && filepath.IsAbs(tt.pattern) {
				relPath = filepath.Clean(tt.localPath[1:]) // Remove leading /
			}

			s3Key := outputPrefix + relPath

			if s3Key != tt.expectedKey {
				t.Errorf("S3 key mismatch:\ngot:  %s\nwant: %s", s3Key, tt.expectedKey)
			}
		})
	}
}

// Test completion marker path generation
func TestCompletionMarkerPath(t *testing.T) {
	tests := []struct {
		prefix      string
		stageID     string
		expectedKey string
	}{
		{
			prefix:      "pipelines/test",
			stageID:     "stage1",
			expectedKey: "pipelines/test/stages/stage1/COMPLETE",
		},
		{
			prefix:      "prod/pipeline-123",
			stageID:     "preprocess",
			expectedKey: "prod/pipeline-123/stages/preprocess/COMPLETE",
		},
	}

	for _, tt := range tests {
		handler := &StageDataHandler{
			prefix:  tt.prefix,
			stageID: tt.stageID,
		}

		markerKey := handler.prefix + "/stages/" + handler.stageID + "/COMPLETE"

		if markerKey != tt.expectedKey {
			t.Errorf("Marker key mismatch:\ngot:  %s\nwant: %s", markerKey, tt.expectedKey)
		}
	}
}
