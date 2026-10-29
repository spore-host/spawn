package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/spore-host/spawn/pkg/launchconfig"
)

// configRefsEC2 is the region's instances, each with the spawn:config it
// carries, and no Spot requests.
type configRefsEC2 struct {
	spotRequestAPI
	shas  []string
	err   error
	calls int
}

func (f *configRefsEC2) DescribeInstances(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	var insts []ec2types.Instance
	for _, s := range f.shas {
		insts = append(insts, ec2types.Instance{Tags: []ec2types.Tag{{Key: aws.String(launchconfig.Tag), Value: aws.String(s)}}})
	}
	return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: insts}}}, nil
}

func (f *configRefsEC2) DescribeSpotInstanceRequests(_ context.Context, _ *ec2.DescribeSpotInstanceRequestsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSpotInstanceRequestsOutput, error) {
	return &ec2.DescribeSpotInstanceRequestsOutput{}, nil
}

// configObjects is a staging bucket's launch-config/ objects.
type configObjects struct {
	objects map[string]time.Time
	deleted []string
	calls   int
}

func (c *configObjects) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	c.calls++
	out := &s3.ListObjectsV2Output{}
	for k, t := range c.objects {
		if strings.HasPrefix(k, aws.ToString(in.Prefix)) {
			out.Contents = append(out.Contents, s3types.Object{Key: aws.String(k), LastModified: aws.Time(t)})
		}
	}
	return out, nil
}

func (c *configObjects) DeleteObject(_ context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	c.calls++
	c.deleted = append(c.deleted, aws.ToString(in.Key))
	return &s3.DeleteObjectOutput{}, nil
}

func configReaper(mode string, dryRun bool, refs *configRefsEC2, objs *configObjects) *reaper {
	return &reaper{
		regions: []string{"us-east-1"}, dryRun: dryRun, launchConfigs: mode,
		accounts: []account{{
			label: "acct", accountID: "111122223333",
			spotFor:    func(string) spotRequestAPI { return refs },
			configsFor: func(string) launchConfigS3API { return objs },
		}},
	}
}

// REAPER_LAUNCH_CONFIGS: unset is report, which deletes nothing; reap deletes
// the unreferenced object only; dry run wins; off makes no call at all.
func TestLaunchConfigSweepModes(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	used, unused := strings.Repeat("a", 64), strings.Repeat("b", 64)
	if parseLaunchConfigs("") != launchConfigsReport {
		t.Fatalf("unset REAPER_LAUNCH_CONFIGS = %q, want report", parseLaunchConfigs(""))
	}
	for _, tc := range []struct {
		mode    string
		dryRun  bool
		deletes int
		calls   bool
	}{
		{parseLaunchConfigs(""), false, 0, true},
		{launchConfigsReap, false, 1, true},
		{launchConfigsReap, true, 0, true},
		{launchConfigsOff, false, 0, false},
	} {
		refs := &configRefsEC2{shas: []string{used}}
		objs := &configObjects{objects: map[string]time.Time{
			launchconfig.Key(used): now.Add(-72 * time.Hour), launchconfig.Key(unused): now.Add(-72 * time.Hour),
		}}
		r := configReaper(tc.mode, tc.dryRun, refs, objs)
		var sum Summary
		r.reapLaunchConfigsRegion(context.Background(), r.accounts[0], "us-east-1", now, &sum)
		if len(objs.deleted) != tc.deletes || (tc.deletes == 1 && objs.deleted[0] != launchconfig.Key(unused)) {
			t.Errorf("mode %s dry-run %v: deleted %v", tc.mode, tc.dryRun, objs.deleted)
		}
		if got := refs.calls+objs.calls > 0; got != tc.calls {
			t.Errorf("mode %s: made calls = %v, want %v", tc.mode, got, tc.calls)
		}
		if tc.calls && tc.deletes == 0 && sum.LaunchConfigsWouldDelete != 1 {
			t.Errorf("mode %s dry-run %v: would-delete = %d, want 1", tc.mode, tc.dryRun, sum.LaunchConfigsWouldDelete)
		}
	}
}

// Fail closed: if the instances cannot be read, the bucket is not touched.
func TestLaunchConfigSweepFailsClosed(t *testing.T) {
	refs := &configRefsEC2{err: errors.New("RequestLimitExceeded")}
	objs := &configObjects{objects: map[string]time.Time{launchconfig.Key(strings.Repeat("b", 64)): time.Unix(0, 0)}}
	r := configReaper(launchConfigsReap, false, refs, objs)
	var sum Summary
	r.reapLaunchConfigsRegion(context.Background(), r.accounts[0], "us-east-1", time.Now(), &sum)
	if objs.calls != 0 || len(objs.deleted) != 0 {
		t.Errorf("touched the bucket (%d calls) with the instances unread", objs.calls)
	}
	if sum.Errors != 1 {
		t.Errorf("Errors = %d, want the unreadable describe counted", sum.Errors)
	}
}
