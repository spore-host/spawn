package cmd

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"io"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
)

// decodeUserData reverses encodeUserData (gzip + base64) for assertions.
func decodeUserData(t *testing.T, encoded string) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	out, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("gzip read: %v", err)
	}
	return string(out)
}

// TestBuildJobArrayMemberConfig verifies the shared per-index member builder:
// each member gets the job-array tags/size and a distinct index, and (with MPI
// enabled) the per-index MPI user-data — so rank 0 and rank 1 differ.
func TestBuildJobArrayMemberConfig(t *testing.T) {
	// Set the flag globals the builder reads, and restore them after.
	defer func(c int, n, names string, mpi bool) {
		count, jobArrayName, instanceNames, mpiEnabled = c, n, names, mpi
	}(count, jobArrayName, instanceNames, mpiEnabled)
	count = 3
	jobArrayName = "mpitest"
	instanceNames = ""
	mpiEnabled = true

	// buildJobArrayMemberConfig decodes the base UserData with plain base64 (it
	// re-gzips the combined result), so the base must be plain base64 here.
	base := &aws.LaunchConfig{
		Region:       "us-east-1",
		InstanceType: "c5n.18xlarge",
		UserData:     base64.StdEncoding.EncodeToString([]byte("#!/bin/bash\necho base\n")),
	}

	cfg0, err := buildJobArrayMemberConfig(base, "mpitest-abc", 0, nil)
	if err != nil {
		t.Fatalf("index 0: %v", err)
	}
	cfg1, err := buildJobArrayMemberConfig(base, "mpitest-abc", 1, nil)
	if err != nil {
		t.Fatalf("index 1: %v", err)
	}

	// Job-array identity fields.
	if cfg0.JobArrayID != "mpitest-abc" || cfg0.JobArrayName != "mpitest" {
		t.Errorf("job-array id/name not set: %+v", cfg0)
	}
	if cfg0.JobArraySize != 3 || cfg1.JobArraySize != 3 {
		t.Errorf("JobArraySize = %d/%d, want 3/3", cfg0.JobArraySize, cfg1.JobArraySize)
	}
	if cfg0.JobArrayIndex != 0 || cfg1.JobArrayIndex != 1 {
		t.Errorf("JobArrayIndex = %d/%d, want 0/1", cfg0.JobArrayIndex, cfg1.JobArrayIndex)
	}
	if cfg0.Name != "mpitest-0" || cfg1.Name != "mpitest-1" {
		t.Errorf("Name = %q/%q, want mpitest-0/mpitest-1", cfg0.Name, cfg1.Name)
	}

	// Per-index MPI user-data: the two must differ (rank 0 generates/uploads the
	// SSH key + runs mpirun; others wait), and both must carry the base script.
	ud0 := decodeUserData(t, cfg0.UserData)
	ud1 := decodeUserData(t, cfg1.UserData)
	if !strings.Contains(ud0, "echo base") || !strings.Contains(ud1, "echo base") {
		t.Error("member user-data lost the base script")
	}
	if ud0 == ud1 {
		t.Error("index 0 and index 1 user-data are identical — per-index MPI data not applied")
	}
}
