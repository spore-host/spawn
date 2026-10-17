package aws

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// The EFA interface `spawn launch --efa` and pipeline EFA stages share (#956).
func TestEFANetworkInterface(t *testing.T) {
	ni := EFANetworkInterface("subnet-1", []string{"sg-1"})
	if aws.ToString(ni.InterfaceType) != "efa" || aws.ToInt32(ni.DeviceIndex) != 0 ||
		!aws.ToBool(ni.DeleteOnTermination) || !aws.ToBool(ni.AssociatePublicIpAddress) {
		t.Errorf("interface = %+v, want the primary interface, type efa, public IP, deleted with the instance", ni)
	}
	if aws.ToString(ni.SubnetId) != "subnet-1" || len(ni.Groups) != 1 || ni.Groups[0] != "sg-1" {
		t.Errorf("subnet/groups = %q/%v", aws.ToString(ni.SubnetId), ni.Groups)
	}
	if bare := EFANetworkInterface("", nil); bare.SubnetId != nil || bare.Groups != nil {
		t.Errorf("empty subnet/groups should be left to EC2's defaults: %+v", bare)
	}
}
