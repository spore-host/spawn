package cmd

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/spore-host/cohort"
	"github.com/spore-host/spawn/pkg/aws"
)

// TestOrderAZs covers the AZ-fallback ordering: the operator-selected AZ is put
// first when present, the rest keep their (sorted) order, and an absent/empty
// preference is a no-op.
func TestOrderAZs(t *testing.T) {
	zones := []string{"us-east-1a", "us-east-1b", "us-east-1c"}
	tests := []struct {
		name      string
		preferred string
		want      []string
	}{
		{"empty preference", "", zones},
		{"preferred first", "us-east-1b", []string{"us-east-1b", "us-east-1a", "us-east-1c"}},
		{"preferred already first", "us-east-1a", zones},
		{"preferred not present", "us-east-1z", zones},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := orderAZs(zones, tt.preferred); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("orderAZs(%v, %q) = %v, want %v", zones, tt.preferred, got, tt.want)
			}
		})
	}
}

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

// TestBuildAZChain_PlacementGroupGate covers the Stage-1 guard: when a cluster
// placement group is set, buildAZChain must return a single-rung chain (no AZ
// fallback) and make no AWS call, since a pre-created PG binds to one AZ. Passing
// a nil *aws.Client proves the PG-set path returns before touching AWS.
func TestBuildAZChain_PlacementGroupGate(t *testing.T) {
	base := &aws.LaunchConfig{
		InstanceType:     "p5.48xlarge",
		Region:           "us-east-1",
		AvailabilityZone: "us-east-1a",
		PlacementGroup:   "spawn-mpi-test",
	}
	rung, chain := buildAZChain(context.Background(), nil, base, cohort.CapacityOnDemand)
	if len(chain) != 1 {
		t.Fatalf("PG set → want single-rung chain, got %d rungs", len(chain))
	}
	if rung.AvailZone != "us-east-1a" || chain[0] != rung {
		t.Errorf("rung = %+v, want single rung in us-east-1a", rung)
	}
	if rung.CapacityModel != cohort.CapacityOnDemand {
		t.Errorf("capacity model = %v, want OnDemand", rung.CapacityModel)
	}
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
