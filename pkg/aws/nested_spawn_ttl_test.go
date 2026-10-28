package aws

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

func countingClient() (*Client, *countingTransport) {
	tr := &countingTransport{}
	return NewClientFromConfig(aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		HTTPClient:  &http.Client{Transport: tr},
	}), tr
}

func TestCheckNestedSpawnTTL(t *testing.T) {
	nested := NestedSpawnRoleName()
	cases := []struct {
		profile, ttl string
		refused      bool
	}{
		{nested, "", true},
		{nested, "  ", true},
		{nested, "1h", false},
		{"spawn-instance-0123abcd", "", false},
		{SporedInstanceProfileName, "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		err := CheckNestedSpawnTTL("--instance-profile", tc.profile, tc.ttl)
		if got := errors.Is(err, ErrNestedSpawnNeedsTTL); got != tc.refused {
			t.Errorf("CheckNestedSpawnTTL(%q, %q) = %v, want refused = %v", tc.profile, tc.ttl, err, tc.refused)
		}
	}
	// The flag path and the named-role path give the same reason.
	flag, named := NestedSpawnTTLError().Error(), CheckNestedSpawnTTL("--instance-profile", nested, "").Error()
	reason := flag[strings.Index(flag, "requires --ttl"):]
	if !strings.HasSuffix(named, reason) {
		t.Errorf("named-role refusal %q does not end with the --nested-spawn wording %q", named, reason)
	}
}

// Client.Launch is the path every launch takes, the backstop for SDK callers:
// a nested-spawn profile without a TTL is refused before any AWS call (#1375).
func TestLaunchRefusesNestedProfileWithoutTTLBeforeAnyAWSCall(t *testing.T) {
	c, tr := countingClient()
	_, err := c.Launch(context.Background(), LaunchConfig{
		InstanceType:       "m7i.large",
		Region:             "us-east-1",
		AMI:                "ami-12345678",
		IamInstanceProfile: NestedSpawnRoleName(),
		IdleTimeout:        "1h",
	})
	if !errors.Is(err, ErrNestedSpawnNeedsTTL) {
		t.Fatalf("Launch = %v, want the nested-spawn TTL refusal", err)
	}
	if n := tr.n.Load(); n != 0 {
		t.Errorf("Launch made %d AWS request(s) before refusing; want 0", n)
	}
}

// CreateOrGetInstanceProfile would create a caller-named nested role with
// caller-chosen grants, or overwrite an existing one's fixed grants and baseline.
func TestCreateOrGetInstanceProfileRefusesANestedRoleName(t *testing.T) {
	c, tr := countingClient()
	_, err := c.CreateOrGetInstanceProfile(context.Background(), IAMRoleConfig{
		RoleName: NestedSpawnRoleName(),
		Policies: []string{"s3:ReadOnly"},
	})
	if !errors.Is(err, ErrNestedSpawnRoleReserved) {
		t.Fatalf("CreateOrGetInstanceProfile(nested RoleName) = %v, want ErrNestedSpawnRoleReserved", err)
	}
	if n := tr.n.Load(); n != 0 {
		t.Errorf("made %d AWS request(s) before refusing; want 0", n)
	}
}
