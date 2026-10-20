package aws

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

func TestCheckUserDataSizeLimits(t *testing.T) {
	enc := func(n int) string { return base64.StdEncoding.EncodeToString(make([]byte, n)) }
	cases := []struct {
		name    string
		in      string
		tooBig  bool
		decoded int
	}{
		{"empty", "", false, 0},
		{"exactly 16384 decoded", enc(MaxUserDataBytes), false, MaxUserDataBytes},
		{"16385 decoded", enc(MaxUserDataBytes + 1), true, MaxUserDataBytes + 1},
		{"not base64, over the encoded limit", strings.Repeat("!", MaxEncodedUserDataBytes+1), true, MaxEncodedUserDataBytes + 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckUserDataSize(tc.in)
			var tooLarge *UserDataTooLargeError
			if got := errors.As(err, &tooLarge); got != tc.tooBig {
				t.Fatalf("CheckUserDataSize: err = %v, want too large = %v", err, tc.tooBig)
			}
			if tc.tooBig && tooLarge.DecodedBytes != tc.decoded {
				t.Errorf("DecodedBytes = %d, want %d", tooLarge.DecodedBytes, tc.decoded)
			}
		})
	}
}

type countingTransport struct{ n atomic.Int32 }

func (c *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	c.n.Add(1)
	return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}}, nil
}

// TestLaunchRefusesOversizedUserDataBeforeAnyAWSCall: Client.Launch is the one
// place every launch path goes through, so it refuses user data over EC2's
// limit before its identity, pricing and AMI calls, not at RunInstances
// (spawn#1063).
func TestLaunchRefusesOversizedUserDataBeforeAnyAWSCall(t *testing.T) {
	tr := &countingTransport{}
	c := NewClientFromConfig(aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		HTTPClient:  &http.Client{Transport: tr},
	})
	_, err := c.Launch(context.Background(), LaunchConfig{
		InstanceType: "m7i.large",
		Region:       "us-east-1",
		AMI:          "ami-12345678",
		TTL:          "1h",
		UserData:     base64.StdEncoding.EncodeToString(make([]byte, MaxUserDataBytes+1)),
	})
	var tooLarge *UserDataTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("Launch = %v, want *UserDataTooLargeError", err)
	}
	if n := tr.n.Load(); n != 0 {
		t.Errorf("Launch made %d AWS request(s) before refusing oversized user data; want 0", n)
	}
}
