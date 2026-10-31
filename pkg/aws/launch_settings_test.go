package aws

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// sampleLaunchConfigValue sets field f of lc to a non-zero sample. It reports
// false for a kind it cannot sample, which the gate below turns into a failure
// rather than a skipped field.
func sampleLaunchConfigValue(f reflect.Value, name string) bool {
	switch f.Kind() {
	case reflect.String:
		f.SetString("sample-" + strings.ToLower(name))
	case reflect.Bool:
		f.SetBool(true)
	case reflect.Int, reflect.Int32, reflect.Int64:
		f.SetInt(64)
	case reflect.Float64:
		f.SetFloat(1.5)
	case reflect.Slice:
		switch f.Type().Elem() {
		case reflect.TypeOf(""):
			f.Set(reflect.ValueOf([]string{"sample-" + strings.ToLower(name)}))
		case reflect.TypeOf(AttachVolumeSpec{}):
			f.Set(reflect.ValueOf([]AttachVolumeSpec{{SnapshotID: "snap-sample", MountPoint: "/data", SizeGiB: 50}}))
		default:
			return false
		}
	case reflect.Map:
		f.Set(reflect.ValueOf(map[string]string{"sample": "value"}))
	case reflect.Struct:
		// SporedObservability: one field set is enough to make it non-zero.
		for i := 0; i < f.NumField(); i++ {
			if f.Field(i).Kind() == reflect.Bool {
				f.Field(i).SetBool(true)
				return true
			}
		}
		return false
	default:
		return false
	}
	return true
}

// foregroundCompose is the part of Client.Launch's RunInstances that a launch
// config decides beyond the instance type, AMI, key, profile, client token, user
// data and tags, with the AWS lookups fixed: memoryMiB for the type's memory,
// rootDevice/amiMin for the AMI's root.
func foregroundCompose(lc LaunchConfig, memoryMiB int64, rootDevice string, amiMin int32) *ec2.RunInstancesInput {
	prepareHibernationWith(&lc, func() (int64, bool) { return memoryMiB, memoryMiB > 0 })
	in := &ec2.RunInstancesInput{}
	applyLaunchSettings(in, lc, rootDevice, amiMin, nil)
	return in
}

// detachedCompose is the same request as a detached sweep member gets it: the
// settings the CLI renders, through JSON as S3 delivers them and through the
// strict decoder the orchestrator uses, then applied.
func detachedCompose(t *testing.T, lc LaunchConfig, memoryMiB int64, rootDevice string, amiMin int32) (*ec2.RunInstancesInput, error) {
	t.Helper()
	s := resolveLaunchSettings(lc, func() (int64, bool) { return memoryMiB, memoryMiB > 0 }, rootDevice, amiMin)
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	back, err := DecodeLaunchSettings(raw)
	if err != nil {
		t.Fatalf("the orchestrator cannot decode settings this build wrote: %v", err)
	}
	in := &ec2.RunInstancesInput{}
	return in, back.Apply(in)
}

func isPersistentSpot(in *ec2.RunInstancesInput) bool {
	return in.InstanceMarketOptions != nil && in.InstanceMarketOptions.SpotOptions != nil &&
		in.InstanceMarketOptions.SpotOptions.SpotInstanceType == types.SpotInstanceTypePersistent
}

// TestDetachedMemberRunInstancesIsTheForegroundMembers is #1353's gate on the
// settings themselves. Every LaunchConfig field is discovered by reflection and
// set, alone, over several bases (plain, Spot, IMDSv2, a reservation, an
// encrypted root), so a field whose effect depends on another one is seen. Any
// field that changes the RunInstances request Client.Launch composes must change
// a detached member's request identically. A new LaunchConfig field that
// Client.Launch sends to RunInstances, and LaunchSettings does not carry, fails
// here.
func TestDetachedMemberRunInstancesIsTheForegroundMembers(t *testing.T) {
	const memMiB, rootDev, amiMin = 16384, "/dev/sda1", 8
	bases := map[string]LaunchConfig{
		"plain":       {InstanceType: "m7i.large", Region: "us-east-1", AMI: "ami-1"},
		"spot":        {InstanceType: "m7i.large", Region: "us-east-1", AMI: "ami-1", Spot: true},
		"imdsv2":      {InstanceType: "m7i.large", Region: "us-east-1", AMI: "ami-1", IMDSv2Enforced: true},
		"reservation": {InstanceType: "m7i.large", Region: "us-east-1", AMI: "ami-1", ReservationID: "cr-1"},
		"encrypted":   {InstanceType: "m7i.large", Region: "us-east-1", AMI: "ami-1", EBSEncrypted: true},
		// A subnet: a private-only member is applied only in the subnet the CLI
		// checked and pinned (#1437).
		"subnet": {InstanceType: "m7i.large", Region: "us-east-1", AMI: "ami-1", SubnetID: "subnet-1"},
	}
	affecting := map[string]bool{}
	typ := reflect.TypeOf(LaunchConfig{})
	for baseName, base := range bases {
		want0 := foregroundCompose(base, memMiB, rootDev, amiMin)
		got0, err := detachedCompose(t, base, memMiB, rootDev, amiMin)
		if err != nil {
			t.Fatalf("base %s: %v", baseName, err)
		}
		if !reflect.DeepEqual(want0, got0) {
			t.Fatalf("base %s: detached request differs before any field is set", baseName)
		}
		for i := 0; i < typ.NumField(); i++ {
			name := typ.Field(i).Name
			lc := base
			if !sampleLaunchConfigValue(reflect.ValueOf(&lc).Elem().Field(i), name) {
				t.Fatalf("cannot sample LaunchConfig.%s (%s): teach sampleLaunchConfigValue its kind, "+
					"or this gate cannot see whether a detached member carries it", name, typ.Field(i).Type)
			}
			want := foregroundCompose(lc, memMiB, rootDev, amiMin)
			if reflect.DeepEqual(want, want0) {
				continue // the field does not reach this part of RunInstances
			}
			affecting[name] = true
			got, err := detachedCompose(t, lc, memMiB, rootDev, amiMin)
			if isPersistentSpot(want) {
				// The orchestrator cannot launch a persistent request (it has no
				// cancel-before-terminate); it must refuse, never fall back to
				// one-time. The CLI refuses earlier (detachedSpotGuard).
				if err == nil {
					t.Errorf("base %s, %s: the foreground makes a persistent Spot request and the detached "+
						"settings launched without refusing", baseName, name)
				}
				continue
			}
			if lc.NoPublicIP && lc.SubnetID == "" {
				// A foreground launch pins and checks a subnet first
				// (PreflightNoPublicIP); settings with none were not written by
				// the CLI, and must not launch in a subnet EC2 picks (#1437).
				if err == nil {
					t.Errorf("base %s, %s: a private-only member with no subnet launched", baseName, name)
				}
				continue
			}
			if err != nil {
				t.Errorf("base %s, %s: detached settings refused: %v", baseName, name, err)
				continue
			}
			if !reflect.DeepEqual(want, got) {
				wj, _ := json.Marshal(want)
				gj, _ := json.Marshal(got)
				t.Errorf("base %s: LaunchConfig.%s changes Client.Launch's RunInstances but a detached member's "+
					"differs (#1353): carry it in LaunchSettings, or refuse it at --detach.\n foreground: %s\n detached:   %s",
					baseName, name, wj, gj)
			}
		}
	}
	// Non-vacuity: the fields #1353 named must be among those seen to matter.
	for _, name := range []string{"AvailabilityZone", "SubnetID", "SecurityGroupIDs", "PlacementGroup", "EFAEnabled",
		"SpotMaxPrice", "Hibernate", "HibernateOnIdle", "RootVolumeSizeGiB", "AttachVolumes", "NestedVirtualization",
		"ReservationID", "CapacityBlock", "IMDSv2HopLimit", "EBSKMSKeyID", "NoPublicIP"} {
		if !affecting[name] {
			t.Errorf("LaunchConfig.%s was never seen to change RunInstances; the gate's bases or sampler "+
				"no longer exercise it", name)
		}
	}
}

// An orchestrator built from an older LaunchSettings must refuse settings it
// cannot fully apply, rather than launch without the part it does not know.
func TestDecodeLaunchSettingsRefusesWhatItCannotApply(t *testing.T) {
	good := map[string]interface{}{"schema": float64(LaunchSettingsSchema), "root_device_name": "/dev/xvda", "root_volume_gib": float64(20)}
	if _, err := DecodeLaunchSettings(good); err != nil {
		t.Fatalf("current settings refused: %v", err)
	}
	cases := map[string]map[string]interface{}{
		"unknown field": {"schema": float64(LaunchSettingsSchema), "root_device_name": "/dev/xvda", "root_volume_gib": float64(20),
			"future_setting": true},
		"newer schema": {"schema": float64(LaunchSettingsSchema + 1), "root_device_name": "/dev/xvda", "root_volume_gib": float64(20)},
		"no schema":    {"root_device_name": "/dev/xvda", "root_volume_gib": float64(20)},
		"unknown nested field": {"schema": float64(LaunchSettingsSchema), "root_device_name": "/dev/xvda", "root_volume_gib": float64(20),
			"attach_volumes": []interface{}{map[string]interface{}{"snapshot_id": "snap-1", "iops": float64(3000)}}},
	}
	for name, raw := range cases {
		if s, err := DecodeLaunchSettings(raw); err == nil {
			t.Errorf("%s: decoded as %+v; an orchestrator must fail loudly on settings it cannot apply", name, s)
		}
	}
	if _, err := DecodeLaunchSettings(nil); err == nil {
		t.Error("missing settings decoded")
	}
}

// launchSettingsKeysV0147 are the top-level keys of LaunchSettings as every
// orchestrator from v0.147.0 (#1353) through v0.152.0 decodes them. A record
// of shipped releases: never edit it, add the next release's set beside it.
var launchSettingsKeysV0147 = []string{
	"schema", "availability_zone", "subnet_id", "security_group_ids", "placement_group", "efa",
	"spot", "spot_max_price", "reservation_id", "capacity_block", "hibernate",
	"nested_virtualization", "imdsv2_enforced", "imdsv2_hop_limit",
	"root_device_name", "root_volume_gib", "ebs_encrypted", "ebs_kms_key_id", "attach_volumes",
}

// olderLaunchSettings is LaunchSettings as a release that knew only keys had
// it: the same fields, less the ones added since.
func olderLaunchSettings(t *testing.T, keys []string) reflect.Type {
	t.Helper()
	typ := reflect.TypeOf(LaunchSettings{})
	var fields []reflect.StructField
	for i := 0; i < typ.NumField(); i++ {
		if slices.Contains(keys, jsonKey(typ.Field(i))) {
			fields = append(fields, typ.Field(i))
		}
	}
	if len(fields) != len(keys) {
		t.Fatalf("LaunchSettings has %d of the %d keys an older release had: a shipped field was renamed or removed", len(fields), len(keys))
	}
	return reflect.StructOf(fields)
}

func jsonKey(f reflect.StructField) string {
	return strings.Split(f.Tag.Get("json"), ",")[0]
}

// TestOlderOrchestratorRefusesSettingsItCannotApply is #1437's version-skew
// gate. The orchestrator Lambda is deployed separately from the CLI, and lags
// it. An orchestrator from a release before a LaunchSettings field must refuse
// settings that use the field (a --no-public-ip member launched there would get
// a public IP), and must still launch settings that do not (or every detached
// sweep breaks until it is redeployed). Both follow from the decoder refusing
// unknown fields and every field added since being omitempty; this decodes
// this build's settings through that decoder into the older release's shape.
func TestOlderOrchestratorRefusesSettingsItCannotApply(t *testing.T) {
	older := olderLaunchSettings(t, launchSettingsKeysV0147)
	decodeOlder := func(s LaunchSettings) error {
		t.Helper()
		data, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]interface{}
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatal(err)
		}
		return decodeLaunchSettingsInto(raw, reflect.New(older).Interface())
	}
	plain := LaunchConfig{InstanceType: "m7i.large", Region: "us-east-1", AMI: "ami-1", SubnetID: "subnet-1"}
	if err := decodeOlder(NewLaunchSettings(plain, "/dev/xvda", 0)); err != nil {
		t.Errorf("an older orchestrator refuses settings that use nothing it lacks: %v. A field added since "+
			"v0.147.0 is written when unset: make it omitempty, with its zero value the old meaning", err)
	}
	private := plain
	private.NoPublicIP = true
	if err := decodeOlder(NewLaunchSettings(private, "/dev/xvda", 0)); err == nil {
		t.Error("an orchestrator from before no_public_ip decodes a --no-public-ip member's settings, so it would " +
			"launch the member with a public IP")
	}
	// Every field added since is one an older orchestrator cannot apply: each,
	// set alone, must be refused there, and unset must not be written.
	typ := reflect.TypeOf(LaunchSettings{})
	added := 0
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if slices.Contains(launchSettingsKeysV0147, jsonKey(f)) {
			continue
		}
		added++
		if !strings.Contains(f.Tag.Get("json"), ",omitempty") {
			t.Errorf("LaunchSettings.%s is not omitempty: an older orchestrator refuses every member's settings", f.Name)
		}
		s := NewLaunchSettings(plain, "/dev/xvda", 0)
		if !sampleLaunchConfigValue(reflect.ValueOf(&s).Elem().Field(i), f.Name) {
			t.Fatalf("cannot sample LaunchSettings.%s", f.Name)
		}
		if err := decodeOlder(s); err == nil {
			t.Errorf("LaunchSettings.%s set: an orchestrator from before it decodes the settings and launches without it", f.Name)
		}
	}
	if added == 0 {
		t.Error("no LaunchSettings field is newer than v0.147.0; the gate is not exercising the skew")
	}
}
