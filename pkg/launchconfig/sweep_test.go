package launchconfig

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
	"github.com/aws/smithy-go"
)

const (
	tAccount = "111122223333"
	tRegion  = "us-east-1"
)

var tNow = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

func sha(c string) string { return strings.Repeat(c, 64) }

// fakeObjects is a staging bucket: keys to their last write.
type fakeObjects struct {
	objects map[string]time.Time
	// rewrite, if set, is written again at tNow between the listing and the
	// delete, as a launch re-uploading the same content-addressed key would.
	rewrite  string
	lists    []string
	deletes  []string
	owners   []string
	listErr  error
	listings int
}

func (f *fakeObjects) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	f.listings++
	f.lists = append(f.lists, aws.ToString(in.Prefix))
	f.owners = append(f.owners, aws.ToString(in.ExpectedBucketOwner))
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.listings > 1 && f.rewrite != "" {
		f.objects[f.rewrite] = tNow
	}
	out := &s3.ListObjectsV2Output{}
	for k, t := range f.objects {
		if strings.HasPrefix(k, aws.ToString(in.Prefix)) {
			out.Contents = append(out.Contents, s3types.Object{Key: aws.String(k), LastModified: aws.Time(t), Size: aws.Int64(10)})
		}
	}
	return out, nil
}

func (f *fakeObjects) DeleteObject(_ context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	f.deletes = append(f.deletes, aws.ToString(in.Key))
	f.owners = append(f.owners, aws.ToString(in.ExpectedBucketOwner))
	delete(f.objects, aws.ToString(in.Key))
	return &s3.DeleteObjectOutput{}, nil
}

// fakeRefs serves instances and Spot requests with tags, one page at a time.
type fakeRefs struct {
	instances [][]ec2types.Tag
	requests  [][]ec2types.Tag
	instErr   error
	spotErr   error
	filters   [][]ec2types.Filter
}

func (f *fakeRefs) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	f.filters = append(f.filters, in.Filters)
	if f.instErr != nil {
		return nil, f.instErr
	}
	// One instance per page, to exercise pagination.
	i := 0
	if in.NextToken != nil {
		i = int(aws.ToString(in.NextToken)[0] - '0')
	}
	if i >= len(f.instances) {
		return &ec2.DescribeInstancesOutput{}, nil
	}
	out := &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{{Tags: f.instances[i]}}}}}
	if i+1 < len(f.instances) {
		out.NextToken = aws.String(string(rune('0' + i + 1)))
	}
	return out, nil
}

func (f *fakeRefs) DescribeSpotInstanceRequests(_ context.Context, in *ec2.DescribeSpotInstanceRequestsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSpotInstanceRequestsOutput, error) {
	f.filters = append(f.filters, in.Filters)
	if f.spotErr != nil {
		return nil, f.spotErr
	}
	out := &ec2.DescribeSpotInstanceRequestsOutput{}
	for _, t := range f.requests {
		out.SpotInstanceRequests = append(out.SpotInstanceRequests, ec2types.SpotInstanceRequest{Tags: t})
	}
	return out, nil
}

func configTag(s string) []ec2types.Tag {
	return []ec2types.Tag{{Key: aws.String(Tag), Value: aws.String(s)}, {Key: aws.String("spawn:managed"), Value: aws.String("true")}}
}

// The deleter gate (#1359): in reap mode the sweep deletes exactly the config
// objects nothing references that were written over a day ago, and keeps an
// object an instance (on any page) or a Spot request names, one written
// within the day, any key that is not a config object, and one re-written
// while it ran.
func TestSweepDeletesOnlyUnreferencedObjects(t *testing.T) {
	old := tNow.Add(-3 * 24 * time.Hour)
	objs := &fakeObjects{objects: map[string]time.Time{
		Key(sha("a")):                   old,                      // instance on page 1
		Key(sha("b")):                   old,                      // instance on page 2
		Key(sha("c")):                   old,                      // a persistent Spot request
		Key(sha("d")):                   old,                      // nothing: delete
		Key(sha("e")):                   tNow.Add(-1 * time.Hour), // young
		Key(sha("f")):                   old,                      // re-written mid-sweep
		KeyPrefix + "notes.txt":         old,                      // not a config object
		KeyPrefix + "config-short.json": old,
	}, rewrite: Key(sha("f"))}
	refsAPI := &fakeRefs{
		instances: [][]ec2types.Tag{configTag(sha("a")), configTag(sha("b"))},
		requests:  [][]ec2types.Tag{configTag(sha("c"))},
	}
	refs, err := Referenced(context.Background(), refsAPI, Tag)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Sweep(context.Background(), objs, refs, SweepOptions{Account: tAccount, Region: tRegion, Now: tNow, Delete: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(objs.deletes, ",") != Key(sha("d")) {
		t.Fatalf("deleted %v, want exactly %s", objs.deletes, Key(sha("d")))
	}
	reasons := map[string]string{}
	for _, d := range res.Decisions {
		reasons[d.Key] = d.Reason
	}
	for k, want := range map[string]string{
		Key(sha("a")): KeepReferenced, Key(sha("b")): KeepReferenced, Key(sha("c")): KeepReferenced,
		Key(sha("e")): KeepYoung, Key(sha("f")): KeepRewritten,
		KeyPrefix + "notes.txt": KeepNotObject, KeyPrefix + "config-short.json": KeepNotObject,
	} {
		if reasons[k] != want {
			t.Errorf("%s kept for %q, want %q", k, reasons[k], want)
		}
	}
	for _, p := range objs.lists {
		if !strings.HasPrefix(p, KeyPrefix) {
			t.Errorf("listed prefix %q: the reaper may list launch-config/ only", p)
		}
	}
	for _, o := range objs.owners {
		if o != tAccount {
			t.Errorf("a call named owner %q, want %s", o, tAccount)
		}
	}
	if _, deleted, would, failed := res.Count(); deleted != 1 || would != 0 || failed != 0 {
		t.Errorf("Count = deleted %d, would %d, failed %d", deleted, would, failed)
	}
	// Only live instances and open requests count as references.
	for _, fs := range refsAPI.filters {
		for _, f := range fs {
			if aws.ToString(f.Name) == "instance-state-name" && strings.Contains(strings.Join(f.Values, ","), "terminated") {
				t.Error("a terminated instance counts as a reference")
			}
		}
	}
}

// Report mode, the default, deletes nothing and reports what reap would.
func TestSweepReportModeDeletesNothing(t *testing.T) {
	objs := &fakeObjects{objects: map[string]time.Time{Key(sha("d")): tNow.Add(-48 * time.Hour)}}
	res, err := Sweep(context.Background(), objs, map[string]bool{}, SweepOptions{Account: tAccount, Region: tRegion, Now: tNow})
	if err != nil {
		t.Fatal(err)
	}
	if len(objs.deletes) != 0 {
		t.Fatalf("report mode deleted %v", objs.deletes)
	}
	if _, _, would, _ := res.Count(); would != 1 {
		t.Errorf("would-delete = %d, want 1", would)
	}
}

// Fail closed: references that cannot be read are an error, never an empty
// set, and Sweep refuses to run without them.
func TestSweepFailsClosedOnAnUnreadableDescribe(t *testing.T) {
	for name, api := range map[string]*fakeRefs{
		"instances":     {instErr: errors.New("UnauthorizedOperation")},
		"spot requests": {spotErr: errors.New("throttled")},
	} {
		t.Run(name, func(t *testing.T) {
			refs, err := Referenced(context.Background(), api, Tag)
			if err == nil || refs != nil {
				t.Fatalf("Referenced = %v, %v; want an error and no set", refs, err)
			}
			objs := &fakeObjects{objects: map[string]time.Time{Key(sha("d")): tNow.Add(-48 * time.Hour)}}
			if _, err := Sweep(context.Background(), objs, refs, SweepOptions{Account: tAccount, Region: tRegion, Now: tNow, Delete: true}); err == nil {
				t.Error("Sweep ran with an unknown referenced set")
			}
			if len(objs.deletes) != 0 || objs.listings != 0 {
				t.Errorf("touched the bucket (%d listings, deletes %v) with the references unknown", objs.listings, objs.deletes)
			}
		})
	}
}

// No bucket is nothing to sweep, not a failure.
func TestSweepWithNoBucket(t *testing.T) {
	objs := &fakeObjects{listErr: &smithy.GenericAPIError{Code: "NoSuchBucket"}}
	res, err := Sweep(context.Background(), objs, map[string]bool{}, SweepOptions{Account: tAccount, Region: tRegion, Now: tNow, Delete: true})
	if err != nil || !res.Absent {
		t.Fatalf("Sweep = %+v, %v; want absent", res, err)
	}
}
