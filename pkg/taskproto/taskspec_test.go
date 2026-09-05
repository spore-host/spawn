package taskproto

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goodSpec = `{
  "task_id": "align-42",
  "command": ["bwa", "mem", "ref.fa"],
  "resources": {"cpu": 16, "memory_gib": 64, "architecture": "x86_64", "purchase": "spot"},
  "inputs":  [{"source": "s3://b/in.fq", "destination": "/work/in.fq"}],
  "outputs": [{"source": "/work/out.bam", "destination": "s3://b/out.bam"}],
  "lifecycle": {"ttl": "4h", "on_complete": "terminate"}
}`

// TestParseSpec_RejectsUnknownFields covers #556's exact repro: a spec that
// guessed the wrong field names ("cpus"/"memory_mb"/"disk_gb" instead of the
// real "cpu"/"memory_gib"/resources.disk_gib) used to parse cleanly and
// validate clean too, silently sizing a t4g.nano for a request that meant 8
// vCPU / 16 GiB. ParseSpec must now reject it with an error naming the bad
// field(s), rather than discarding them.
func TestParseSpec_RejectsUnknownFields(t *testing.T) {
	badSpec := `{
	  "task_id": "align-42",
	  "command": ["bwa", "mem", "ref.fa"],
	  "resources": {"cpus": 8, "memory_mb": 16384, "disk_gb": 20},
	  "lifecycle": {"ttl": "4h"}
	}`
	_, err := ParseSpec([]byte(badSpec))
	if err == nil {
		t.Fatal("ParseSpec: expected an error for unknown fields (cpus/memory_mb/disk_gb), got nil")
	}
	// json.Decoder's DisallowUnknownFields error names the offending field.
	if !strings.Contains(err.Error(), "cpus") {
		t.Errorf("error %q does not name the bad field %q", err.Error(), "cpus")
	}
}

// TestParseSpec_KnownFieldsStillParse is the control for the above: the real
// field names, including the two new ones from #556 (disk_gib, cost_limit),
// must still parse and validate cleanly under DisallowUnknownFields.
func TestParseSpec_KnownFieldsStillParse(t *testing.T) {
	spec := `{
	  "task_id": "align-42",
	  "command": ["bwa", "mem", "ref.fa"],
	  "resources": {"cpu": 8, "memory_gib": 16, "disk_gib": 20},
	  "lifecycle": {"ttl": "4h", "cost_limit": 5}
	}`
	got, err := ParseSpec([]byte(spec))
	if err != nil {
		t.Fatalf("ParseSpec: unexpected error for valid known fields: %v", err)
	}
	if got.Resources.DiskGiB != 20 {
		t.Errorf("Resources.DiskGiB = %d, want 20", got.Resources.DiskGiB)
	}
	if got.Lifecycle.CostLimit != 5 {
		t.Errorf("Lifecycle.CostLimit = %v, want 5", got.Lifecycle.CostLimit)
	}
}

func TestParseSpecFile_Good(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "task.json")
	if err := os.WriteFile(p, []byte(goodSpec), 0o644); err != nil {
		t.Fatal(err)
	}
	spec, err := ParseSpecFile(p)
	if err != nil {
		t.Fatalf("ParseSpecFile: %v", err)
	}
	if spec.TaskID != "align-42" || len(spec.Command) != 3 {
		t.Errorf("unexpected parse: %+v", spec)
	}
	if spec.EffectiveOnComplete() != "terminate" {
		t.Errorf("EffectiveOnComplete = %q", spec.EffectiveOnComplete())
	}
}

func TestValidate_Failures(t *testing.T) {
	cases := []struct {
		name string
		spec TaskSpec
		want string
	}{
		{"no task_id", TaskSpec{Command: []string{"x"}, Lifecycle: Lifecycle{TTL: "1h"}}, "task_id is required"},
		{"no command", TaskSpec{TaskID: "t", Lifecycle: Lifecycle{TTL: "1h"}}, "command is required"},
		{"no ttl", TaskSpec{TaskID: "t", Command: []string{"x"}}, "lifecycle.ttl is required"},
		{"bad ttl", TaskSpec{TaskID: "t", Command: []string{"x"}, Lifecycle: Lifecycle{TTL: "soon"}}, "not a valid duration"},
		{"bad arch", TaskSpec{TaskID: "t", Command: []string{"x"}, Lifecycle: Lifecycle{TTL: "1h"}, Resources: ResourceRequest{Architecture: "sparc"}}, "architecture"},
		{"bad purchase", TaskSpec{TaskID: "t", Command: []string{"x"}, Lifecycle: Lifecycle{TTL: "1h"}, Resources: ResourceRequest{Purchase: "layaway"}}, "purchase"},
		{"bad on_complete", TaskSpec{TaskID: "t", Command: []string{"x"}, Lifecycle: Lifecycle{TTL: "1h", OnComplete: "explode"}}, "on_complete"},
		{"headroom range", TaskSpec{TaskID: "t", Command: []string{"x"}, Lifecycle: Lifecycle{TTL: "1h"}, Resources: ResourceRequest{MemoryHeadroomPercent: 250}}, "memory_headroom_percent"},
		{"negative disk_gib", TaskSpec{TaskID: "t", Command: []string{"x"}, Lifecycle: Lifecycle{TTL: "1h"}, Resources: ResourceRequest{DiskGiB: -5}}, "disk_gib"},
		{"negative cost_limit", TaskSpec{TaskID: "t", Command: []string{"x"}, Lifecycle: Lifecycle{TTL: "1h", CostLimit: -1}}, "cost_limit"},
		{"bad input manifest", TaskSpec{TaskID: "t", Command: []string{"x"}, Lifecycle: Lifecycle{TTL: "1h"}, Inputs: []Manifest{{Source: "s3://b/x"}}}, "inputs[0]"},
		{"bad env key", TaskSpec{TaskID: "t", Command: []string{"x"}, Lifecycle: Lifecycle{TTL: "1h"}, Env: map[string]string{"BAD-KEY": "v"}}, "env key"},
		{"env key with space", TaskSpec{TaskID: "t", Command: []string{"x"}, Lifecycle: Lifecycle{TTL: "1h"}, Env: map[string]string{"a b": "v"}}, "env key"},
		{"bad s3_read_write", TaskSpec{TaskID: "t", Command: []string{"x"}, Lifecycle: Lifecycle{TTL: "1h"}, Resources: ResourceRequest{S3ReadWrite: []string{"my-bucket"}}}, "s3_read_write"},
		{"bad volume snapshot", TaskSpec{TaskID: "t", Command: []string{"x"}, Lifecycle: Lifecycle{TTL: "1h"}, Placement: Placement{Volumes: []VolumeRef{{Snapshot: "vol-x", MountPath: "/ref"}}}}, "snapshot"},
		{"volume no mount", TaskSpec{TaskID: "t", Command: []string{"x"}, Lifecycle: Lifecycle{TTL: "1h"}, Placement: Placement{Volumes: []VolumeRef{{Snapshot: "snap-abc"}}}}, "mount_path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.spec.Validate()
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q missing %q", err.Error(), tc.want)
			}
		})
	}
}

// TestValidate_RejectsNonS3ManifestSide guards spawn#570 sub-issue 2: the
// wrapper unconditionally emits `aws s3 cp` for every manifest entry in both
// directions, so a manifest entry whose S3-side value isn't actually an
// s3://... URI (e.g. a path on a mounted placement EFS, which passed
// Validate() and --dry-run before this fix) used to fail only at runtime,
// after a paid boot. Validate must reject it immediately, naming which side
// and index is wrong.
func TestValidate_RejectsNonS3ManifestSide(t *testing.T) {
	cases := []struct {
		name string
		spec TaskSpec
		want string
	}{
		{
			"input source not s3",
			TaskSpec{TaskID: "t", Command: []string{"x"}, Lifecycle: Lifecycle{TTL: "1h"},
				Inputs: []Manifest{{Source: "/efs/refs/GRCh38.fa", Destination: "/tmp/GRCh38.fa"}}},
			"inputs[0].source",
		},
		{
			"output destination not s3",
			TaskSpec{TaskID: "t", Command: []string{"x"}, Lifecycle: Lifecycle{TTL: "1h"},
				Outputs: []Manifest{{Source: "/work/out.bam", Destination: "/efs/results/out.bam"}}},
			"outputs[0].destination",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.spec.Validate()
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q missing %q", err.Error(), tc.want)
			}
		})
	}
}

// TestValidate_S3OnlyManifestsStillValid is the regression control for the
// above: a spec whose manifests are s3:// on the correct side (the normal,
// already-working case) must still validate cleanly.
func TestValidate_S3OnlyManifestsStillValid(t *testing.T) {
	s := TaskSpec{
		TaskID:    "t",
		Command:   []string{"echo", "hi"},
		Lifecycle: Lifecycle{TTL: "2h"},
		Inputs:    []Manifest{{Source: "s3://in-bucket/ref.fa", Destination: "/data/ref.fa"}},
		Outputs:   []Manifest{{Source: "/work/out.bam", Destination: "s3://out-bucket/out.bam"}},
	}
	if err := s.Validate(); err != nil {
		t.Errorf("valid s3-only manifest spec rejected: %v", err)
	}
}

// TestPlacement_EffectiveMountPoints covers spawn#570 sub-issue 3: an unset
// EFSMountPoint/FSxMountPoint resolves to the historical hardcoded default
// ("/efs"/"/fsx"), and a spec-supplied override is honored verbatim — the
// same resolution wrapper.go's containerMountDirs and cmd/task.go's
// taskStorageScript both now call, so the two can't independently hardcode
// diverging defaults.
func TestPlacement_EffectiveMountPoints(t *testing.T) {
	def := Placement{EFSID: "fs-1", FSxLustreID: "fs-2"}
	if got := def.EffectiveEFSMountPoint(); got != "/efs" {
		t.Errorf("EffectiveEFSMountPoint() = %q, want default %q", got, "/efs")
	}
	if got := def.EffectiveFSxMountPoint(); got != "/fsx" {
		t.Errorf("EffectiveFSxMountPoint() = %q, want default %q", got, "/fsx")
	}

	override := Placement{EFSID: "fs-1", EFSMountPoint: "/mnt/refdata", FSxLustreID: "fs-2", FSxMountPoint: "/mnt/scratch"}
	if got := override.EffectiveEFSMountPoint(); got != "/mnt/refdata" {
		t.Errorf("EffectiveEFSMountPoint() = %q, want override %q", got, "/mnt/refdata")
	}
	if got := override.EffectiveFSxMountPoint(); got != "/mnt/scratch" {
		t.Errorf("EffectiveFSxMountPoint() = %q, want override %q", got, "/mnt/scratch")
	}
}

func TestValidate_Good(t *testing.T) {
	s := TaskSpec{TaskID: "t", Command: []string{"echo", "hi"}, Lifecycle: Lifecycle{TTL: "2h"}}
	if err := s.Validate(); err != nil {
		t.Errorf("valid minimal spec rejected: %v", err)
	}
}

func TestValidate_PlacementAndS3ReadWrite(t *testing.T) {
	s := TaskSpec{
		TaskID:    "t",
		Command:   []string{"echo", "hi"},
		Lifecycle: Lifecycle{TTL: "2h"},
		Resources: ResourceRequest{S3ReadWrite: []string{"s3://storage-bucket", "s3://other/prefix"}},
		Placement: Placement{
			AMI:              "ami-123",
			AvailabilityZone: "us-east-1a",
			Volumes:          []VolumeRef{{Snapshot: "snap-abc", MountPath: "/ref", ReadOnly: true}},
			FSxLustreID:      "fs-abc",
			EFSID:            "fs-def",
		},
	}
	if err := s.Validate(); err != nil {
		t.Errorf("valid placement/s3_read_write spec rejected: %v", err)
	}
}
