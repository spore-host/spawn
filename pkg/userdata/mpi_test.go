package userdata

import (
	"strings"
	"testing"
)

func TestGenerateMPIUserData_Basic(t *testing.T) {
	config := MPIConfig{
		Region:              "us-east-1",
		JobArrayID:          "test-job-array",
		JobArrayIndex:       0,
		JobArraySize:        4,
		MPIProcessesPerNode: 8,
		MPICommand:          "mpirun -np 32 ./my-program",
		SkipInstall:         false,
		EFAEnabled:          false,
		BinariesBucket:      "spawn-binaries-us-east-1",
	}

	script, err := GenerateMPIUserData(config)
	if err != nil {
		t.Fatalf("GenerateMPIUserData() error = %v", err)
	}

	// Check key sections are present
	expectedSections := []string{
		"# MPI Setup",
		"Installing OpenMPI",
		"export PATH=/usr/lib64/openmpi/bin",
		"export LD_LIBRARY_PATH=/usr/lib64/openmpi/lib",
		"export OMPI_MCA_plm_rsh_agent=ssh",
		"export OMPI_ALLOW_RUN_AS_ROOT=1",
		"aws s3 cp /root/.ssh/id_rsa.pub",
		"spawn-binaries-us-east-1",
		"mpi-keys/test-job-array",
	}

	for _, expected := range expectedSections {
		if !strings.Contains(script, expected) {
			t.Errorf("expected script to contain %q, but it didn't", expected)
		}
	}
}

func TestGenerateMPIUserData_SkipInstall(t *testing.T) {
	config := MPIConfig{
		Region:        "us-west-2",
		JobArrayID:    "test-job",
		JobArrayIndex: 0,
		JobArraySize:  2,
		SkipInstall:   true,
		EFAEnabled:    false,
	}

	script, err := GenerateMPIUserData(config)
	if err != nil {
		t.Fatalf("GenerateMPIUserData() error = %v", err)
	}

	// Should skip installation
	if strings.Contains(script, "Installing OpenMPI") {
		t.Error("expected script to skip MPI installation when SkipInstall=true")
	}

	if !strings.Contains(script, "Skipping MPI installation") {
		t.Error("expected script to contain skip message when SkipInstall=true")
	}

	// But should still configure environment
	if !strings.Contains(script, "export PATH=/usr/lib64/openmpi/bin") {
		t.Error("expected script to configure MPI environment even when skipping install")
	}
}

func TestGenerateMPIUserData_EFAEnabled(t *testing.T) {
	config := MPIConfig{
		Region:        "us-east-1",
		JobArrayID:    "efa-job",
		JobArrayIndex: 0,
		JobArraySize:  4,
		SkipInstall:   false,
		EFAEnabled:    true,
	}

	script, err := GenerateMPIUserData(config)
	if err != nil {
		t.Fatalf("GenerateMPIUserData() error = %v", err)
	}

	// Check EFA-specific sections
	expectedEFASections := []string{
		"Installing EFA driver",
		"curl -O https://efa-installer.amazonaws.com/aws-efa-installer-latest.tar.gz",
		"./efa_installer.sh -y -g",
		"export FI_PROVIDER=efa",
		"export FI_EFA_USE_DEVICE_RDMA=1",
	}

	for _, expected := range expectedEFASections {
		if !strings.Contains(script, expected) {
			t.Errorf("expected EFA-enabled script to contain %q, but it didn't", expected)
		}
	}
}

func TestGenerateMPIUserData_NoEFA(t *testing.T) {
	config := MPIConfig{
		Region:        "us-east-1",
		JobArrayID:    "regular-job",
		JobArrayIndex: 0,
		JobArraySize:  2,
		SkipInstall:   false,
		EFAEnabled:    false,
	}

	script, err := GenerateMPIUserData(config)
	if err != nil {
		t.Fatalf("GenerateMPIUserData() error = %v", err)
	}

	// Should NOT contain EFA sections
	if strings.Contains(script, "Installing EFA driver") {
		t.Error("expected non-EFA script to not contain EFA driver installation")
	}

	if strings.Contains(script, "FI_PROVIDER=efa") {
		t.Error("expected non-EFA script to not contain EFA configuration")
	}
}

func TestGenerateMPIUserData_HeadNode(t *testing.T) {
	config := MPIConfig{
		Region:        "us-east-1",
		JobArrayID:    "test-job",
		JobArrayIndex: 0, // Head node
		JobArraySize:  4,
		MPICommand:    "mpirun -np 16 ./app",
		SkipInstall:   false,
		EFAEnabled:    false,
	}

	script, err := GenerateMPIUserData(config)
	if err != nil {
		t.Fatalf("GenerateMPIUserData() error = %v", err)
	}

	// Head node should generate SSH key
	if !strings.Contains(script, "ssh-keygen -t rsa -N \"\" -f /root/.ssh/id_rsa -q") {
		t.Error("expected head node (index 0) to generate SSH key")
	}

	// Head node should run mpirun command
	if !strings.Contains(script, "mpirun -np 16 ./app") {
		t.Error("expected head node to contain mpirun command")
	}
}

func TestGenerateMPIUserData_WorkerNode(t *testing.T) {
	config := MPIConfig{
		Region:         "us-east-1",
		JobArrayID:     "test-job",
		JobArrayIndex:  2, // Worker node (not 0)
		JobArraySize:   4,
		MPICommand:     "mpirun -np 16 ./app",
		SkipInstall:    false,
		EFAEnabled:     false,
		BinariesBucket: "spawn-binaries-us-east-1",
	}

	script, err := GenerateMPIUserData(config)
	if err != nil {
		t.Fatalf("GenerateMPIUserData() error = %v", err)
	}

	// Check that the conditional check uses the correct index (2, not 0)
	// The template generates bash if/else, so both branches will be in the script
	if !strings.Contains(script, `if [ "2" -eq 0 ]; then`) {
		t.Error("expected script to have conditional with JobArrayIndex value 2")
	}

	// Worker node should download key from S3 in the else branch
	if !strings.Contains(script, "aws s3 cp s3://spawn-binaries-us-east-1/mpi-keys/test-job/id_rsa.pub /tmp/key.pub") {
		t.Error("expected worker node to download SSH key from S3")
	}

	// Worker node should wait for key with retry loop
	if !strings.Contains(script, "for i in {1..60}; do") {
		t.Error("expected worker node to have retry loop for key download")
	}
}

func TestGenerateMPIUserData_MPIProcessesPerNode(t *testing.T) {
	config := MPIConfig{
		Region:              "us-east-1",
		JobArrayID:          "test-job",
		JobArrayIndex:       0,
		JobArraySize:        4,
		MPIProcessesPerNode: 16,
		SkipInstall:         false,
		EFAEnabled:          false,
	}

	script, err := GenerateMPIUserData(config)
	if err != nil {
		t.Fatalf("GenerateMPIUserData() error = %v", err)
	}

	// Should set SLOTS variable to specified value
	if !strings.Contains(script, "SLOTS=16") {
		t.Error("expected script to set SLOTS to MPIProcessesPerNode value")
	}
}

func TestGenerateMPIUserData_DefaultProcessesPerNode(t *testing.T) {
	config := MPIConfig{
		Region:              "us-east-1",
		JobArrayID:          "test-job",
		JobArrayIndex:       0,
		JobArraySize:        2,
		MPIProcessesPerNode: 0, // Default - use nproc
		SkipInstall:         false,
		EFAEnabled:          false,
	}

	script, err := GenerateMPIUserData(config)
	if err != nil {
		t.Fatalf("GenerateMPIUserData() error = %v", err)
	}

	// Should use nproc when MPIProcessesPerNode is 0
	if !strings.Contains(script, "SLOTS=$(nproc)") {
		t.Error("expected script to use nproc when MPIProcessesPerNode=0")
	}
}

func TestGenerateMPIUserData_RegionSubstitution(t *testing.T) {
	regions := []string{"us-east-1", "us-west-2", "eu-west-1", "ap-northeast-1"}

	for _, region := range regions {
		config := MPIConfig{
			Region:         region,
			JobArrayID:     "test-job",
			JobArrayIndex:  0,
			JobArraySize:   2,
			SkipInstall:    false,
			EFAEnabled:     false,
			BinariesBucket: "spawn-binaries-" + region,
		}

		script, err := GenerateMPIUserData(config)
		if err != nil {
			t.Fatalf("GenerateMPIUserData() error = %v for region %s", err, region)
		}

		expectedBucket := "spawn-binaries-" + region
		if !strings.Contains(script, expectedBucket) {
			t.Errorf("expected script to contain region-specific bucket %q for region %s", expectedBucket, region)
		}
	}
}

func TestGenerateMPIUserData_SSHConfig(t *testing.T) {
	config := MPIConfig{
		Region:        "us-east-1",
		JobArrayID:    "test-job",
		JobArrayIndex: 0,
		JobArraySize:  2,
		SkipInstall:   false,
		EFAEnabled:    false,
	}

	script, err := GenerateMPIUserData(config)
	if err != nil {
		t.Fatalf("GenerateMPIUserData() error = %v", err)
	}

	// Check SSH configuration
	sshConfigSections := []string{
		"StrictHostKeyChecking no",
		"UserKnownHostsFile=/dev/null",
		"chmod 700 /root/.ssh",
		"chmod 600 /root/.ssh/authorized_keys",
		"chmod 600 /root/.ssh/config",
	}

	for _, expected := range sshConfigSections {
		if !strings.Contains(script, expected) {
			t.Errorf("expected script to contain SSH config section %q", expected)
		}
	}
}

func TestGenerateMPIUserData_NoMPICommand(t *testing.T) {
	config := MPIConfig{
		Region:        "us-east-1",
		JobArrayID:    "test-job",
		JobArrayIndex: 0,
		JobArraySize:  2,
		MPICommand:    "", // No MPI command
		SkipInstall:   false,
		EFAEnabled:    false,
	}

	script, err := GenerateMPIUserData(config)
	if err != nil {
		t.Fatalf("GenerateMPIUserData() error = %v", err)
	}

	// Should still generate SSH setup and MPI environment
	if !strings.Contains(script, "export PATH=/usr/lib64/openmpi/bin") {
		t.Error("expected script to contain MPI environment even without MPI command")
	}

	// But should not have a mpirun command in the script
	// (empty command should result in empty command in template)
	lines := strings.Split(script, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "mpirun") && !strings.Contains(trimmed, "{{") {
			t.Errorf("expected no mpirun command when MPICommand is empty, found: %s", line)
		}
	}
}
