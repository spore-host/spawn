package aws

import (
	"encoding/json"
	"reflect"
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
		"ReservationID", "CapacityBlock", "IMDSv2HopLimit", "EBSKMSKeyID"} {
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
