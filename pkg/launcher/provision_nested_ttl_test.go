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

// Gate: an SDK caller (lagotto, MCP) naming the nested-spawn profile with no
// TTL is refused by Provision before the AMI lookup or any other AWS call
// (#1375). Provision does not otherwise require a TTL.
func TestProvisionRefusesNestedProfileWithoutTTLBeforeAnyAWSCall(t *testing.T) {
	tr := &countingTransport{}
	client := aws.NewClientFromConfig(sdkaws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		HTTPClient:  &http.Client{Transport: tr},
	})
	// No AMI: its lookup would be the first AWS call if the check were missing.
	_, err := Provision(context.Background(), client,
		aws.LaunchConfig{InstanceType: "m7i.large", Region: "us-east-1", IamInstanceProfile: aws.NestedSpawnRoleName(), IdleTimeout: "1h"},
		Options{})
	if !errors.Is(err, aws.ErrNestedSpawnNeedsTTL) {
		t.Fatalf("Provision with a nested-spawn profile and no TTL = %v, want the nested-spawn TTL refusal", err)
	}
	if n := tr.n.Load(); n != 0 {
		t.Errorf("Provision made %d AWS request(s) before refusing; want 0", n)
	}
}
