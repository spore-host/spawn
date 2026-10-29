package launcher

import (
	"context"
	"errors"
	"net/http"
	"testing"

	sdkaws "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/spore-host/spawn/pkg/aws"
)

// Gate (#1410): an SDK caller (lagotto, MCP) asking Provision for more than
// one EFA interface with neither a subnet nor an AZ is refused before the AMI
// lookup and the spored IAM role, the AWS calls Provision makes before
// Client.Launch. Client.Launch refuses it too, but only after them.
func TestProvisionRefusesMultiCardEFAWithoutSubnetOrAZBeforeAnyAWSCall(t *testing.T) {
	tr := &countingTransport{}
	client := aws.NewClientFromConfig(sdkaws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		HTTPClient:  &http.Client{Transport: tr},
	})
	// No AMI and no instance profile: their lookup and setup would be the
	// first AWS calls if the check were missing.
	_, err := Provision(context.Background(), client,
		aws.LaunchConfig{InstanceType: "c8gn.48xlarge", Region: "us-east-1", TTL: "1h", EFAEnabled: true, EFAInterfaces: 2},
		Options{})
	if !errors.Is(err, aws.ErrEFANeedsSubnet) {
		t.Fatalf("Provision with 2 EFA interfaces and no subnet or AZ = %v, want the EFA subnet refusal", err)
	}
	if n := tr.n.Load(); n != 0 {
		t.Errorf("Provision made %d AWS request(s) before refusing; want 0", n)
	}
}
