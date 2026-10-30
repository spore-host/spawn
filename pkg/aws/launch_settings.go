package aws

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// The RunInstances-side half of a launch, for a detached sweep member (#1353).
//
// A detached sweep's members are launched by the sweep-orchestrator Lambda,
// which never sees a LaunchConfig: it reads the params the CLI uploaded. Its
// RunInstances used to carry the instance type, AMI, key, profile, user data
// and tags and nothing else, so a member's volume size, security groups,
// subnet, AZ, Spot max price, hibernation, EFA, placement group, capacity
// reservation and nested virtualization were dropped on the way.
//
// LaunchSettings is that half, resolved by the CLI from the member's config
// (ResolveLaunchSettings, which makes the AWS calls Client.Launch makes for the
// same fields) and applied by the Lambda with Apply. Both Apply and
// Client.Launch compose the request with applyLaunchSettings, so a detached
// member's RunInstances is the foreground member's by construction.

// LaunchSettingsSchema is the version of [LaunchSettings] this build writes and
// understands. Bump it whenever a field is added or its meaning changes: an
// orchestrator built before the bump refuses a newer CLI's settings instead of
// launching without the new field.
const LaunchSettingsSchema = 1

// LaunchSettings is the RunInstances-side part of a sweep member's launch,
// resolved: every value is final, so applying it needs no AWS call and no
// knowledge of the instance type. JSON, as it travels in the uploaded params.
type LaunchSettings struct {
	// Schema is LaunchSettingsSchema of the build that wrote it.
	Schema int `json:"schema"`

	AvailabilityZone string   `json:"availability_zone,omitempty"`
	SubnetID         string   `json:"subnet_id,omitempty"`
	SecurityGroupIDs []string `json:"security_group_ids,omitempty"`
	PlacementGroup   string   `json:"placement_group,omitempty"`
	// EFA is a single EFA interface. More than one (#661) needs an Elastic IP
	// associated after launch, which only Client.Launch does.
	EFA bool `json:"efa,omitempty"`

	Spot          bool   `json:"spot,omitempty"`
	SpotMaxPrice  string `json:"spot_max_price,omitempty"`
	ReservationID string `json:"reservation_id,omitempty"`
	CapacityBlock bool   `json:"capacity_block,omitempty"`
	Hibernate     bool   `json:"hibernate,omitempty"`

	NestedVirtualization bool `json:"nested_virtualization,omitempty"`
	IMDSv2Enforced       bool `json:"imdsv2_enforced,omitempty"`
	IMDSv2HopLimit       int  `json:"imdsv2_hop_limit,omitempty"`

	// RootDeviceName is the AMI's registered root device and RootVolumeGiB the
	// root volume's final size: --volume-size or the default, sized for RAM
	// under hibernation, and never below the AMI's root snapshot.
	RootDeviceName string `json:"root_device_name"`
	RootVolumeGiB  int32  `json:"root_volume_gib"`
	EBSEncrypted   bool   `json:"ebs_encrypted,omitempty"`
	EBSKMSKeyID    string `json:"ebs_kms_key_id,omitempty"`

	AttachVolumes []LaunchSettingsVolume `json:"attach_volumes,omitempty"`
}

// LaunchSettingsVolume is an --attach-volume data volume as RunInstances
// creates it.
type LaunchSettingsVolume struct {
	SnapshotID string `json:"snapshot_id"`
	SizeGiB    int32  `json:"size_gib,omitempty"`
}

// ResolveLaunchSettings resolves lc's RunInstances-side settings with the AWS
// calls Client.Launch makes for them: the hibernation root size from the
// type's memory, and the AMI's root device and minimum size.
func (c *Client) ResolveLaunchSettings(ctx context.Context, lc LaunchConfig) LaunchSettings {
	rootDeviceName, amiMinGiB := rootDeviceInfoFromAMI(ctx, c.regionalEC2(lc.Region), lc.AMI)
	return resolveLaunchSettings(lc, func() (int64, bool) {
		return c.InstanceMemoryMiB(ctx, lc.Region, lc.InstanceType)
	}, rootDeviceName, amiMinGiB)
}

// NewLaunchSettings is lc's RunInstances-side settings, given the AMI's root
// device and minimum root size, as a launch whose instance-type memory lookup
// failed would size a hibernating root (the per-family estimate). Pure; it is
// what ResolveLaunchSettings returns when its lookups find nothing, and lets a
// caller with no AWS access render settings.
func NewLaunchSettings(lc LaunchConfig, rootDeviceName string, amiMinGiB int32) LaunchSettings {
	return resolveLaunchSettings(lc, func() (int64, bool) { return 0, false }, rootDeviceName, amiMinGiB)
}

// resolveLaunchSettings normalizes lc as Client.Launch does (prepareHibernation,
// with memoryMiB as the type's memory) and renders its settings.
func resolveLaunchSettings(lc LaunchConfig, memoryMiB func() (int64, bool), rootDeviceName string, amiMinGiB int32) LaunchSettings {
	prepareHibernationWith(&lc, memoryMiB)
	root := buildBlockDevices(lc, rootDeviceName, amiMinGiB)[0]
	s := LaunchSettings{
		Schema:               LaunchSettingsSchema,
		AvailabilityZone:     lc.AvailabilityZone,
		SubnetID:             lc.SubnetID,
		SecurityGroupIDs:     append([]string(nil), lc.SecurityGroupIDs...),
		PlacementGroup:       lc.PlacementGroup,
		EFA:                  lc.EFAEnabled,
		Spot:                 lc.Spot,
		SpotMaxPrice:         lc.SpotMaxPrice,
		ReservationID:        lc.ReservationID,
		CapacityBlock:        lc.CapacityBlock,
		Hibernate:            lc.Hibernate,
		NestedVirtualization: lc.NestedVirtualization,
		IMDSv2Enforced:       lc.IMDSv2Enforced,
		IMDSv2HopLimit:       lc.IMDSv2HopLimit,
		RootDeviceName:       aws.ToString(root.DeviceName),
		RootVolumeGiB:        aws.ToInt32(root.Ebs.VolumeSize),
		EBSEncrypted:         lc.EBSEncrypted,
		EBSKMSKeyID:          lc.EBSKMSKeyID,
	}
	for _, v := range lc.AttachVolumes {
		s.AttachVolumes = append(s.AttachVolumes, LaunchSettingsVolume{SnapshotID: v.SnapshotID, SizeGiB: v.SizeGiB})
	}
	return s
}

// config is s as the LaunchConfig applyLaunchSettings composes from.
func (s LaunchSettings) config() LaunchConfig {
	lc := LaunchConfig{
		AvailabilityZone:     s.AvailabilityZone,
		SubnetID:             s.SubnetID,
		SecurityGroupIDs:     append([]string(nil), s.SecurityGroupIDs...),
		PlacementGroup:       s.PlacementGroup,
		EFAEnabled:           s.EFA,
		Spot:                 s.Spot,
		SpotMaxPrice:         s.SpotMaxPrice,
		ReservationID:        s.ReservationID,
		CapacityBlock:        s.CapacityBlock,
		Hibernate:            s.Hibernate,
		NestedVirtualization: s.NestedVirtualization,
		IMDSv2Enforced:       s.IMDSv2Enforced,
		IMDSv2HopLimit:       s.IMDSv2HopLimit,
		RootVolumeSizeGiB:    s.RootVolumeGiB,
		EBSEncrypted:         s.EBSEncrypted,
		EBSKMSKeyID:          s.EBSKMSKeyID,
	}
	if len(lc.SecurityGroupIDs) == 0 {
		lc.SecurityGroupIDs = nil
	}
	for _, v := range s.AttachVolumes {
		lc.AttachVolumes = append(lc.AttachVolumes, AttachVolumeSpec{SnapshotID: v.SnapshotID, SizeGiB: v.SizeGiB})
	}
	return lc
}

// DecodeLaunchSettings reads LaunchSettings from a decoded params value (a JSON
// object). It fails, rather than ignoring what it cannot apply, on a schema
// other than LaunchSettingsSchema and on any field this build does not know:
// either means the settings were written by a newer spawn, and launching
// without them is the silent drop #1353 was.
func DecodeLaunchSettings(raw interface{}) (LaunchSettings, error) {
	var s LaunchSettings
	if raw == nil {
		return s, fmt.Errorf("no launch settings")
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return s, fmt.Errorf("launch settings: %w", err)
	}
	var probe struct {
		Schema *int `json:"schema"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return s, fmt.Errorf("launch settings are not a JSON object: %w", err)
	}
	if probe.Schema == nil {
		return s, fmt.Errorf("launch settings carry no schema version")
	}
	if *probe.Schema != LaunchSettingsSchema {
		return s, fmt.Errorf("launch settings are schema %d and this build understands only schema %d: "+
			"they were written by a different spawn release; deploy the orchestrator from the same release as the CLI",
			*probe.Schema, LaunchSettingsSchema)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return LaunchSettings{}, fmt.Errorf("launch settings (schema %d) hold a field this build cannot apply: %w; "+
			"deploy the orchestrator from the same release as the CLI", LaunchSettingsSchema, err)
	}
	return s, nil
}

// Apply sets on input every RunInstances field s carries, composed as
// Client.Launch composes them.
//
// A Spot member that hibernates would need a persistent Spot request, which
// the orchestrator cannot cancel before terminating its instance; the CLI
// refuses one, and so does this.
func (s LaunchSettings) Apply(input *ec2.RunInstancesInput) error {
	if s.Schema != LaunchSettingsSchema {
		return fmt.Errorf("launch settings are schema %d, this build applies schema %d", s.Schema, LaunchSettingsSchema)
	}
	lc := s.config()
	if lc.Spot && spotIsPersistent(lc) {
		return fmt.Errorf("a hibernating Spot member needs a persistent Spot request, which a detached sweep cannot launch")
	}
	applyLaunchSettings(input, lc, s.RootDeviceName, 0, nil)
	return nil
}

// applyLaunchSettings sets the RunInstances fields Client.Launch derives from
// lc beyond the instance type, AMI, key, profile, client token, user data and
// tags: block devices, CPU options, network interfaces (unless input already
// has them, as multi-interface EFA does), placement, hibernation, Spot,
// capacity reservation and IMDS. tags are the instance's, which a persistent
// Spot request also carries.
func applyLaunchSettings(input *ec2.RunInstancesInput, lc LaunchConfig, rootDeviceName string, amiMinGiB int32, tags []types.Tag) {
	// Block device mappings from the AMI's registered root device. The root
	// mapping's DeviceName must equal the AMI's RootDeviceName or the size/
	// encryption override lands on a non-root device and EC2 keeps the AMI's
	// default root — silently on /dev/sda1 AMIs like Ubuntu/Rocky (#284).
	input.BlockDeviceMappings = buildBlockDevices(lc, rootDeviceName, amiMinGiB)

	// Enable nested virtualization (hypervisor-in-instance) when requested.
	// Only C8i/M8i/R8i support it; RunInstances errors on other types.
	if lc.NestedVirtualization {
		input.CpuOptions = &types.CpuOptionsRequest{
			NestedVirtualization: types.NestedVirtualizationSpecificationEnabled,
		}
	}

	// Network configuration.
	if len(input.NetworkInterfaces) == 0 {
		if lc.EFAEnabled {
			input.NetworkInterfaces = []types.InstanceNetworkInterfaceSpecification{
				EFANetworkInterface(lc.SubnetID, lc.SecurityGroupIDs),
			}
		} else {
			// Always request a public IP via a NetworkInterface spec — this works
			// even when the subnet has MapPublicIpOnLaunch=false (fixes #308).
			ni := types.InstanceNetworkInterfaceSpecification{
				AssociatePublicIpAddress: aws.Bool(true),
				DeviceIndex:              aws.Int32(0),
				DeleteOnTermination:      aws.Bool(true),
			}
			if lc.SubnetID != "" {
				ni.SubnetId = aws.String(lc.SubnetID)
			}
			if len(lc.SecurityGroupIDs) > 0 {
				ni.Groups = lc.SecurityGroupIDs
			}
			input.NetworkInterfaces = []types.InstanceNetworkInterfaceSpecification{ni}
		}
	}

	// Add placement (AZ, placement group, and reservation)
	placement := &types.Placement{}
	if lc.AvailabilityZone != "" {
		placement.AvailabilityZone = aws.String(lc.AvailabilityZone)
	}
	if lc.PlacementGroup != "" {
		placement.GroupName = aws.String(lc.PlacementGroup)
	}
	if placement.AvailabilityZone != nil || placement.GroupName != nil {
		input.Placement = placement
	}

	// Add hibernation if enabled
	if lc.Hibernate {
		input.HibernationOptions = &types.HibernationOptionsRequest{
			Configured: aws.Bool(true),
		}
	}

	// Add Spot configuration if needed (one-time or persistent: see applySpotOptions).
	applySpotOptions(input, lc, tags)

	// Target a Capacity Reservation / Capacity Block for ML (#216). When a
	// reservation id is set, point RunInstances at it; the instance must be in the
	// reservation's AZ (the caller pins --az to match). Consuming a Capacity Block
	// additionally requires MarketType=capacity-block — and is mutually exclusive
	// with Spot (the caller rejects that combination before reaching here).
	if lc.ReservationID != "" {
		input.CapacityReservationSpecification = &types.CapacityReservationSpecification{
			CapacityReservationTarget: &types.CapacityReservationTarget{
				CapacityReservationId: aws.String(lc.ReservationID),
			},
		}
		if lc.CapacityBlock {
			input.InstanceMarketOptions = &types.InstanceMarketOptionsRequest{
				MarketType: types.MarketTypeCapacityBlock,
			}
		}
	}

	// Add IMDSv2 configuration if enforced (compliance requirement)
	if lc.IMDSv2Enforced {
		hopLimit := int32(1) // Default: only local access
		if lc.IMDSv2HopLimit > 0 {
			hopLimit = int32(lc.IMDSv2HopLimit)
		}

		input.MetadataOptions = &types.InstanceMetadataOptionsRequest{
			HttpTokens:              types.HttpTokensStateRequired, // Require IMDSv2
			HttpPutResponseHopLimit: aws.Int32(hopLimit),
			HttpEndpoint:            types.InstanceMetadataEndpointStateEnabled,
		}
	}
}
