package cmd

import (
	"strings"
	"testing"
)

// TestBuildLaunchConfig_SecurityGroupIDs is spawn#667.
//
// --security-group-ids was parsed into the sgIDs global and then never copied
// onto the LaunchConfig. The only assignment lived on the batch-queue path, so
// every ordinary `spawn launch --security-group-ids sg-...` silently got the
// VPC's DEFAULT security group instead, with nothing warning.
//
// The downstream breakage is worse than "the wrong SG": --efs-id mounts hang
// (the mount target sits in another SG, so 2049 is blocked and a `hard` NFS
// mount blocks forever rather than failing), EnsureLustrePorts iterates an empty
// slice so FSx never opens 988, and SSH from a restricted CIDR is refused.
func TestBuildLaunchConfig_SecurityGroupIDs(t *testing.T) {
	prevSGs, prevType := sgIDs, instanceType
	t.Cleanup(func() { sgIDs, instanceType = prevSGs, prevType })
	instanceType = "c7g.4xlarge"

	t.Run("flag reaches the config", func(t *testing.T) {
		sgIDs = []string{"sg-0123456789abcdef0", "sg-fedcba9876543210f"}
		cfg, err := buildLaunchConfig(nil)
		if err != nil {
			t.Fatalf("buildLaunchConfig: %v", err)
		}
		if len(cfg.SecurityGroupIDs) != 2 {
			t.Fatalf("SecurityGroupIDs = %v, want the 2 groups from --security-group-ids; "+
				"a dropped SG means the instance silently joins the VPC default group",
				cfg.SecurityGroupIDs)
		}
		for i, want := range sgIDs {
			if cfg.SecurityGroupIDs[i] != want {
				t.Errorf("SecurityGroupIDs[%d] = %q, want %q", i, cfg.SecurityGroupIDs[i], want)
			}
		}
	})

	t.Run("unset leaves it empty so spawn can auto-create", func(t *testing.T) {
		sgIDs = nil
		cfg, err := buildLaunchConfig(nil)
		if err != nil {
			t.Fatalf("buildLaunchConfig: %v", err)
		}
		if len(cfg.SecurityGroupIDs) != 0 {
			t.Errorf("SecurityGroupIDs = %v, want empty: an unset flag must leave the "+
				"auto-create path (Windows/MPI managed SG) reachable", cfg.SecurityGroupIDs)
		}
	})
}

// TestBuildLaunchConfig_SubnetID covers the identical gap on --subnet-id, found
// while fixing #667: it too was assigned only on the batch-queue path.
//
// This one also decides the AZ, so dropping it sends the instance to an
// arbitrary subnet — which is the other half of why an --efs-id mount hangs: a
// one-zone mount target is only reachable from its own AZ.
func TestBuildLaunchConfig_SubnetID(t *testing.T) {
	prevSubnet, prevType := subnetID, instanceType
	t.Cleanup(func() { subnetID, instanceType = prevSubnet, prevType })
	instanceType = "c7g.4xlarge"

	subnetID = "subnet-0a1b2c3d4e5f60718"
	cfg, err := buildLaunchConfig(nil)
	if err != nil {
		t.Fatalf("buildLaunchConfig: %v", err)
	}
	if cfg.SubnetID != subnetID {
		t.Errorf("SubnetID = %q, want %q from --subnet-id", cfg.SubnetID, subnetID)
	}
}

// TestMergeManagedSecurityGroup is the half of #667 that the one-line copy does
// NOT fix, and the reason the issue says the fix must not be undone.
//
// ensureSecurityGroup overwrote config.SecurityGroupIDs outright when it created
// a managed MPI or Windows group, so under --mpi the user's groups were dropped
// a second time, after being copied correctly. There was no flag-level
// workaround at all: you could not add a group to an MPI launch.
func TestMergeManagedSecurityGroup(t *testing.T) {
	tests := []struct {
		name     string
		existing []string
		managed  string
		want     []string
	}{{
		name:     "no user groups yields just the managed one",
		existing: nil,
		managed:  "sg-managed",
		want:     []string{"sg-managed"},
	}, {
		name:     "user groups are kept and the managed group is added",
		existing: []string{"sg-user1", "sg-user2"},
		managed:  "sg-managed",
		want:     []string{"sg-user1", "sg-user2", "sg-managed"},
	}, {
		name:     "an already-present managed group is not duplicated",
		existing: []string{"sg-user1", "sg-managed"},
		managed:  "sg-managed",
		want:     []string{"sg-user1", "sg-managed"},
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeManagedSecurityGroup(tc.existing, tc.managed)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("mergeManagedSecurityGroup(%v, %q) = %v, want %v",
					tc.existing, tc.managed, got, tc.want)
			}
		})
	}
}
