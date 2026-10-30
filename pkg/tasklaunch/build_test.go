package tasklaunch

import (
	"reflect"
	"regexp"
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

// A task cannot ask for multi-card EFA, so it has nothing to carry
// --efa-secondary-type to (#1404): no TaskSpec field names EFA, a spec that
// tries is refused by the strict parser rather than launched without it, and
// the config a task launches with never enables EFA. If TaskSpec gains EFA,
// this fails until taskLaunchConfig threads EFAInterfaces and EFASecondaryType.
var (
	efaName = regexp.MustCompile(`EFA|Efa([A-Z]|$)`)
	efaTag  = regexp.MustCompile(`(^|_)efa(_|,|$)`)
)

func TestTaskSpecCarriesNoEFA(t *testing.T) {
	var walk func(reflect.Type, string)
	found := 0
	walk = func(typ reflect.Type, path string) {
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Map {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct {
			return
		}
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			found++
			// A word, not a substring: "default" contains "efa".
			if efaName.MatchString(f.Name) || efaTag.MatchString(f.Tag.Get("json")) {
				t.Errorf("TaskSpec%s.%s names EFA: thread it (and EFASecondaryType) through taskLaunchConfig, then update this gate", path, f.Name)
			}
			walk(f.Type, path+"."+f.Name)
		}
	}
	walk(reflect.TypeOf(taskproto.TaskSpec{}), "")
	if found == 0 {
		t.Fatal("discovered no TaskSpec fields")
	}

	for _, spec := range []string{
		`{"task_id":"t","command":["x"],"lifecycle":{"ttl":"1h"},"placement":{"efa_secondary_type":"efa"}}`,
		`{"task_id":"t","command":["x"],"lifecycle":{"ttl":"1h"},"resources":{"efa":true}}`,
	} {
		if _, err := taskproto.ParseSpec([]byte(spec)); err == nil {
			t.Errorf("ParseSpec accepted %s: the task would launch without EFA", spec)
		}
	}

	spec := &taskproto.TaskSpec{TaskID: "t", Command: []string{"x"}, Lifecycle: taskproto.Lifecycle{TTL: "1h"}}
	cfg := taskLaunchConfig(spec, &taskproto.SizeResult{InstanceType: "c8gn.48xlarge"}, "us-east-1", "p", "w")
	if cfg.EFAEnabled || cfg.EFAInterfaces != 0 || cfg.EFASecondaryType != "" {
		t.Errorf("task config enables EFA: %+v", cfg)
	}
}

// A task cannot ask for a private-only launch (#1433), so it has nothing to
// carry --no-public-ip to: no TaskSpec field names a public IP, and the config
// a task launches with never sets NoPublicIP, so its launch is unchanged. If
// TaskSpec gains one, this fails until taskLaunchConfig threads it (and a task
// gets a subnet to check).
var publicIPName = regexp.MustCompile(`(?i)public_?ip|private_?only`)

func TestTaskSpecCarriesNoPublicIPChoice(t *testing.T) {
	var walk func(reflect.Type, string)
	found := 0
	walk = func(typ reflect.Type, path string) {
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Map {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct {
			return
		}
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			found++
			if publicIPName.MatchString(f.Name) || publicIPName.MatchString(f.Tag.Get("json")) {
				t.Errorf("TaskSpec%s.%s names a public IP: thread it through taskLaunchConfig as LaunchConfig.NoPublicIP, then update this gate", path, f.Name)
			}
			walk(f.Type, path+"."+f.Name)
		}
	}
	walk(reflect.TypeOf(taskproto.TaskSpec{}), "")
	if found == 0 {
		t.Fatal("discovered no TaskSpec fields")
	}
	spec := &taskproto.TaskSpec{TaskID: "t", Command: []string{"x"}, Lifecycle: taskproto.Lifecycle{TTL: "1h"}}
	cfg := taskLaunchConfig(spec, &taskproto.SizeResult{InstanceType: "c8gn.48xlarge"}, "us-east-1", "p", "w")
	if cfg.NoPublicIP || cfg.AllowMissingEndpoints {
		t.Errorf("task config asks for a private-only launch: %+v", cfg)
	}
}
