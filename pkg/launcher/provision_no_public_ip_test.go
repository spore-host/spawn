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

// Gate (#1433): an SDK caller asking Provision for a private-only launch with
// neither a subnet nor an AZ to check is refused before the AMI lookup and the
// spored IAM role. Client.Launch refuses it too, but only after them.
func TestProvisionRefusesNoPublicIPWithoutSubnetOrAZBeforeAnyAWSCall(t *testing.T) {
	tr := &countingTransport{}
	client := aws.NewClientFromConfig(sdkaws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		HTTPClient:  &http.Client{Transport: tr},
	})
	_, err := Provision(context.Background(), client,
		aws.LaunchConfig{InstanceType: "c8gn.48xlarge", Region: "us-east-1", TTL: "1h", NoPublicIP: true},
		Options{})
	if !errors.Is(err, aws.ErrNoPublicIPNeedsSubnet) {
		t.Fatalf("Provision with --no-public-ip and no subnet or AZ = %v, want the subnet refusal", err)
	}
	if n := tr.n.Load(); n != 0 {
		t.Errorf("Provision made %d AWS request(s) before refusing; want 0", n)
	}
}
