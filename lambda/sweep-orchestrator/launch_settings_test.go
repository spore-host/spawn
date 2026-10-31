package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/sweep"
	"gopkg.in/yaml.v3"
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

// A member whose settings this build cannot fully apply is refused rather than
// launched without them, and not as a capacity error (which would just try the
// next instance type).
func TestMemberWithoutApplicableSettingsIsNotLaunched(t *testing.T) {
	newer := cliSettings(t, nil)
	newer["future_setting"] = true
	otherSchema := cliSettings(t, nil)
	otherSchema["schema"] = float64(spawnaws.LaunchSettingsSchema + 1)
	for name, config := range map[string]map[string]interface{}{
		"unknown field": {userDataParam: "x", tagsParam: cliTags(nil), launchSettingsParam: newer},
		"other schema":  {userDataParam: "x", tagsParam: cliTags(nil), launchSettingsParam: otherSchema},
		"not an object": {userDataParam: "x", tagsParam: cliTags(nil), launchSettingsParam: "volume_size=200"},
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

// legacyConfig is a member's params as a CLI older than #1353 uploads them: its
// bootstrap and tags, the bare spot flag, and no launch settings.
func legacyConfig(spot bool) map[string]interface{} {
	return map[string]interface{}{
		"instance_type": "t3.micro", "ami": "ami-0123456789abcdef0", "spot": spot,
		userDataParam: "H4sIAAAAAAAA/legacy", tagsParam: cliTags(map[string]string{"spawn:version": "0.146.0"}),
	}
}

// assertLegacyLaunch holds in to what this Lambda composed before #1353: the
// bare Spot market option for a spot member, and none of the settings.
func assertLegacyLaunch(t *testing.T, in *ec2.RunInstancesInput, spot bool) {
	t.Helper()
	if spot {
		if in.InstanceMarketOptions == nil || in.InstanceMarketOptions.MarketType != ec2types.MarketTypeSpot ||
			in.InstanceMarketOptions.SpotOptions != nil {
			t.Errorf("InstanceMarketOptions = %+v, want the bare spot market option legacy params always got", in.InstanceMarketOptions)
		}
	} else if in.InstanceMarketOptions != nil {
		t.Errorf("an on-demand legacy member got market options %+v", in.InstanceMarketOptions)
	}
	if in.BlockDeviceMappings != nil || in.NetworkInterfaces != nil || in.Placement != nil ||
		in.HibernationOptions != nil || in.CpuOptions != nil || in.MetadataOptions != nil {
		t.Errorf("a legacy member got settings its params never carried: %+v", in)
	}
	if awssdk.ToString(in.UserData) != "H4sIAAAAAAAA/legacy" || awssdk.ToString(in.ImageId) != "ami-0123456789abcdef0" {
		t.Errorf("legacy member lost its user data or AMI: UserData=%q ImageId=%q", awssdk.ToString(in.UserData), awssdk.ToString(in.ImageId))
	}
}

// TestLegacyParamsMemberLaunchesAsBefore: this Lambda is shared, hosted infra,
// so after it is redeployed it still serves CLIs older than #1353. Their
// members launch as they did, from the legacy params, the bare spot flag
// honoured, rather than being refused.
func TestLegacyParamsMemberLaunchesAsBefore(t *testing.T) {
	for _, spot := range []bool{true, false} {
		in, err := buildRunInstancesInput(&SweepRecord{SweepID: "legacy-sweep"}, legacyConfig(spot), 0, "t3.micro", time.Now())
		if err != nil {
			t.Fatalf("spot=%t: a legacy member was refused: %v", spot, err)
		}
		assertLegacyLaunch(t, in, spot)
	}
}

// TestScheduleReplayedLegacyParamsMemberLaunches: a schedule's params are the
// ones its creating CLI uploaded, and scheduler-handler replays them on every
// firing (YAML-decoded, re-encoded by sweep.EncodeParams), so a schedule an
// older CLI made stays legacy for its whole life. Such a member launches.
func TestScheduleReplayedLegacyParamsMemberLaunches(t *testing.T) {
	stored, err := json.Marshal(map[string]interface{}{
		"defaults": map[string]interface{}{"instance_type": "t3.micro"},
		"params":   []map[string]interface{}{legacyConfig(true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	// scheduler-handler: downloadParams yaml-decodes the stored object.
	var handler struct {
		Defaults map[string]interface{}   `yaml:"defaults"`
		Params   []map[string]interface{} `yaml:"params"`
	}
	if err := yaml.Unmarshal(stored, &handler); err != nil {
		t.Fatal(err)
	}
	// prepareSweep stages it with sweep.EncodeParams.
	body, err := sweep.EncodeParams(&sweep.ParamFileFormat{Defaults: handler.Defaults, Params: handler.Params})
	if err != nil {
		t.Fatal(err)
	}
	// The orchestrator: downloadParams json-decodes it, then mergeParams.
	var params ParamFileFormat
	if err := json.Unmarshal(body, &params); err != nil {
		t.Fatal(err)
	}
	in, err := buildRunInstancesInput(&SweepRecord{SweepID: "nightly-1"}, mergeParams(params.Defaults, params.Params[0]), 0, "t3.micro", time.Now())
	if err != nil {
		t.Fatalf("a schedule-replayed legacy member was refused: %v", err)
	}
	assertLegacyLaunch(t, in, true)
}

// TestPrivateMemberLaunchesWithNoPublicIP is #1437's Lambda half: a member the
// CLI shipped with no_public_ip (its subnet checked and pinned at --detach)
// asks RunInstances for no public IP on its interface, in that subnet, where
// it would otherwise ask for one (#308). Settings saying no_public_ip with no
// subnet were not written by the CLI and are refused: EC2 would pick a subnet
// nobody checked.
func TestPrivateMemberLaunchesWithNoPublicIP(t *testing.T) {
	build := func(lc *spawnaws.LaunchConfig) (*ec2.RunInstancesInput, error) {
		config := map[string]interface{}{userDataParam: "x", tagsParam: cliTags(nil), launchSettingsParam: cliSettings(t, lc)}
		return buildRunInstancesInput(&SweepRecord{SweepID: "s"}, config, 0, "c8i.large", time.Now())
	}
	public, err := build(&spawnaws.LaunchConfig{SubnetID: "subnet-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(public.NetworkInterfaces) != 1 || !awssdk.ToBool(public.NetworkInterfaces[0].AssociatePublicIpAddress) {
		t.Fatalf("a plain member's interfaces %+v do not ask for a public IP; the gate below proves nothing", public.NetworkInterfaces)
	}
	private, err := build(&spawnaws.LaunchConfig{SubnetID: "subnet-1", NoPublicIP: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(private.NetworkInterfaces) != 1 {
		t.Fatalf("NetworkInterfaces = %+v, want one", private.NetworkInterfaces)
	}
	ni := private.NetworkInterfaces[0]
	if ni.AssociatePublicIpAddress == nil || *ni.AssociatePublicIpAddress || awssdk.ToString(ni.SubnetId) != "subnet-1" {
		t.Errorf("a --no-public-ip member's interface asks for public IP %v in subnet %q; want an explicit false "+
			"(a default subnet assigns one otherwise) in the checked subnet-1", ni.AssociatePublicIpAddress, awssdk.ToString(ni.SubnetId))
	}
	if in, err := build(&spawnaws.LaunchConfig{NoPublicIP: true, AvailabilityZone: "us-east-1a"}); err == nil || in != nil {
		t.Errorf("a private-only member with no subnet was launched (input built: %t, err=%v)", in != nil, err)
	}
}

// eipCall is an invocation of an EC2 Elastic IP operation.
var eipCall = regexp.MustCompile(`\.(AllocateAddress|AssociateAddress|DisassociateAddress|ReleaseAddress)\(`)

// TestOrchestratorNeverManagesElasticIPs: a member gets its public IP, if any,
// from RunInstances alone; this Lambda allocates and associates no Elastic IP.
// That is why multi-card EFA, which needs one, is refused at --detach (#1353),
// and why a --no-public-ip member needs no more than its settings to have no
// public address at all (#1437). Every non-test source file is read.
func TestOrchestratorNeverManagesElasticIPs(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	read := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		read++
		if m := eipCall.Find(src); m != nil {
			t.Errorf("%s calls %s: the orchestrator does not manage Elastic IPs; a member's public IP is RunInstances'", f, m)
		}
	}
	if read == 0 {
		t.Fatal("no orchestrator source found")
	}
}
