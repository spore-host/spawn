package cmd

import (
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
)

// TestSweepStartsFromTheCLIConfig is the spawn#697 gate.
//
// The sweep path used to build a two-field LaunchConfig — Region and
// InstanceType — and merge param rows onto an EMPTY struct, so every other CLI
// flag was dropped. 83 of 127. A flag worked only if someone hand-wrote a shim,
// which is why the same class was reported and patched six times (#525, #539
// twice, #549, #667, #673/#674/#675).
//
// This asserts the inversion at the seam that caused it: a value set on the base
// config must survive into a row's config. It covers the categories the issue
// called out as functional holes rather than cosmetic ones — storage (a sweep
// could not mount ANYTHING), networking (#667 fixed single launches only, so
// every row still landed in the VPC default security group), and --pre-stop,
// which syncs results before termination, so dropping it loses output.
func TestSweepStartsFromTheCLIConfig(t *testing.T) {
	base := aws.LaunchConfig{
		// Storage: a sweep could not mount any of this.
		EFSID:         "fs-0123456789abcdef0",
		EFSMountPoint: "/efs",
		FSxLustreID:   "fs-fedcba9876543210f",
		// Networking: #667's other half.
		SecurityGroupIDs: []string{"sg-0a1b2c3d", "sg-4e5f6a7b"},
		SubnetID:         "subnet-0123456789abcdef0",
		// Data safety.
		PreStop: "aws s3 sync /scratch s3://bucket/out",
		// Attribution.
		Tags: map[string]string{"project": "gchp", "owner": "lab"},
	}

	got, err := buildLaunchConfigFromParams(base, nil, map[string]interface{}{
		"instance_type": "c7g.4xlarge",
	}, "sweep-1", "test", 0, 1)
	if err != nil {
		t.Fatalf("buildLaunchConfigFromParams: %v", err)
	}

	checks := []struct {
		name string
		got  string
		want string
	}{
		{"--efs-id", got.EFSID, base.EFSID},
		{"--efs-mount-point", got.EFSMountPoint, base.EFSMountPoint},
		{"--fsx-id", got.FSxLustreID, base.FSxLustreID},
		{"--subnet-id", got.SubnetID, base.SubnetID},
		{"--pre-stop", got.PreStop, base.PreStop},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s did not reach the sweep row: got %q, want %q — this is the #697 "+
				"drop, and for storage it means the row cannot see its data at all",
				c.name, c.got, c.want)
		}
	}
	if len(got.SecurityGroupIDs) != 2 {
		t.Errorf("--security-group-ids did not reach the row (%v); every row would land in "+
			"the VPC default security group, which is #667's unfixed half",
			got.SecurityGroupIDs)
	}
	if got.Tags["project"] != "gchp" {
		t.Errorf("--tag did not reach the row: %v", got.Tags)
	}
	// And the row's own value still wins — the precedence #539 established.
	if got.InstanceType != "c7g.4xlarge" {
		t.Errorf("the row's instance_type did not override the base: %q", got.InstanceType)
	}
}

// TestSweepRowsDoNotShareTheBaseMaps is the bug the inversion could easily have
// introduced.
//
// `config := base` copies the struct, so without an explicit deep copy every row
// would share the base's Tags map and SecurityGroupIDs slice. One row adding a
// tag would silently add it to all of them — and sweep rows are launched
// CONCURRENTLY, so it would also be a data race.
func TestSweepRowsDoNotShareTheBaseMaps(t *testing.T) {
	base := aws.LaunchConfig{
		Tags:             map[string]string{"shared": "yes"},
		SecurityGroupIDs: []string{"sg-base"},
		AttachVolumes:    []aws.AttachVolumeSpec{{SnapshotID: "snap-1", MountPoint: "/data"}},
	}

	a, err := buildLaunchConfigFromParams(base, nil, map[string]interface{}{}, "s", "n", 0, 2)
	if err != nil {
		t.Fatalf("row a: %v", err)
	}
	b, err := buildLaunchConfigFromParams(base, nil, map[string]interface{}{}, "s", "n", 1, 2)
	if err != nil {
		t.Fatalf("row b: %v", err)
	}

	// Mutate row a the way a per-row tag would.
	a.Tags["only-a"] = "1"
	a.SecurityGroupIDs[0] = "sg-mutated"
	a.AttachVolumes[0].MountPoint = "/mutated"

	if _, leaked := b.Tags["only-a"]; leaked {
		t.Error("rows share the Tags map — a tag set on one row appears on every other, " +
			"and rows launch concurrently so this is also a race")
	}
	if b.SecurityGroupIDs[0] != "sg-base" {
		t.Errorf("rows share the SecurityGroupIDs slice: row b now has %q", b.SecurityGroupIDs[0])
	}
	if b.AttachVolumes[0].MountPoint != "/data" {
		t.Errorf("rows share the AttachVolumes slice: row b now mounts %q", b.AttachVolumes[0].MountPoint)
	}
	if _, leaked := base.Tags["only-a"]; leaked {
		t.Error("a row mutated the BASE config's Tags map, so the leak outlives the sweep")
	}
}

// TestSweepIdentityFieldsAreNotInherited: the per-sweep identity must come from
// the arguments, never from a base config that happens to carry stale values —
// e.g. a resumed sweep reusing a config.
func TestSweepIdentityFieldsAreNotInherited(t *testing.T) {
	base := aws.LaunchConfig{SweepID: "STALE", SweepName: "STALE", SweepIndex: 99, SweepSize: 99}

	got, err := buildLaunchConfigFromParams(base, nil, map[string]interface{}{}, "real-id", "real-name", 3, 7)
	if err != nil {
		t.Fatalf("buildLaunchConfigFromParams: %v", err)
	}
	if got.SweepID != "real-id" || got.SweepName != "real-name" || got.SweepIndex != 3 || got.SweepSize != 7 {
		t.Errorf("sweep identity leaked from the base: got id=%q name=%q index=%d size=%d",
			got.SweepID, got.SweepName, got.SweepIndex, got.SweepSize)
	}
}
