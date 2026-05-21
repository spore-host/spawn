package pipeline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadPipelineFromJSON(t *testing.T) {
	tests := []struct {
		name    string
		json    string
		wantErr bool
	}{
		{
			name: "valid_simple_pipeline",
			json: `{
				"pipeline_id": "test-pipeline",
				"pipeline_name": "Test Pipeline",
				"s3_bucket": "my-bucket",
				"s3_prefix": "pipelines",
				"stages": [
					{
						"stage_id": "stage1",
						"instance_type": "t3.micro",
						"instance_count": 1,
						"region": "us-east-1",
						"command": "echo hello",
						"depends_on": []
					}
				]
			}`,
			wantErr: false,
		},
		{
			name:    "invalid_json",
			json:    `{invalid json}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadPipelineFromJSON([]byte(tt.json))
			if (err != nil) != tt.wantErr {
				t.Errorf("LoadPipelineFromJSON() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestPipelineValidate(t *testing.T) {
	tests := []struct {
		name     string
		pipeline *Pipeline
		wantErr  bool
		errMsg   string
	}{
		{
			name: "valid_pipeline",
			pipeline: &Pipeline{
				PipelineID:   "test",
				PipelineName: "Test",
				S3Bucket:     "bucket",
				Stages: []Stage{
					{
						StageID:       "stage1",
						InstanceType:  "t3.micro",
						InstanceCount: 1,
						Command:       "echo hello",
						DependsOn:     []string{},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "missing_pipeline_id",
			pipeline: &Pipeline{
				PipelineName: "Test",
				S3Bucket:     "bucket",
				Stages:       []Stage{{StageID: "stage1", InstanceType: "t3.micro", InstanceCount: 1, Command: "echo"}},
			},
			wantErr: true,
			errMsg:  "pipeline_id is required",
		},
		{
			name: "missing_pipeline_name",
			pipeline: &Pipeline{
				PipelineID: "test",
				S3Bucket:   "bucket",
				Stages:     []Stage{{StageID: "stage1", InstanceType: "t3.micro", InstanceCount: 1, Command: "echo"}},
			},
			wantErr: true,
			errMsg:  "pipeline_name is required",
		},
		{
			name: "missing_s3_bucket",
			pipeline: &Pipeline{
				PipelineID:   "test",
				PipelineName: "Test",
				Stages:       []Stage{{StageID: "stage1", InstanceType: "t3.micro", InstanceCount: 1, Command: "echo"}},
			},
			wantErr: true,
			errMsg:  "s3_bucket is required",
		},
		{
			name: "no_stages",
			pipeline: &Pipeline{
				PipelineID:   "test",
				PipelineName: "Test",
				S3Bucket:     "bucket",
				Stages:       []Stage{},
			},
			wantErr: true,
			errMsg:  "at least one stage is required",
		},
		{
			name: "duplicate_stage_id",
			pipeline: &Pipeline{
				PipelineID:   "test",
				PipelineName: "Test",
				S3Bucket:     "bucket",
				Stages: []Stage{
					{StageID: "stage1", InstanceType: "t3.micro", InstanceCount: 1, Command: "echo"},
					{StageID: "stage1", InstanceType: "t3.micro", InstanceCount: 1, Command: "echo"},
				},
			},
			wantErr: true,
			errMsg:  "duplicate stage_id",
		},
		{
			name: "invalid_on_failure",
			pipeline: &Pipeline{
				PipelineID:   "test",
				PipelineName: "Test",
				S3Bucket:     "bucket",
				OnFailure:    "invalid",
				Stages:       []Stage{{StageID: "stage1", InstanceType: "t3.micro", InstanceCount: 1, Command: "echo"}},
			},
			wantErr: true,
			errMsg:  "on_failure must be",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.pipeline.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr && tt.errMsg != "" {
				if err == nil || !contains(err.Error(), tt.errMsg) {
					t.Errorf("Validate() error = %v, want error containing %q", err, tt.errMsg)
				}
			}
		})
	}
}

func TestStageValidate(t *testing.T) {
	validStageIDs := map[string]bool{"stage1": true, "stage2": true}

	tests := []struct {
		name    string
		stage   *Stage
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid_stage",
			stage: &Stage{
				StageID:       "stage1",
				InstanceType:  "t3.micro",
				InstanceCount: 1,
				Command:       "echo hello",
				DependsOn:     []string{},
			},
			wantErr: false,
		},
		{
			name: "missing_instance_type",
			stage: &Stage{
				StageID:       "stage1",
				InstanceCount: 1,
				Command:       "echo",
			},
			wantErr: true,
			errMsg:  "instance_type is required",
		},
		{
			name: "invalid_instance_count",
			stage: &Stage{
				StageID:       "stage1",
				InstanceType:  "t3.micro",
				InstanceCount: 0,
				Command:       "echo",
			},
			wantErr: true,
			errMsg:  "instance_count must be >= 1",
		},
		{
			name: "missing_command",
			stage: &Stage{
				StageID:       "stage1",
				InstanceType:  "t3.micro",
				InstanceCount: 1,
			},
			wantErr: true,
			errMsg:  "command is required",
		},
		{
			name: "invalid_dependency",
			stage: &Stage{
				StageID:       "stage1",
				InstanceType:  "t3.micro",
				InstanceCount: 1,
				Command:       "echo",
				DependsOn:     []string{"nonexistent"},
			},
			wantErr: true,
			errMsg:  "depends_on references unknown stage",
		},
		{
			name: "invalid_timeout",
			stage: &Stage{
				StageID:       "stage1",
				InstanceType:  "t3.micro",
				InstanceCount: 1,
				Command:       "echo",
				Timeout:       "invalid",
			},
			wantErr: true,
			errMsg:  "invalid timeout format",
		},
		{
			name: "valid_timeout",
			stage: &Stage{
				StageID:       "stage1",
				InstanceType:  "t3.micro",
				InstanceCount: 1,
				Command:       "echo",
				Timeout:       "2h30m",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.stage.Validate(validStageIDs)
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr && tt.errMsg != "" {
				if err == nil || !contains(err.Error(), tt.errMsg) {
					t.Errorf("Validate() error = %v, want error containing %q", err, tt.errMsg)
				}
			}
		})
	}
}

func TestDataConfigValidate(t *testing.T) {
	validStageIDs := map[string]bool{"stage1": true, "stage2": true}

	tests := []struct {
		name    string
		config  *DataConfig
		isInput bool
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid_s3_input",
			config: &DataConfig{
				Mode:        "s3",
				SourceStage: "stage1",
				DestPath:    "/data",
			},
			isInput: true,
			wantErr: false,
		},
		{
			name: "valid_s3_output",
			config: &DataConfig{
				Mode:  "s3",
				Paths: []string{"/output/*"},
			},
			isInput: false,
			wantErr: false,
		},
		{
			name: "valid_stream_input",
			config: &DataConfig{
				Mode:        "stream",
				Protocol:    "tcp",
				SourceStage: "stage1",
			},
			isInput: true,
			wantErr: false,
		},
		{
			name: "valid_stream_output",
			config: &DataConfig{
				Mode:     "stream",
				Protocol: "tcp",
				Port:     50000,
			},
			isInput: false,
			wantErr: false,
		},
		{
			name: "invalid_mode",
			config: &DataConfig{
				Mode: "invalid",
			},
			isInput: true,
			wantErr: true,
			errMsg:  "mode must be",
		},
		{
			name: "s3_input_missing_source",
			config: &DataConfig{
				Mode:     "s3",
				DestPath: "/data",
			},
			isInput: true,
			wantErr: true,
			errMsg:  "requires source_stage or source_stages",
		},
		{
			name: "s3_input_invalid_source_stage",
			config: &DataConfig{
				Mode:        "s3",
				SourceStage: "nonexistent",
			},
			isInput: true,
			wantErr: true,
			errMsg:  "references unknown stage",
		},
		{
			name: "stream_input_invalid_protocol",
			config: &DataConfig{
				Mode:        "stream",
				Protocol:    "invalid",
				SourceStage: "stage1",
			},
			isInput: true,
			wantErr: true,
			errMsg:  "requires protocol 'tcp', 'grpc', or 'zmq'",
		},
		{
			name: "stream_output_invalid_port",
			config: &DataConfig{
				Mode:     "stream",
				Protocol: "tcp",
				Port:     99999,
			},
			isInput: false,
			wantErr: true,
			errMsg:  "port must be",
		},
		{
			name: "s3_output_missing_paths",
			config: &DataConfig{
				Mode: "s3",
			},
			isInput: false,
			wantErr: true,
			errMsg:  "requires at least one path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var err error
			if tt.isInput {
				err = tt.config.ValidateInput(validStageIDs)
			} else {
				err = tt.config.ValidateOutput()
			}

			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr && tt.errMsg != "" {
				if err == nil || !contains(err.Error(), tt.errMsg) {
					t.Errorf("Validate() error = %v, want error containing %q", err, tt.errMsg)
				}
			}
		})
	}
}

func TestValidateDAG(t *testing.T) {
	tests := []struct {
		name     string
		pipeline *Pipeline
		wantErr  bool
		errMsg   string
	}{
		{
			name: "valid_linear",
			pipeline: &Pipeline{
				PipelineID:   "test",
				PipelineName: "Test",
				S3Bucket:     "bucket",
				Stages: []Stage{
					{StageID: "stage1", InstanceType: "t3.micro", InstanceCount: 1, Command: "echo", DependsOn: []string{}},
					{StageID: "stage2", InstanceType: "t3.micro", InstanceCount: 1, Command: "echo", DependsOn: []string{"stage1"}},
					{StageID: "stage3", InstanceType: "t3.micro", InstanceCount: 1, Command: "echo", DependsOn: []string{"stage2"}},
				},
			},
			wantErr: false,
		},
		{
			name: "valid_fanout",
			pipeline: &Pipeline{
				PipelineID:   "test",
				PipelineName: "Test",
				S3Bucket:     "bucket",
				Stages: []Stage{
					{StageID: "stage1", InstanceType: "t3.micro", InstanceCount: 1, Command: "echo", DependsOn: []string{}},
					{StageID: "stage2", InstanceType: "t3.micro", InstanceCount: 1, Command: "echo", DependsOn: []string{"stage1"}},
					{StageID: "stage3", InstanceType: "t3.micro", InstanceCount: 1, Command: "echo", DependsOn: []string{"stage1"}},
				},
			},
			wantErr: false,
		},
		{
			name: "valid_fanin",
			pipeline: &Pipeline{
				PipelineID:   "test",
				PipelineName: "Test",
				S3Bucket:     "bucket",
				Stages: []Stage{
					{StageID: "stage1", InstanceType: "t3.micro", InstanceCount: 1, Command: "echo", DependsOn: []string{}},
					{StageID: "stage2", InstanceType: "t3.micro", InstanceCount: 1, Command: "echo", DependsOn: []string{}},
					{StageID: "stage3", InstanceType: "t3.micro", InstanceCount: 1, Command: "echo", DependsOn: []string{"stage1", "stage2"}},
				},
			},
			wantErr: false,
		},
		{
			name: "circular_dependency",
			pipeline: &Pipeline{
				PipelineID:   "test",
				PipelineName: "Test",
				S3Bucket:     "bucket",
				Stages: []Stage{
					{StageID: "stage1", InstanceType: "t3.micro", InstanceCount: 1, Command: "echo", DependsOn: []string{"stage2"}},
					{StageID: "stage2", InstanceType: "t3.micro", InstanceCount: 1, Command: "echo", DependsOn: []string{"stage1"}},
				},
			},
			wantErr: true,
			errMsg:  "circular dependency",
		},
		{
			name: "circular_dependency_3_stages",
			pipeline: &Pipeline{
				PipelineID:   "test",
				PipelineName: "Test",
				S3Bucket:     "bucket",
				Stages: []Stage{
					{StageID: "stage1", InstanceType: "t3.micro", InstanceCount: 1, Command: "echo", DependsOn: []string{"stage3"}},
					{StageID: "stage2", InstanceType: "t3.micro", InstanceCount: 1, Command: "echo", DependsOn: []string{"stage1"}},
					{StageID: "stage3", InstanceType: "t3.micro", InstanceCount: 1, Command: "echo", DependsOn: []string{"stage2"}},
				},
			},
			wantErr: true,
			errMsg:  "circular dependency",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.pipeline.ValidateDAG()
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateDAG() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr && tt.errMsg != "" {
				if err == nil || !contains(err.Error(), tt.errMsg) {
					t.Errorf("ValidateDAG() error = %v, want error containing %q", err, tt.errMsg)
				}
			}
		})
	}
}

func TestGetTopologicalOrder(t *testing.T) {
	pipeline := &Pipeline{
		PipelineID:   "test",
		PipelineName: "Test",
		S3Bucket:     "bucket",
		Stages: []Stage{
			{StageID: "stage3", InstanceType: "t3.micro", InstanceCount: 1, Command: "echo", DependsOn: []string{"stage1", "stage2"}},
			{StageID: "stage1", InstanceType: "t3.micro", InstanceCount: 1, Command: "echo", DependsOn: []string{}},
			{StageID: "stage2", InstanceType: "t3.micro", InstanceCount: 1, Command: "echo", DependsOn: []string{}},
		},
	}

	order, err := pipeline.GetTopologicalOrder()
	if err != nil {
		t.Fatalf("GetTopologicalOrder() error = %v", err)
	}

	// stage1 and stage2 should come before stage3
	stage3Index := -1
	stage1Index := -1
	stage2Index := -1

	for i, stageID := range order {
		switch stageID {
		case "stage1":
			stage1Index = i
		case "stage2":
			stage2Index = i
		case "stage3":
			stage3Index = i
		}
	}

	if stage3Index == -1 || stage1Index == -1 || stage2Index == -1 {
		t.Fatalf("Missing stages in topological order: %v", order)
	}

	if stage1Index >= stage3Index {
		t.Errorf("stage1 should come before stage3 in topological order")
	}

	if stage2Index >= stage3Index {
		t.Errorf("stage2 should come before stage3 in topological order")
	}
}

func TestGetStage(t *testing.T) {
	pipeline := &Pipeline{
		Stages: []Stage{
			{StageID: "stage1", InstanceType: "t3.micro", InstanceCount: 1, Command: "echo"},
			{StageID: "stage2", InstanceType: "t3.micro", InstanceCount: 1, Command: "echo"},
		},
	}

	stage := pipeline.GetStage("stage1")
	if stage == nil {
		t.Fatal("GetStage() returned nil for existing stage")
	}
	if stage.StageID != "stage1" {
		t.Errorf("GetStage() returned wrong stage: got %s, want stage1", stage.StageID)
	}

	stage = pipeline.GetStage("nonexistent")
	if stage != nil {
		t.Error("GetStage() should return nil for nonexistent stage")
	}
}

func TestHasStreamingStages(t *testing.T) {
	tests := []struct {
		name     string
		pipeline *Pipeline
		want     bool
	}{
		{
			name: "no_streaming",
			pipeline: &Pipeline{
				Stages: []Stage{
					{StageID: "stage1", InstanceType: "t3.micro", InstanceCount: 1, Command: "echo"},
				},
			},
			want: false,
		},
		{
			name: "streaming_input",
			pipeline: &Pipeline{
				Stages: []Stage{
					{
						StageID:       "stage1",
						InstanceType:  "t3.micro",
						InstanceCount: 1,
						Command:       "echo",
						DataInput:     &DataConfig{Mode: "stream", Protocol: "tcp", SourceStage: "stage0"},
					},
				},
			},
			want: true,
		},
		{
			name: "streaming_output",
			pipeline: &Pipeline{
				Stages: []Stage{
					{
						StageID:       "stage1",
						InstanceType:  "t3.micro",
						InstanceCount: 1,
						Command:       "echo",
						DataOutput:    &DataConfig{Mode: "stream", Protocol: "tcp", Port: 50000},
					},
				},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.pipeline.HasStreamingStages(); got != tt.want {
				t.Errorf("HasStreamingStages() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHasEFAStages(t *testing.T) {
	tests := []struct {
		name     string
		pipeline *Pipeline
		want     bool
	}{
		{
			name: "no_efa",
			pipeline: &Pipeline{
				Stages: []Stage{
					{StageID: "stage1", InstanceType: "t3.micro", InstanceCount: 1, Command: "echo"},
				},
			},
			want: false,
		},
		{
			name: "has_efa",
			pipeline: &Pipeline{
				Stages: []Stage{
					{StageID: "stage1", InstanceType: "p3.8xlarge", InstanceCount: 1, Command: "echo", EFAEnabled: true},
				},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.pipeline.HasEFAStages(); got != tt.want {
				t.Errorf("HasEFAStages() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLoadPipelineFromFile(t *testing.T) {
	// Create temporary test file
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "pipeline.json")

	pipelineJSON := `{
		"pipeline_id": "test-pipeline",
		"pipeline_name": "Test Pipeline",
		"s3_bucket": "my-bucket",
		"stages": [
			{
				"stage_id": "stage1",
				"instance_type": "t3.micro",
				"instance_count": 1,
				"command": "echo hello",
				"depends_on": []
			}
		]
	}`

	if err := os.WriteFile(testFile, []byte(pipelineJSON), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	pipeline, err := LoadPipelineFromFile(testFile)
	if err != nil {
		t.Fatalf("LoadPipelineFromFile() error = %v", err)
	}

	if pipeline.PipelineID != "test-pipeline" {
		t.Errorf("PipelineID = %s, want test-pipeline", pipeline.PipelineID)
	}

	// Test nonexistent file
	_, err = LoadPipelineFromFile(filepath.Join(tmpDir, "nonexistent.json"))
	if err == nil {
		t.Error("LoadPipelineFromFile() should error on nonexistent file")
	}
}

func TestJSONRoundTrip(t *testing.T) {
	original := &Pipeline{
		PipelineID:     "test-pipeline",
		PipelineName:   "Test Pipeline",
		S3Bucket:       "my-bucket",
		S3Prefix:       "pipelines",
		ResultS3Bucket: "results",
		ResultS3Prefix: "results/pipeline",
		OnFailure:      "stop",
		Stages: []Stage{
			{
				StageID:       "stage1",
				InstanceType:  "t3.micro",
				InstanceCount: 1,
				Region:        "us-east-1",
				Command:       "echo hello",
				DependsOn:     []string{},
				Env: map[string]string{
					"KEY": "value",
				},
			},
		},
	}

	// Marshal to JSON
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	// Unmarshal back
	var parsed Pipeline
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	// Compare key fields
	if parsed.PipelineID != original.PipelineID {
		t.Errorf("PipelineID mismatch: got %s, want %s", parsed.PipelineID, original.PipelineID)
	}
	if parsed.PipelineName != original.PipelineName {
		t.Errorf("PipelineName mismatch: got %s, want %s", parsed.PipelineName, original.PipelineName)
	}
	if len(parsed.Stages) != len(original.Stages) {
		t.Errorf("Stages length mismatch: got %d, want %d", len(parsed.Stages), len(original.Stages))
	}
}

// Helper function
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && containsSubstring(s, substr)))
}

func containsSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
