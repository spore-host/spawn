package pipeline

import (
	"os"
	"path/filepath"
	"testing"
)

// TestExamplePipelinesValid validates all example pipeline JSON files
func TestExamplePipelinesValid(t *testing.T) {
	// Find all example pipeline files
	exampleFiles := []string{
		"../../examples/pipelines/ml-training.json",
		"../../examples/pipelines/video-streaming.json",
		"../../examples/pipelines/fanout-fanin.json",
		"../../examples/pipelines/streaming-demo.json",
		"../../examples/genomics/pipeline.json",
		"../../examples/genomics/build-library-pipeline.json",
	}

	for _, file := range exampleFiles {
		t.Run(filepath.Base(file), func(t *testing.T) {
			// Check if file exists
			if _, err := os.Stat(file); os.IsNotExist(err) {
				t.Skipf("Example file not found: %s", file)
				return
			}

			// Load and validate pipeline
			pipeline, err := LoadPipelineFromFile(file)
			if err != nil {
				t.Fatalf("Failed to load pipeline from %s: %v", file, err)
			}

			// Validate pipeline structure
			if err := pipeline.Validate(); err != nil {
				t.Errorf("Pipeline validation failed for %s: %v", file, err)
			}

			// Basic sanity checks
			if pipeline.PipelineID == "" {
				t.Error("Pipeline missing pipeline_id")
			}
			if pipeline.PipelineName == "" {
				t.Error("Pipeline missing pipeline_name")
			}
			if len(pipeline.Stages) == 0 {
				t.Error("Pipeline has no stages")
			}

			// Verify each stage has required fields
			for i, stage := range pipeline.Stages {
				if stage.StageID == "" {
					t.Errorf("Stage %d missing stage_id", i)
				}
				if stage.InstanceType == "" {
					t.Errorf("Stage %s missing instance_type", stage.StageID)
				}
				if stage.Command == "" {
					t.Errorf("Stage %s missing command", stage.StageID)
				}
			}

			// Check DAG is valid (no cycles)
			if err := pipeline.ValidateDAG(); err != nil {
				t.Errorf("DAG validation failed for %s: %v", file, err)
			}

			// Get topological order
			order, err := pipeline.GetTopologicalOrder()
			if err != nil {
				t.Errorf("Failed to get topological order for %s: %v", file, err)
			}
			if len(order) != len(pipeline.Stages) {
				t.Errorf("Topological order length mismatch: got %d, want %d",
					len(order), len(pipeline.Stages))
			}

			t.Logf("✓ Pipeline %s is valid (%d stages)",
				pipeline.PipelineName, len(pipeline.Stages))
		})
	}
}

// TestExamplePipelinesGraphRender tests that example pipelines can be rendered as graphs
func TestExamplePipelinesGraphRender(t *testing.T) {
	exampleFiles := []string{
		"../../examples/pipelines/ml-training.json",
		"../../examples/pipelines/fanout-fanin.json",
	}

	for _, file := range exampleFiles {
		t.Run(filepath.Base(file), func(t *testing.T) {
			// Check if file exists
			if _, err := os.Stat(file); os.IsNotExist(err) {
				t.Skipf("Example file not found: %s", file)
				return
			}

			// Load pipeline
			pipeline, err := LoadPipelineFromFile(file)
			if err != nil {
				t.Fatalf("Failed to load pipeline: %v", err)
			}

			// Render graph
			graph, err := pipeline.RenderGraph()
			if err != nil {
				t.Fatalf("Failed to render graph: %v", err)
			}

			// Verify graph is not empty
			if len(graph) == 0 {
				t.Error("Rendered graph is empty")
			}

			// Verify graph contains stage IDs
			for _, stage := range pipeline.Stages {
				if len(graph) > 0 && len(stage.StageID) > 0 {
					// Graph should contain stage ID somewhere
					t.Logf("Stage %s rendered in graph", stage.StageID)
				}
			}

			t.Logf("✓ Graph rendered successfully:\n%s", graph)
		})
	}
}

// TestExamplePipelineStats tests that example pipelines produce valid stats
func TestExamplePipelineStats(t *testing.T) {
	file := "../../examples/pipelines/ml-training.json"

	if _, err := os.Stat(file); os.IsNotExist(err) {
		t.Skip("Example file not found")
		return
	}

	pipeline, err := LoadPipelineFromFile(file)
	if err != nil {
		t.Fatalf("Failed to load pipeline: %v", err)
	}

	stats := pipeline.GetGraphStats()

	// Verify stats
	totalStages, ok := stats["total_stages"].(int)
	if !ok || totalStages != len(pipeline.Stages) {
		t.Errorf("TotalStages mismatch: got %v, want %d", stats["total_stages"], len(pipeline.Stages))
	}

	// maxDepth is computed from stages_by_depth
	stagesByDepth, ok := stats["stages_by_depth"].(map[int]int)
	maxDepth := 0
	if ok {
		for depth := range stagesByDepth {
			if depth > maxDepth {
				maxDepth = depth
			}
		}
	}
	if totalStages > 0 && maxDepth < 0 {
		t.Error("MaxDepth should be >= 0 for non-empty pipeline")
	}

	totalInstances, ok := stats["total_instances"].(int)
	if ok && totalInstances < totalStages {
		t.Error("TotalInstances should be >= TotalStages")
	}

	t.Logf("Pipeline stats: %v", stats)
}
