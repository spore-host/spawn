//go:build e2e_tier0

package e2e

import (
	"context"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/launchconfig"
)

// substrateAccount is the account Substrate's credentials resolve to.
const substrateAccount = "123456789012"

func (e *spawnEnv) s3Client() *s3.Client {
	return s3.NewFromConfig(e.AWSConfig, func(o *s3.Options) { o.UsePathStyle = true })
}

// preconfigureStagingBucket creates the account's us-east-1 staging bucket with
// the lifecycle rules EnsureStagingBucket installs (#1274, #1359), so a launch
// finds it configured and only writes its launch config object. Substrate
// cannot run EnsureStagingBucket's own setup: it does not implement
// PutBucketOwnershipControls (scttfrdmn/substrate#1445). The rules are built
// from pkg/aws's exported names; if they drift from what Ensure checks, the
// launch reconfigures the bucket and fails here on that same 501, loudly.
func (e *spawnEnv) preconfigureStagingBucket() {
	e.t.Helper()
	ctx := context.Background()
	c := e.s3Client()
	bucket := launchconfig.Bucket(substrateAccount, "us-east-1")
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		e.t.Fatalf("CreateBucket %s: %v", bucket, err)
	}
	day := aws.Int32(spawnaws.StagingObjectExpiryDays)
	rules := []s3types.LifecycleRule{
		{
			ID: aws.String(spawnaws.StagingLifecycleRuleID), Status: s3types.ExpirationStatusEnabled,
			Filter:                         &s3types.LifecycleRuleFilter{Prefix: aws.String(spawnaws.StagingBootstrapPrefix)},
			Expiration:                     &s3types.LifecycleExpiration{Days: day},
			AbortIncompleteMultipartUpload: &s3types.AbortIncompleteMultipartUpload{DaysAfterInitiation: day},
		},
		{
			ID: aws.String(spawnaws.StagingBackstopRuleID), Status: s3types.ExpirationStatusEnabled,
			Filter:                         &s3types.LifecycleRuleFilter{Prefix: aws.String("")},
			Expiration:                     &s3types.LifecycleExpiration{Days: aws.Int32(launchconfig.ObjectMaxDays)},
			AbortIncompleteMultipartUpload: &s3types.AbortIncompleteMultipartUpload{DaysAfterInitiation: day},
		},
	}
	if _, err := c.PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{
		Bucket: aws.String(bucket), LifecycleConfiguration: &s3types.BucketLifecycleConfiguration{Rules: rules},
	}); err != nil {
		e.t.Fatalf("PutBucketLifecycleConfiguration %s: %v", bucket, err)
	}
}

// launchConfigObject fetches the launch config object sha names from the
// staging bucket, as spored does, and verifies it.
func (e *spawnEnv) launchConfigObject(sha string) map[string]string {
	e.t.Helper()
	out, err := e.s3Client().GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(launchconfig.Bucket(substrateAccount, "us-east-1")), Key: aws.String(launchconfig.Key(sha)),
	})
	if err != nil {
		e.t.Fatalf("the launch config object %s is not in the staging bucket: %v", launchconfig.Key(sha), err)
	}
	defer func() { _ = out.Body.Close() }()
	body, err := io.ReadAll(out.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	doc, _, err := launchconfig.Decode(body, sha)
	if err != nil {
		e.t.Fatalf("the launch config object: %v", err)
	}
	return doc.Settings
}

// describeInstance fetches a single instance from Substrate by ID and returns
// its state name and tag map, so lifecycle tests can assert what actually
// landed in the emulator rather than just that the command exited 0.
func (e *spawnEnv) describeInstance(id string) (state string, tags map[string]string) {
	e.t.Helper()
	out, err := e.EC2Client().DescribeInstances(context.Background(), &ec2.DescribeInstancesInput{
		InstanceIds: []string{id},
	})
	if err != nil {
		e.t.Fatalf("DescribeInstances %s: %v", id, err)
	}
	tags = map[string]string{}
	for _, r := range out.Reservations {
		for _, inst := range r.Instances {
			if aws.ToString(inst.InstanceId) != id {
				continue
			}
			if inst.State != nil {
				state = string(inst.State.Name)
			}
			for _, t := range inst.Tags {
				tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
			}
		}
	}
	return state, tags
}

// requireState fails unless the instance's Substrate state matches want.
func (e *spawnEnv) requireState(id, want string) {
	e.t.Helper()
	if got, _ := e.describeInstance(id); got != want {
		e.t.Errorf("instance %s state = %q, want %q", id, got, want)
	}
}

// requireTag fails unless the instance carries tag key=want.
func (e *spawnEnv) requireTag(id, key, want string) {
	e.t.Helper()
	_, tags := e.describeInstance(id)
	if got, ok := tags[key]; !ok {
		e.t.Errorf("instance %s missing tag %q (have: %v)", id, key, tagKeys(tags))
	} else if want != "" && got != want {
		e.t.Errorf("instance %s tag %q = %q, want %q", id, key, got, want)
	}
}

func tagKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
