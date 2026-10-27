package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
)

// cliSettings is the launch settings the CLI ships for a member with config lc
// (nil: a plain member), as the orchestrator receives them: through JSON.
func cliSettings(t *testing.T, lc *spawnaws.LaunchConfig) map[string]interface{} {
	t.Helper()
	cfg := spawnaws.LaunchConfig{}
	if lc != nil {
		cfg = *lc
	}
	data, err := json.Marshal(spawnaws.NewLaunchSettings(cfg, "/dev/xvda", 0))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestMemberLaunchesWithTheCLIsSettings is #1353's Lambda half: the settings
// the CLI uploaded reach RunInstances. The pkg/aws gate
// (TestDetachedMemberRunInstancesIsTheForegroundMembers) proves applying them
// composes what Client.Launch composes for the same config.
func TestMemberLaunchesWithTheCLIsSettings(t *testing.T) {
	member := &spawnaws.LaunchConfig{
		AvailabilityZone: "us-east-1b", SubnetID: "subnet-1", SecurityGroupIDs: []string{"sg-1", "sg-2"},
		PlacementGroup: "pg-1", Spot: true, SpotMaxPrice: "0.25", RootVolumeSizeGiB: 200,
		NestedVirtualization: true, AttachVolumes: []spawnaws.AttachVolumeSpec{{SnapshotID: "snap-1"}},
	}
	config := map[string]interface{}{userDataParam: "x", tagsParam: cliTags(nil), launchSettingsParam: cliSettings(t, member)}
	in, err := buildRunInstancesInput(&SweepRecord{SweepID: "s"}, config, 0, "c8i.large", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if in.Placement == nil || awssdk.ToString(in.Placement.AvailabilityZone) != "us-east-1b" || awssdk.ToString(in.Placement.GroupName) != "pg-1" {
		t.Errorf("Placement = %+v, want us-east-1b / pg-1", in.Placement)
	}
	if len(in.NetworkInterfaces) != 1 || awssdk.ToString(in.NetworkInterfaces[0].SubnetId) != "subnet-1" ||
		strings.Join(in.NetworkInterfaces[0].Groups, ",") != "sg-1,sg-2" {
		t.Errorf("NetworkInterfaces = %+v, want subnet-1 with sg-1,sg-2", in.NetworkInterfaces)
	}
	if m := in.InstanceMarketOptions; m == nil || m.SpotOptions == nil || awssdk.ToString(m.SpotOptions.MaxPrice) != "0.25" ||
		m.SpotOptions.SpotInstanceType != ec2types.SpotInstanceTypeOneTime {
		t.Errorf("InstanceMarketOptions = %+v, want a one-time Spot request capped at 0.25", in.InstanceMarketOptions)
	}
	if len(in.BlockDeviceMappings) != 2 || awssdk.ToInt32(in.BlockDeviceMappings[0].Ebs.VolumeSize) != 200 ||
		awssdk.ToString(in.BlockDeviceMappings[1].Ebs.SnapshotId) != "snap-1" {
		t.Errorf("BlockDeviceMappings = %+v, want a 200 GiB root and snap-1", in.BlockDeviceMappings)
	}
	if in.CpuOptions == nil || in.CpuOptions.NestedVirtualization != ec2types.NestedVirtualizationSpecificationEnabled {
		t.Errorf("CpuOptions = %+v, want nested virtualization", in.CpuOptions)
	}
}

// A member with no settings, or with settings this build cannot fully apply, is
// refused rather than launched without them, and not as a capacity error (which
// would just try the next instance type).
func TestMemberWithoutApplicableSettingsIsNotLaunched(t *testing.T) {
	newer := cliSettings(t, nil)
	newer["future_setting"] = true
	for name, config := range map[string]map[string]interface{}{
		"no settings":   {userDataParam: "x", tagsParam: cliTags(nil)},
		"unknown field": {userDataParam: "x", tagsParam: cliTags(nil), launchSettingsParam: newer},
	} {
		in, err := buildRunInstancesInput(&SweepRecord{SweepID: "s"}, config, 2, "t3.micro", time.Now())
		if err == nil || in != nil {
			t.Errorf("%s: launched (input built: %t, err=%v)", name, in != nil, err)
			continue
		}
		if !strings.Contains(err.Error(), "param set 2") {
			t.Errorf("%s: error %q does not name the member", name, err)
		}
		if capacity, _ := isCapacityError(err); capacity {
			t.Errorf("%s: refusal reads as a capacity error", name)
		}
	}
}
