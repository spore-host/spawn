package aws

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// TestApplyLaunchSettingsAlwaysRequiresIMDSv2 (#1588): every launch that goes
// through applyLaunchSettings (Client.Launch, and a detached member's
// LaunchSettings.Apply) requires IMDSv2 with hop limit 1, compliance mode or
// not; a caller's hop limit is honoured; IMDSv2Enforced=false never turns it
// off.
func TestApplyLaunchSettingsAlwaysRequiresIMDSv2(t *testing.T) {
	for _, tc := range []struct {
		name string
		lc   LaunchConfig
		want int32
	}{
		{"default", LaunchConfig{}, 1},
		{"compliance", LaunchConfig{IMDSv2Enforced: true, IMDSv2HopLimit: 1}, 1},
		{"bridged containers", LaunchConfig{IMDSv2HopLimit: IMDSHopLimitBridgedContainers}, 2},
		{"over EC2's maximum", LaunchConfig{IMDSv2HopLimit: 100}, MaxIMDSHopLimit},
	} {
		in := &ec2.RunInstancesInput{}
		applyLaunchSettings(in, tc.lc, "/dev/xvda", 0, nil)
		m := in.MetadataOptions
		if m == nil || m.HttpTokens != types.HttpTokensStateRequired ||
			m.HttpEndpoint != types.InstanceMetadataEndpointStateEnabled || aws.ToInt32(m.HttpPutResponseHopLimit) != tc.want {
			t.Errorf("%s: MetadataOptions = %+v, want IMDSv2 required, endpoint enabled, hop limit %d", tc.name, m, tc.want)
		}
	}
}

// An orchestrator deployed before #1588 applies IMDSv2 only when the settings
// say imdsv2_enforced, so the CLI always says it.
func TestLaunchSettingsAlwaysCarryIMDSv2Enforced(t *testing.T) {
	if s := NewLaunchSettings(LaunchConfig{}, "/dev/xvda", 0); !s.IMDSv2Enforced {
		t.Error("launch settings carry imdsv2_enforced=false: an older orchestrator would launch the member without IMDSv2")
	}
}
