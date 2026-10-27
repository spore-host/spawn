package launcher

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	sdkaws "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/spore-host/spawn/pkg/aws"
)

// Provision refuses a launch over AWS's 50-tag limit before the AMI lookup and
// the IAM setup, so nothing is looked up or created for a launch RunInstances
// would reject (#1336).
func TestProvisionRefusesTooManyTagsBeforeAnyAWSCall(t *testing.T) {
	tr := &countingTransport{}
	client := aws.NewClientFromConfig(sdkaws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		HTTPClient:  &http.Client{Transport: tr},
	})
	tags := map[string]string{}
	for i := 0; i < 50; i++ {
		tags[fmt.Sprintf("project-%02d", i)] = "v"
	}
	// No AMI and no instance profile: both would be AWS calls if reached.
	_, err := Provision(context.Background(), client,
		aws.LaunchConfig{InstanceType: "m7i.large", Region: "us-east-1", TTL: "1h", Tags: tags}, Options{})
	var limit *aws.TagLimitError
	if !errors.As(err, &limit) {
		t.Fatalf("Provision = %v, want *aws.TagLimitError", err)
	}
	if n := tr.n.Load(); n != 0 {
		t.Errorf("Provision made %d AWS request(s) before refusing a launch over the tag limit; want 0", n)
	}
}
