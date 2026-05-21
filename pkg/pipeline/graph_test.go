package pipeline

import (
	"strings"
	"testing"
)

func TestRenderGraph(t *testing.T) {
	tests := []struct {
		name     string
		pipeline *Pipeline
		wantErr  bool
	}{
		{
			name: "simple_linear",
			pipeline: &Pipeline{
				PipelineID:   "test",
				PipelineName: "Simple Linear Pipeline",
				S3Bucket:     "bucket",
				Stages: []Stage{
					{StageID: "stage1", InstanceType: "t3.micro", InstanceCount: 1, Command: "echo", DependsOn: []string{}},
					{StageID: "stage2", InstanceType: "t3.micro", InstanceCount: 1, Command: "echo", DependsOn: []string{"stage1"}},
				},
			},
			wantErr: false,
		},
		{
			name: "fanout",
			pipeline: &Pipeline{
				PipelineID:   "test",
				PipelineName: "Fan-out Pipeline",
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
			name: "with_streaming",
			pipeline: &Pipeline{
				PipelineID:   "test",
				PipelineName: "Streaming Pipeline",
				S3Bucket:     "bucket",
				Stages: []Stage{
					{
						StageID:       "capture",
						InstanceType:  "c5n.xlarge",
						InstanceCount: 1,
						Command:       "capture",
						DependsOn:     []string{},
						DataOutput:    &DataConfig{Mode: "stream", Protocol: "tcp", Port: 50000},
					},
					{
						StageID:       "process",
						InstanceType:  "g4dn.xlarge",
						InstanceCount: 1,
						Command:       "process",
						DependsOn:     []string{"capture"},
						DataInput:     &DataConfig{Mode: "stream", Protocol: "tcp", SourceStage: "capture"},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "circular_dependency",
			pipeline: &Pipeline{
				PipelineID:   "test",
				PipelineName: "Invalid Pipeline",
				S3Bucket:     "bucket",
				Stages: []Stage{
					{StageID: "stage1", InstanceType: "t3.micro", InstanceCount: 1, Command: "echo", DependsOn: []string{"stage2"}},
					{StageID: "stage2", InstanceType: "t3.micro", InstanceCount: 1, Command: "echo", DependsOn: []string{"stage1"}},
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			graph, err := tt.pipeline.RenderGraph()
			if (err != nil) != tt.wantErr {
				t.Errorf("RenderGraph() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				// Check that output contains pipeline name
				if !strings.Contains(graph, tt.pipeline.PipelineName) {
					t.Errorf("RenderGraph() output missing pipeline name")
				}
				// Check that output contains all stage IDs
				for _, stage := range tt.pipeline.Stages {
					if !strings.Contains(graph, stage.StageID) {
						t.Errorf("RenderGraph() output missing stage ID: %s", stage.StageID)
					}
				}
			}
		})
	}
}

func TestRenderSimpleGraph(t *testing.T) {
	pipeline := &Pipeline{
		PipelineID:   "test",
		PipelineName: "Test Pipeline",
		S3Bucket:     "bucket",
		Stages: []Stage{
			{StageID: "stage1", InstanceType: "t3.micro", InstanceCount: 1, Command: "echo", DependsOn: []string{}},
			{StageID: "stage2", InstanceType: "t3.micro", InstanceCount: 2, Command: "echo", DependsOn: []string{"stage1"}},
		},
	}

	graph, err := pipeline.RenderSimpleGraph()
	if err != nil {
		t.Fatalf("RenderSimpleGraph() error = %v", err)
	}

	// Check that output contains stage IDs
	if !strings.Contains(graph, "stage1") {
		t.Error("RenderSimpleGraph() missing stage1")
	}
	if !strings.Contains(graph, "stage2") {
		t.Error("RenderSimpleGraph() missing stage2")
	}

	// Check that instance count is shown for stage2
	if !strings.Contains(graph, "×2") {
		t.Error("RenderSimpleGraph() missing instance count annotation")
	}

	// Check for arrow
	if !strings.Contains(graph, "▼") {
		t.Error("RenderSimpleGraph() missing arrow between stages")
	}
}

func TestGetGraphStats(t *testing.T) {
	pipeline := &Pipeline{
		PipelineID:   "test",
		PipelineName: "Test Pipeline",
		S3Bucket:     "bucket",
		Stages: []Stage{
			{StageID: "stage1", InstanceType: "t3.micro", InstanceCount: 2, Command: "echo", DependsOn: []string{}},
			{StageID: "stage2", InstanceType: "t3.micro", InstanceCount: 3, Command: "echo", DependsOn: []string{"stage1"}},
			{StageID: "stage3", InstanceType: "t3.micro", InstanceCount: 1, Command: "echo", DependsOn: []string{"stage1"}},
			{
				StageID:       "stage4",
				InstanceType:  "t3.micro",
				InstanceCount: 1,
				Command:       "echo",
				DependsOn:     []string{"stage2", "stage3"},
				EFAEnabled:    true,
				DataInput:     &DataConfig{Mode: "stream", Protocol: "tcp", SourceStage: "stage2"},
			},
		},
	}

	stats := pipeline.GetGraphStats()

	// Check total stages
	if stats["total_stages"] != 4 {
		t.Errorf("total_stages = %v, want 4", stats["total_stages"])
	}

	// Check total instances
	if stats["total_instances"] != 7 {
		t.Errorf("total_instances = %v, want 7 (2+3+1+1)", stats["total_instances"])
	}

	// Check max fan-out (stage1 -> stage2, stage3)
	if stats["max_fan_out"] != 2 {
		t.Errorf("max_fan_out = %v, want 2", stats["max_fan_out"])
	}

	// Check max fan-in (stage4 depends on stage2, stage3)
	if stats["max_fan_in"] != 2 {
		t.Errorf("max_fan_in = %v, want 2", stats["max_fan_in"])
	}

	// Check streaming detection
	if stats["has_streaming"] != true {
		t.Error("has_streaming should be true")
	}

	// Check EFA detection
	if stats["has_efa"] != true {
		t.Error("has_efa should be true")
	}
}

func TestRenderGraphWithMultipleInstances(t *testing.T) {
	pipeline := &Pipeline{
		PipelineID:   "test",
		PipelineName: "Multi-Instance Pipeline",
		S3Bucket:     "bucket",
		Stages: []Stage{
			{
				StageID:       "preprocess",
				InstanceType:  "c5.2xlarge",
				InstanceCount: 1,
				Region:        "us-east-1",
				Command:       "preprocess",
				DependsOn:     []string{},
			},
			{
				StageID:        "train",
				InstanceType:   "p3.8xlarge",
				InstanceCount:  4,
				Region:         "us-east-1",
				Spot:           true,
				EFAEnabled:     true,
				PlacementGroup: "auto",
				Command:        "train",
				DependsOn:      []string{"preprocess"},
			},
		},
	}

	graph, err := pipeline.RenderGraph()
	if err != nil {
		t.Fatalf("RenderGraph() error = %v", err)
	}

	// Check for instance count
	if !strings.Contains(graph, "×4") {
		t.Error("RenderGraph() should show instance count for train stage")
	}

	// Check for spot indicator
	if !strings.Contains(graph, "Spot: true") {
		t.Error("RenderGraph() should show spot indicator")
	}

	// Check for EFA indicator
	if !strings.Contains(graph, "EFA: enabled") {
		t.Error("RenderGraph() should show EFA indicator")
	}
}

func TestRenderGraphComplexTopology(t *testing.T) {
	// Fan-out and fan-in pattern
	pipeline := &Pipeline{
		PipelineID:   "test",
		PipelineName: "Complex Topology",
		S3Bucket:     "bucket",
		Stages: []Stage{
			{StageID: "split", InstanceType: "t3.micro", InstanceCount: 1, Command: "split", DependsOn: []string{}},
			{StageID: "process1", InstanceType: "t3.micro", InstanceCount: 1, Command: "process", DependsOn: []string{"split"}},
			{StageID: "process2", InstanceType: "t3.micro", InstanceCount: 1, Command: "process", DependsOn: []string{"split"}},
			{StageID: "process3", InstanceType: "t3.micro", InstanceCount: 1, Command: "process", DependsOn: []string{"split"}},
			{StageID: "aggregate", InstanceType: "t3.micro", InstanceCount: 1, Command: "aggregate", DependsOn: []string{"process1", "process2", "process3"}},
		},
	}

	graph, err := pipeline.RenderGraph()
	if err != nil {
		t.Fatalf("RenderGraph() error = %v", err)
	}

	// Check that all stages are present
	for _, stage := range pipeline.Stages {
		if !strings.Contains(graph, stage.StageID) {
			t.Errorf("RenderGraph() missing stage: %s", stage.StageID)
		}
	}

	// Check for fan-out arrows
	if !strings.Contains(graph, "──▶") {
		t.Error("RenderGraph() should show fan-out arrows")
	}
}
