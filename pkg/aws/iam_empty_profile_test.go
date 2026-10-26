package aws

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/spore-host/spawn/pkg/testutil"
)

// #1318: an existing instance profile with no role in it (a role sweep that
// removed the role and had not yet deleted the profile, or a launch that died
// between CreateInstanceProfile and AddRoleToInstanceProfile) must not be
// handed back as-is: the instance would boot with no credentials. The second
// CreateOrGetInstanceProfile must put the role back.
func TestCreateOrGetInstanceProfile_RepairsAnEmptyProfile(t *testing.T) {
	env := testutil.SubstrateServer(t)
	c := NewClientFromConfig(env.AWSConfig)
	ctx := context.Background()
	cfg := IAMRoleConfig{InlinePolicyJSON: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`}

	name, err := c.CreateOrGetInstanceProfile(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ic := iam.NewFromConfig(env.AWSConfig)
	if _, err := ic.RemoveRoleFromInstanceProfile(ctx, &iam.RemoveRoleFromInstanceProfileInput{
		InstanceProfileName: aws.String(name), RoleName: aws.String(name),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := ic.GetInstanceProfile(ctx, &iam.GetInstanceProfileInput{InstanceProfileName: aws.String(name)})
	if err != nil || len(got.InstanceProfile.Roles) != 0 {
		t.Fatalf("fixture: the profile should now be empty (err %v)", err)
	}

	if _, err := c.CreateOrGetInstanceProfile(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	got, err = ic.GetInstanceProfile(ctx, &iam.GetInstanceProfileInput{InstanceProfileName: aws.String(name)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.InstanceProfile.Roles) != 1 || aws.ToString(got.InstanceProfile.Roles[0].RoleName) != name {
		t.Errorf("reused instance profile %s holds %d role(s); an instance launched with it gets no credentials", name, len(got.InstanceProfile.Roles))
	}
}
