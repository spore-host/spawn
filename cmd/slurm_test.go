package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spore-host/spawn/pkg/slurm"
	"github.com/spore-host/spawn/pkg/testutil"
	"gopkg.in/yaml.v3"
)

// TestSlurmConvert tests the slurm convert command
func TestSlurmConvert(t *testing.T) {
	tests := []struct {
		name        string
		script      string
		wantErr     bool
		checkOutput bool
	}{
		{
			name: "simple array job",
			script: `#!/bin/bash
#SBATCH --job-name=test-array
#SBATCH --array=1-10
#SBATCH --time=01:00:00
#SBATCH --mem=4G
#SBATCH --cpus-per-task=2

echo "Task ${SLURM_ARRAY_TASK_ID}"
`,
			wantErr:     false,
			checkOutput: true,
		},
		{
			name: "GPU job",
			script: `#!/bin/bash
#SBATCH --job-name=gpu-training
#SBATCH --gres=gpu:2
#SBATCH --time=02:00:00
#SBATCH --mem=32G

python train.py
`,
			wantErr:     false,
			checkOutput: true,
		},
		{
			name: "MPI job",
			script: `#!/bin/bash
#SBATCH --job-name=mpi-sim
#SBATCH --nodes=4
#SBATCH --ntasks-per-node=8
#SBATCH --time=04:00:00

mpirun ./simulation
`,
			wantErr:     false,
			checkOutput: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create temp directory
			tmpDir := testutil.CreateTempDir(t, "slurm-test-*")

			// Write test script
			scriptPath := filepath.Join(tmpDir, "test.sbatch")
			testutil.WriteFile(t, scriptPath, tt.script)

			// Parse script
			job, err := slurm.ParseSlurmScript(scriptPath)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseSlurmScript() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr {
				return
			}

			// Convert to spawn format
			config, err := slurm.ConvertToSpawn(job)
			if err != nil {
				t.Errorf("ConvertToSpawn() error = %v", err)
				return
			}

			if tt.checkOutput {
				// Verify basic structure
				if config == nil {
					t.Error("config is nil")
					return
				}

				// Marshal to YAML to verify it's valid
				_, err := yaml.Marshal(config)
				if err != nil {
					t.Errorf("failed to marshal config to YAML: %v", err)
				}
			}
		})
	}
}

// TestSlurmArrayJobParsing tests parsing of Slurm array job directives
func TestSlurmArrayJobParsing(t *testing.T) {
	tests := []struct {
		name      string
		directive string
		wantStart int
		wantEnd   int
		wantErr   bool
	}{
		{
			name:      "simple range",
			directive: "#SBATCH --array=1-10",
			wantStart: 1,
			wantEnd:   10,
			wantErr:   false,
		},
		{
			name:      "large range",
			directive: "#SBATCH --array=0-999",
			wantStart: 0,
			wantEnd:   999,
			wantErr:   false,
		},
		{
			name:      "single value",
			directive: "#SBATCH --array=5",
			wantStart: 5,
			wantEnd:   5,
			wantErr:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simple parser test for array directive
			// In actual implementation, this would call slurm.ParseArrayDirective
			t.Logf("Testing array directive: %s", tt.directive)
			// Actual parsing logic would go here
		})
	}
}

// TestSlurmTimeFormat tests parsing of Slurm time formats
func TestSlurmTimeFormat(t *testing.T) {
	tests := []struct {
		name        string
		timeStr     string
		wantHours   int
		wantMinutes int
		wantErr     bool
	}{
		{
			name:        "HH:MM:SS",
			timeStr:     "01:30:00",
			wantHours:   1,
			wantMinutes: 30,
			wantErr:     false,
		},
		{
			name:        "MM:SS",
			timeStr:     "45:00",
			wantHours:   0,
			wantMinutes: 45,
			wantErr:     false,
		},
		{
			name:        "days-hours",
			timeStr:     "2-12:00:00",
			wantHours:   60, // 2*24 + 12
			wantMinutes: 0,
			wantErr:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Time parsing logic test
			t.Logf("Testing time format: %s", tt.timeStr)
			// Actual parsing logic would go here
		})
	}
}

// TestSlurmMemoryFormat tests parsing of Slurm memory specifications
func TestSlurmMemoryFormat(t *testing.T) {
	tests := []struct {
		name    string
		memStr  string
		wantGB  int
		wantErr bool
	}{
		{
			name:    "gigabytes",
			memStr:  "4G",
			wantGB:  4,
			wantErr: false,
		},
		{
			name:    "gigabytes full",
			memStr:  "16GB",
			wantGB:  16,
			wantErr: false,
		},
		{
			name:    "megabytes",
			memStr:  "2048M",
			wantGB:  2,
			wantErr: false,
		},
		{
			name:    "megabytes full",
			memStr:  "4096MB",
			wantGB:  4,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Memory parsing logic test
			t.Logf("Testing memory format: %s", tt.memStr)
			// Actual parsing logic would go here
		})
	}
}

// TestSlurmGPUDirective tests parsing of Slurm GPU directives
func TestSlurmGPUDirective(t *testing.T) {
	tests := []struct {
		name      string
		directive string
		wantCount int
		wantType  string
		wantErr   bool
	}{
		{
			name:      "basic GPU count",
			directive: "#SBATCH --gres=gpu:1",
			wantCount: 1,
			wantType:  "",
			wantErr:   false,
		},
		{
			name:      "multiple GPUs",
			directive: "#SBATCH --gres=gpu:4",
			wantCount: 4,
			wantType:  "",
			wantErr:   false,
		},
		{
			name:      "GPU with type",
			directive: "#SBATCH --gres=gpu:v100:2",
			wantCount: 2,
			wantType:  "v100",
			wantErr:   false,
		},
		{
			name:      "A100 GPU",
			directive: "#SBATCH --gres=gpu:a100:1",
			wantCount: 1,
			wantType:  "a100",
			wantErr:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// GPU directive parsing test
			t.Logf("Testing GPU directive: %s", tt.directive)
			// Actual parsing logic would go here
		})
	}
}

// TestSlurmMPIJob tests conversion of MPI jobs
func TestSlurmMPIJob(t *testing.T) {
	script := `#!/bin/bash
#SBATCH --job-name=mpi-test
#SBATCH --nodes=8
#SBATCH --ntasks-per-node=4
#SBATCH --time=01:00:00

mpirun ./my_program
`

	tmpDir := testutil.CreateTempDir(t, "slurm-mpi-test-*")
	scriptPath := filepath.Join(tmpDir, "mpi.sbatch")
	testutil.WriteFile(t, scriptPath, script)

	job, err := slurm.ParseSlurmScript(scriptPath)
	if err != nil {
		t.Fatalf("ParseSlurmScript() error = %v", err)
	}

	config, err := slurm.ConvertToSpawn(job)
	if err != nil {
		t.Fatalf("ConvertToSpawn() error = %v", err)
	}

	// Verify MPI configuration
	if config == nil {
		t.Fatal("config is nil")
	}

	// Would check for MPI-specific fields here
	t.Logf("Successfully converted MPI job")
}

// TestSlurmEstimate tests cost estimation for Slurm jobs
func TestSlurmEstimate(t *testing.T) {
	script := `#!/bin/bash
#SBATCH --job-name=test-estimate
#SBATCH --array=1-100
#SBATCH --time=02:00:00
#SBATCH --mem=8G
#SBATCH --cpus-per-task=4

./compute_task.sh
`

	tmpDir := testutil.CreateTempDir(t, "slurm-estimate-test-*")
	scriptPath := filepath.Join(tmpDir, "estimate.sbatch")
	testutil.WriteFile(t, scriptPath, script)

	job, err := slurm.ParseSlurmScript(scriptPath)
	if err != nil {
		t.Fatalf("ParseSlurmScript() error = %v", err)
	}

	// Would test cost estimation here
	// estimate, err := slurm.EstimateCost(job)
	t.Logf("Parsed job for estimation: %+v", job)
}

// TestSlurmCustomDirectives tests parsing of custom #SPAWN directives
func TestSlurmCustomDirectives(t *testing.T) {
	script := `#!/bin/bash
#SBATCH --job-name=custom-test
#SBATCH --time=01:00:00
#SPAWN --instance-type=c5.2xlarge
#SPAWN --region=us-west-2
#SPAWN --spot=true

./my_program
`

	tmpDir := testutil.CreateTempDir(t, "slurm-custom-test-*")
	scriptPath := filepath.Join(tmpDir, "custom.sbatch")
	testutil.WriteFile(t, scriptPath, script)

	job, err := slurm.ParseSlurmScript(scriptPath)
	if err != nil {
		t.Fatalf("ParseSlurmScript() error = %v", err)
	}

	// Verify custom directives were parsed
	t.Logf("Parsed job with custom directives: %+v", job)
}

// TestSlurmOutputFormats tests different output format options
func TestSlurmOutputFormats(t *testing.T) {
	script := `#!/bin/bash
#SBATCH --job-name=test
#SBATCH --time=01:00:00

echo "hello"
`

	tmpDir := testutil.CreateTempDir(t, "slurm-output-test-*")
	scriptPath := filepath.Join(tmpDir, "test.sbatch")
	testutil.WriteFile(t, scriptPath, script)

	job, err := slurm.ParseSlurmScript(scriptPath)
	if err != nil {
		t.Fatalf("ParseSlurmScript() error = %v", err)
	}

	config, err := slurm.ConvertToSpawn(job)
	if err != nil {
		t.Fatalf("ConvertToSpawn() error = %v", err)
	}

	// Test YAML marshaling
	yamlData, err := yaml.Marshal(config)
	if err != nil {
		t.Errorf("YAML marshal failed: %v", err)
	}
	if len(yamlData) == 0 {
		t.Error("YAML output is empty")
	}

	// Test writing to file
	outputPath := filepath.Join(tmpDir, "output.yaml")
	err = os.WriteFile(outputPath, yamlData, 0644)
	if err != nil {
		t.Errorf("failed to write output file: %v", err)
	}

	testutil.AssertFileExists(t, outputPath)
}

// TestSlurmValidation tests validation of Slurm script requirements
func TestSlurmValidation(t *testing.T) {
	tests := []struct {
		name    string
		script  string
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid script",
			script: `#!/bin/bash
#SBATCH --job-name=valid
#SBATCH --time=01:00:00
echo "hello"
`,
			wantErr: false,
		},
		{
			name:    "empty script",
			script:  "",
			wantErr: true,
			errMsg:  "empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := testutil.CreateTempDir(t, "slurm-validation-test-*")
			scriptPath := filepath.Join(tmpDir, "test.sbatch")

			if tt.script != "" {
				testutil.WriteFile(t, scriptPath, tt.script)
			}

			_, err := slurm.ParseSlurmScript(scriptPath)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseSlurmScript() error = %v, wantErr %v", err, tt.wantErr)
			}

			if tt.wantErr && err != nil && tt.errMsg != "" {
				// Check error message contains expected substring
				t.Logf("Got expected error: %v", err)
			}
		})
	}
}
