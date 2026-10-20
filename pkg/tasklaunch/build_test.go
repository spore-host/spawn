package tasklaunch

import (
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/taskproto"
)

// TestTaskAMIPlan_GPUResolvesDLAMI asserts the classifier the dry-run and the real
// launch (via GetRecommendedAMI) share agrees on GPU-ness per instance type: a GPU
// family maps to the DLAMI label with the right arch, a CPU family to the default,
// and an explicit AMI wins regardless (spawn#601).
func TestTaskAMIPlan_GPUResolvesDLAMI(t *testing.T) {
	cases := []struct {
		name         string
		instanceType string
		placementAMI string
		wantContains string
		wantAbsent   string
	}{
		{"g5 x86 GPU", "g5.2xlarge", "", "AL2023 GPU DLAMI — NVIDIA driver, x86_64", "AL2023 default"},
		{"g5g arm GPU", "g5g.xlarge", "", "AL2023 GPU DLAMI — NVIDIA driver, arm64", "AL2023 default"},
		{"c7i CPU", "c7i.4xlarge", "", "AL2023 default — x86_64", "GPU DLAMI"},
		{"c8g arm CPU", "c8g.4xlarge", "", "AL2023 default — arm64", "GPU DLAMI"},
		{"explicit wins over GPU", "g5.2xlarge", "ami-pinned", "ami-pinned", "GPU DLAMI"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := &taskproto.TaskSpec{Placement: taskproto.Placement{AMI: tc.placementAMI}}
			got := taskAMIPlan(spec, tc.instanceType)
			if !strings.Contains(got, tc.wantContains) {
				t.Errorf("taskAMIPlan(%q) = %q, want contains %q", tc.instanceType, got, tc.wantContains)
			}
			if tc.wantAbsent != "" && strings.Contains(got, tc.wantAbsent) {
				t.Errorf("taskAMIPlan(%q) = %q, must not contain %q", tc.instanceType, got, tc.wantAbsent)
			}
		})
	}
}

func TestTaskLaunchConfig(t *testing.T) {
	spec := &taskproto.TaskSpec{
		TaskID:    "align-42",
		Command:   []string{"bwa", "mem"},
		Resources: taskproto.ResourceRequest{Purchase: taskproto.PurchaseSpot},
		Lifecycle: taskproto.Lifecycle{TTL: "4h"}, // OnComplete empty → defaults to terminate
	}
	sized := &taskproto.SizeResult{InstanceType: "c7i.4xlarge"}
	cfg := taskLaunchConfig(spec, sized, "us-east-1", "spawn-task-profile", "#!/bin/bash\ntrue\n")

	if cfg.InstanceType != "c7i.4xlarge" {
		t.Errorf("InstanceType = %q", cfg.InstanceType)
	}
	if !cfg.Spot {
		t.Error("Spot should be true for purchase=spot")
	}
	if cfg.TTL != "4h" {
		t.Errorf("TTL = %q", cfg.TTL)
	}
	if cfg.OnComplete != "terminate" {
		t.Errorf("OnComplete = %q, want terminate (default)", cfg.OnComplete)
	}
	if cfg.Name != "align-42" || cfg.Tags["spawn:task-id"] != "align-42" {
		t.Errorf("task-id not stamped: Name=%q tag=%q", cfg.Name, cfg.Tags["spawn:task-id"])
	}
	if cfg.IamInstanceProfile != "spawn-task-profile" {
		t.Errorf("IamInstanceProfile = %q", cfg.IamInstanceProfile)
	}
	if !strings.Contains(cfg.JobArrayCommand, "#!/bin/bash") {
		t.Errorf("JobArrayCommand should carry the wrapper, got %q", cfg.JobArrayCommand)
	}
	// AMI/UserData must be left empty for Provision to fill.
	if cfg.AMI != "" || cfg.UserData != "" {
		t.Errorf("AMI/UserData should be empty for Provision, got AMI=%q UserData=%q", cfg.AMI, cfg.UserData)
	}
	// Completion file must be tagged explicitly (spawn#406) so on-complete
	// teardown doesn't depend on provider-side defaulting / tag-visibility timing.
	if cfg.CompletionFile != "/tmp/SPAWN_COMPLETE" {
		t.Errorf("CompletionFile = %q, want /tmp/SPAWN_COMPLETE", cfg.CompletionFile)
	}
	if cfg.CompletionDelay == "" {
		t.Error("CompletionDelay should be set so on-complete has a bounded grace")
	}
}

func TestTaskLaunchConfig_OnDemandDefault(t *testing.T) {
	spec := &taskproto.TaskSpec{TaskID: "t", Command: []string{"x"}, Lifecycle: taskproto.Lifecycle{TTL: "1h"}}
	cfg := taskLaunchConfig(spec, &taskproto.SizeResult{InstanceType: "m7i.large"}, "us-east-1", "p", "w")
	if cfg.Spot {
		t.Error("Spot should default to false when purchase is unset")
	}
}

// TestTaskLaunchConfig_DiskGiB covers #556 item 2: resources.disk_gib must wire
// into the SAME aws.LaunchConfig field (RootVolumeSizeGiB) that --volume-size
// populates on the regular launch path, so a task and a plain launch converge on
// identical downstream EBS sizing.
func TestTaskLaunchConfig_DiskGiB(t *testing.T) {
	spec := &taskproto.TaskSpec{
		TaskID:    "t",
		Command:   []string{"x"},
		Resources: taskproto.ResourceRequest{DiskGiB: 100},
		Lifecycle: taskproto.Lifecycle{TTL: "1h"},
	}
	cfg := taskLaunchConfig(spec, &taskproto.SizeResult{InstanceType: "m7i.large"}, "us-east-1", "p", "w")
	if cfg.RootVolumeSizeGiB != 100 {
		t.Errorf("RootVolumeSizeGiB = %d, want 100 (from resources.disk_gib)", cfg.RootVolumeSizeGiB)
	}
}

// TestTaskLaunchConfig_DiskGiBOmitted covers the other half of #556 item 2: when
// resources.disk_gib is omitted (zero), RootVolumeSizeGiB must stay at its zero
// value so the AMI-default behavior (pkg/aws/ebs.go's buildBlockDevices) is
// unchanged from before this fix.
func TestTaskLaunchConfig_DiskGiBOmitted(t *testing.T) {
	spec := &taskproto.TaskSpec{TaskID: "t", Command: []string{"x"}, Lifecycle: taskproto.Lifecycle{TTL: "1h"}}
	cfg := taskLaunchConfig(spec, &taskproto.SizeResult{InstanceType: "m7i.large"}, "us-east-1", "p", "w")
	if cfg.RootVolumeSizeGiB != 0 {
		t.Errorf("RootVolumeSizeGiB = %d, want 0 (unset → AMI default, unchanged behavior)", cfg.RootVolumeSizeGiB)
	}
}

// TestTaskLaunchConfig_CostLimit covers #556 item 3: lifecycle.cost_limit must
// wire into the SAME aws.LaunchConfig.CostLimit field --cost-limit populates,
// which pkg/aws/pricing.go's resolvePricePerHour (the #533/#536 real-pricing
// fix) enforces against — not any older/removed static-price logic.
func TestTaskLaunchConfig_CostLimit(t *testing.T) {
	spec := &taskproto.TaskSpec{
		TaskID:    "t",
		Command:   []string{"x"},
		Lifecycle: taskproto.Lifecycle{TTL: "1h", CostLimit: 5.5},
	}
	cfg := taskLaunchConfig(spec, &taskproto.SizeResult{InstanceType: "m7i.large"}, "us-east-1", "p", "w")
	if cfg.CostLimit != 5.5 {
		t.Errorf("CostLimit = %v, want 5.5 (from lifecycle.cost_limit)", cfg.CostLimit)
	}
}

// TestTaskLaunchConfig_CostLimitOmitted covers the omitted half: no cost_limit
// in the spec must leave CostLimit at 0 ("disabled"), matching --cost-limit's
// own default and leaving today's behavior unchanged.
func TestTaskLaunchConfig_CostLimitOmitted(t *testing.T) {
	spec := &taskproto.TaskSpec{TaskID: "t", Command: []string{"x"}, Lifecycle: taskproto.Lifecycle{TTL: "1h"}}
	cfg := taskLaunchConfig(spec, &taskproto.SizeResult{InstanceType: "m7i.large"}, "us-east-1", "p", "w")
	if cfg.CostLimit != 0 {
		t.Errorf("CostLimit = %v, want 0 (unset → disabled, unchanged behavior)", cfg.CostLimit)
	}
}
