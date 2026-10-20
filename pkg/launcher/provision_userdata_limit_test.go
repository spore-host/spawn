package launcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	sdkaws "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/spore-host/spawn/pkg/aws"
)

type countingTransport struct{ n atomic.Int32 }

func (c *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	c.n.Add(1)
	return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}}, nil
}

// TestProvisionRefusesOversizedUserDataBeforeAnyAWSCall: Provision builds the
// user data before the AMI lookup and the IAM setup, so user data over EC2's
// limit fails with nothing looked up or created (spawn#1063).
func TestProvisionRefusesOversizedUserDataBeforeAnyAWSCall(t *testing.T) {
	var b strings.Builder
	sum := sha256.Sum256([]byte("spawn#1063"))
	for b.Len() < 40*1024 {
		b.WriteString(hex.EncodeToString(sum[:]))
		sum = sha256.Sum256(sum[:])
	}
	tr := &countingTransport{}
	client := aws.NewClientFromConfig(sdkaws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		HTTPClient:  &http.Client{Transport: tr},
	})
	// No AMI and no instance profile: both would be AWS calls if reached.
	_, err := Provision(context.Background(), client,
		aws.LaunchConfig{InstanceType: "m7i.large", Region: "us-east-1", TTL: "1h"},
		Options{CustomUserData: "echo " + b.String()})
	var tooLarge *aws.UserDataTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("Provision = %v, want *aws.UserDataTooLargeError", err)
	}
	if n := tr.n.Load(); n != 0 {
		t.Errorf("Provision made %d AWS request(s) before refusing oversized user data; want 0", n)
	}
}
