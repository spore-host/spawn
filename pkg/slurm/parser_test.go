package slurm

import (
	"testing"
	"time"
)

func TestParseTimeLimit(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected time.Duration
		wantErr  bool
	}{
		{
			name:     "HH:MM:SS format",
			input:    "02:30:45",
			expected: 2*time.Hour + 30*time.Minute + 45*time.Second,
		},
		{
			name:     "MM:SS format",
			input:    "30:15",
			expected: 30*time.Minute + 15*time.Second,
		},
		{
			name:     "DD-HH:MM:SS format",
			input:    "2-12:30:00",
			expected: 2*24*time.Hour + 12*time.Hour + 30*time.Minute,
		},
		{
			name:     "Single day",
			input:    "1-00:00:00",
			expected: 24 * time.Hour,
		},
		{
			name:     "One hour",
			input:    "01:00:00",
			expected: 1 * time.Hour,
		},
		{
			name:    "Invalid format",
			input:   "invalid",
			wantErr: true,
		},
		{
			name:    "Too many colons",
			input:   "1:2:3:4",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parseTimeLimit(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseTimeLimit() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && result != tt.expected {
				t.Errorf("parseTimeLimit() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestParseMemory(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected int
		wantErr  bool
	}{
		{
			name:     "Gigabytes",
			input:    "16GB",
			expected: 16 * 1024,
		},
		{
			name:     "Megabytes",
			input:    "512MB",
			expected: 512,
		},
		{
			name:     "Megabytes (M)",
			input:    "1024M",
			expected: 1024,
		},
		{
			name:     "Gigabytes (G)",
			input:    "32G",
			expected: 32 * 1024,
		},
		{
			name:     "Terabytes",
			input:    "1TB",
			expected: 1024 * 1024,
		},
		{
			name:     "Kilobytes",
			input:    "2048KB",
			expected: 2,
		},
		{
			name:     "No unit (assumed MB)",
			input:    "8192",
			expected: 8192,
		},
		{
			name:     "Lowercase",
			input:    "16gb",
			expected: 16 * 1024,
		},
		{
			name:    "Invalid format",
			input:   "invalid",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parseMemory(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseMemory() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && result != tt.expected {
				t.Errorf("parseMemory() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestParseArray(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected *ArraySpec
		wantErr  bool
	}{
		{
			name:  "Simple range",
			input: "1-100",
			expected: &ArraySpec{
				Start: 1,
				End:   100,
				Step:  1,
			},
		},
		{
			name:  "Range with step",
			input: "1-100:2",
			expected: &ArraySpec{
				Start: 1,
				End:   100,
				Step:  2,
			},
		},
		{
			name:  "Range with max running",
			input: "1-100%10",
			expected: &ArraySpec{
				Start:      1,
				End:        100,
				Step:       1,
				MaxRunning: 10,
			},
		},
		{
			name:  "Range with step and max running",
			input: "1-100:5%20",
			expected: &ArraySpec{
				Start:      1,
				End:        100,
				Step:       5,
				MaxRunning: 20,
			},
		},
		{
			name:  "Different start",
			input: "10-50",
			expected: &ArraySpec{
				Start: 10,
				End:   50,
				Step:  1,
			},
		},
		{
			name:    "No range",
			input:   "100",
			wantErr: true,
		},
		{
			name:    "Invalid format",
			input:   "a-b",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parseArray(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseArray() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}

			if result.Start != tt.expected.Start {
				t.Errorf("Start = %d, want %d", result.Start, tt.expected.Start)
			}
			if result.End != tt.expected.End {
				t.Errorf("End = %d, want %d", result.End, tt.expected.End)
			}
			if result.Step != tt.expected.Step {
				t.Errorf("Step = %d, want %d", result.Step, tt.expected.Step)
			}
			if result.MaxRunning != tt.expected.MaxRunning {
				t.Errorf("MaxRunning = %d, want %d", result.MaxRunning, tt.expected.MaxRunning)
			}
		})
	}
}

func TestParseGRES(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		expectGPUs int
		expectType string
		wantErr    bool
	}{
		{
			name:       "GPU count only",
			input:      "gpu:1",
			expectGPUs: 1,
			expectType: "",
		},
		{
			name:       "GPU with type",
			input:      "gpu:v100:2",
			expectGPUs: 2,
			expectType: "v100",
		},
		{
			name:       "GPU with a100 type",
			input:      "gpu:a100:4",
			expectGPUs: 4,
			expectType: "a100",
		},
		{
			name:       "GPU with t4 type",
			input:      "gpu:t4:1",
			expectGPUs: 1,
			expectType: "t4",
		},
		{
			name:       "Non-GPU GRES (ignored)",
			input:      "mem:1024",
			expectGPUs: 0,
			expectType: "",
		},
		{
			name:    "Invalid format",
			input:   "gpu",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gpus, gpuType, err := parseGRES(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseGRES() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}

			if gpus != tt.expectGPUs {
				t.Errorf("GPUs = %d, want %d", gpus, tt.expectGPUs)
			}
			if gpuType != tt.expectType {
				t.Errorf("GPUType = %q, want %q", gpuType, tt.expectType)
			}
		})
	}
}

func TestParseNodes(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected int
		wantErr  bool
	}{
		{
			name:     "Single node count",
			input:    "4",
			expected: 4,
		},
		{
			name:     "Node range (takes min)",
			input:    "2-4",
			expected: 2,
		},
		{
			name:     "Large node range",
			input:    "10-20",
			expected: 10,
		},
		{
			name:     "Single node",
			input:    "1",
			expected: 1,
		},
		{
			name:    "Invalid format",
			input:   "abc",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parseNodes(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseNodes() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && result != tt.expected {
				t.Errorf("parseNodes() = %d, want %d", result, tt.expected)
			}
		})
	}
}

func TestParseSlurmScript_ArrayJob(t *testing.T) {
	job, err := ParseSlurmScript("testdata/array_job.sbatch")
	if err != nil {
		t.Fatalf("ParseSlurmScript() error = %v", err)
	}

	if job.JobName != "array-test" {
		t.Errorf("JobName = %q, want %q", job.JobName, "array-test")
	}

	if job.Array == nil {
		t.Fatal("Array is nil, expected array job")
	}

	if job.Array.Start != 1 {
		t.Errorf("Array.Start = %d, want 1", job.Array.Start)
	}
	if job.Array.End != 100 {
		t.Errorf("Array.End = %d, want 100", job.Array.End)
	}

	expectedTime := 2 * time.Hour
	if job.TimeLimit != expectedTime {
		t.Errorf("TimeLimit = %v, want %v", job.TimeLimit, expectedTime)
	}

	if job.MemoryMB != 16*1024 {
		t.Errorf("MemoryMB = %d, want %d", job.MemoryMB, 16*1024)
	}

	if job.CPUsPerTask != 4 {
		t.Errorf("CPUsPerTask = %d, want 4", job.CPUsPerTask)
	}

	if !job.IsArrayJob() {
		t.Error("IsArrayJob() = false, want true")
	}

	if job.GetTotalTasks() != 100 {
		t.Errorf("GetTotalTasks() = %d, want 100", job.GetTotalTasks())
	}
}

func TestParseSlurmScript_GPUJob(t *testing.T) {
	job, err := ParseSlurmScript("testdata/gpu_job.sbatch")
	if err != nil {
		t.Fatalf("ParseSlurmScript() error = %v", err)
	}

	if job.JobName != "gpu-training" {
		t.Errorf("JobName = %q, want %q", job.JobName, "gpu-training")
	}

	if job.GPUs != 1 {
		t.Errorf("GPUs = %d, want 1", job.GPUs)
	}

	if !job.IsGPUJob() {
		t.Error("IsGPUJob() = false, want true")
	}

	// Check #SPAWN directives
	if !job.SpawnSpot {
		t.Error("SpawnSpot = false, want true")
	}

	if job.SpawnRegion != "us-east-1" {
		t.Errorf("SpawnRegion = %q, want %q", job.SpawnRegion, "us-east-1")
	}
}

func TestParseSlurmScript_MPIJob(t *testing.T) {
	job, err := ParseSlurmScript("testdata/mpi_job.sbatch")
	if err != nil {
		t.Fatalf("ParseSlurmScript() error = %v", err)
	}

	if job.JobName != "mpi-simulation" {
		t.Errorf("JobName = %q, want %q", job.JobName, "mpi-simulation")
	}

	if job.Nodes != 8 {
		t.Errorf("Nodes = %d, want 8", job.Nodes)
	}

	if job.TasksPerNode != 16 {
		t.Errorf("TasksPerNode = %d, want 16", job.TasksPerNode)
	}

	if !job.IsMPIJob() {
		t.Error("IsMPIJob() = false, want true")
	}

	if job.GetTotalTasks() != 128 {
		t.Errorf("GetTotalTasks() = %d, want 128 (8 nodes × 16 tasks)", job.GetTotalTasks())
	}
}

func TestGetTotalTasks(t *testing.T) {
	tests := []struct {
		name     string
		job      *SlurmJob
		expected int
	}{
		{
			name: "Array job",
			job: &SlurmJob{
				Array: &ArraySpec{
					Start: 1,
					End:   100,
					Step:  1,
				},
			},
			expected: 100,
		},
		{
			name: "Array job with step",
			job: &SlurmJob{
				Array: &ArraySpec{
					Start: 1,
					End:   100,
					Step:  2,
				},
			},
			expected: 50,
		},
		{
			name: "MPI job",
			job: &SlurmJob{
				Nodes:        4,
				TasksPerNode: 8,
			},
			expected: 32,
		},
		{
			name: "Single task",
			job: &SlurmJob{
				Nodes:        1,
				TasksPerNode: 1,
			},
			expected: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.job.GetTotalTasks()
			if result != tt.expected {
				t.Errorf("GetTotalTasks() = %d, want %d", result, tt.expected)
			}
		})
	}
}
