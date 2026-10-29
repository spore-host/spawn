package launchconfig

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// The deleter (#1359): the TTL reaper's REAPER_LAUNCH_CONFIGS sweep.
//
// An object is deleted only when ALL of these hold:
//
//   - its key is exactly launch-config/config-<sha256>.json in the staging
//     bucket named for the account and region swept, every call naming the
//     account as the expected bucket owner;
//   - no instance in the region, pending through stopped, and no Spot request
//     in it that is open, active or disabled, carries spawn:config=<that sha>.
//     A Spot request counts because a persistent one launches a replacement,
//     which boots with a fresh root volume and reads its notification settings
//     from the object (#1080). If either describe fails, nothing is deleted;
//   - it was last written at least Grace ago, so an object a launch has just
//     uploaded is not deleted in the seconds before RunInstances tags an
//     instance with it, and that is checked twice: once in the listing, and
//     again for that key alone just before the delete, so a launch that
//     re-wrote the same content-addressed key while the instances were being
//     described keeps it.
//
// The objects are spawn's own machinery, a copy of settings the instance has
// cached on its root volume, so deleting one never loses data.

// Grace is how recently written an object must not be for the sweep to delete
// it.
const Grace = 24 * time.Hour

// EC2API is the EC2 slice the sweep uses to find the references. *ec2.Client
// satisfies it.
type EC2API interface {
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	DescribeSpotInstanceRequests(ctx context.Context, in *ec2.DescribeSpotInstanceRequestsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeSpotInstanceRequestsOutput, error)
}

// S3API is the S3 slice the sweep uses. *s3.Client satisfies it. Listing is
// only ever under [KeyPrefix]: the reaper's s3:ListBucket on the staging
// bucket is conditioned on that prefix, so the task bootstrap keys beside it
// stay unlistable (#1274).
type S3API interface {
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	DeleteObject(ctx context.Context, in *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
}

// Referenced is the set of shas some instance or Spot request in the region
// still names in its spawn:config tag. tagKey is that tag's key (spawn:config,
// or the deployment's prefix). An error means the set is unknown, and nothing
// may be deleted.
func Referenced(ctx context.Context, api EC2API, tagKey string) (map[string]bool, error) {
	refs := map[string]bool{}
	in := &ec2.DescribeInstancesInput{Filters: []ec2types.Filter{
		{Name: aws.String("tag-key"), Values: []string{tagKey}},
		{Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "shutting-down", "stopping", "stopped"}},
	}}
	for {
		out, err := api.DescribeInstances(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("describe instances: %w", err)
		}
		for _, r := range out.Reservations {
			for _, inst := range r.Instances {
				addRef(refs, inst.Tags, tagKey)
			}
		}
		if aws.ToString(out.NextToken) == "" {
			break
		}
		in.NextToken = out.NextToken
	}
	sin := &ec2.DescribeSpotInstanceRequestsInput{Filters: []ec2types.Filter{
		{Name: aws.String("tag-key"), Values: []string{tagKey}},
		{Name: aws.String("state"), Values: []string{"open", "active", "disabled"}},
	}}
	for {
		out, err := api.DescribeSpotInstanceRequests(ctx, sin)
		if err != nil {
			return nil, fmt.Errorf("describe spot instance requests: %w", err)
		}
		for _, r := range out.SpotInstanceRequests {
			addRef(refs, r.Tags, tagKey)
		}
		if aws.ToString(out.NextToken) == "" {
			break
		}
		sin.NextToken = out.NextToken
	}
	return refs, nil
}

func addRef(refs map[string]bool, tags []ec2types.Tag, tagKey string) {
	for _, t := range tags {
		if aws.ToString(t.Key) == tagKey {
			refs[aws.ToString(t.Value)] = true
		}
	}
}

// SweepOptions controls a sweep.
type SweepOptions struct {
	// Account owns the bucket; every call sends it as ExpectedBucketOwner.
	Account string
	// Region is the bucket's region, and the region refs was read in.
	Region string
	// Now is the sweep's clock.
	Now time.Time
	// Grace is how recently written an object may be and still be kept;
	// zero means [Grace].
	Grace time.Duration
	// Delete lets the sweep delete. Without it, it reports what it would
	// delete and changes nothing.
	Delete bool
}

// Decision is what a sweep did, or would do, with one object.
type Decision struct {
	Key  string
	SHA  string
	Size int64
	// LastModified is when the object was last written.
	LastModified time.Time
	// Delete is true when the object qualified for deletion.
	Delete bool
	// Deleted is true when it was deleted.
	Deleted bool
	// Reason says why it was kept.
	Reason string
	Err    error
}

// Reasons an object is kept.
const (
	KeepReferenced = "an instance or Spot request carries its spawn:config"
	KeepYoung      = "written within the grace"
	KeepNotObject  = "not a launch-config/config-<sha256>.json key"
	KeepRewritten  = "re-written while the sweep ran"
)

// SweepResult is one bucket's sweep.
type SweepResult struct {
	Bucket    string
	Decisions []Decision
	// Absent is true when there is no such bucket: nothing to sweep.
	Absent bool
}

// Count returns how many objects were listed, deleted, would have been
// deleted, and failed to delete.
func (r SweepResult) Count() (listed, deleted, would, failed int) {
	for _, d := range r.Decisions {
		listed++
		switch {
		case d.Deleted:
			deleted++
		case d.Delete && d.Err != nil:
			failed++
		case d.Delete:
			would++
		}
	}
	return
}

// Sweep lists the config objects in the account's staging bucket for the
// region and deletes, as opts allows, the ones refs does not name. refs is
// [Referenced] for the same region. A nil refs is refused, since a set that
// could not be read must never look like one that is empty.
func Sweep(ctx context.Context, api S3API, refs map[string]bool, opts SweepOptions) (SweepResult, error) {
	if opts.Account == "" || opts.Region == "" {
		return SweepResult{}, errors.New("launchconfig.Sweep: Account and Region are required")
	}
	if refs == nil {
		return SweepResult{}, errors.New("launchconfig.Sweep: the referenced set is unknown; refusing to delete anything")
	}
	grace := opts.Grace
	if grace <= 0 {
		grace = Grace
	}
	bucket := Bucket(opts.Account, opts.Region)
	res := SweepResult{Bucket: bucket}
	objs, err := listConfigObjects(ctx, api, bucket, opts.Account, KeyPrefix)
	if isCode(err, "NoSuchBucket") {
		res.Absent = true
		return res, nil
	}
	if err != nil {
		return res, err
	}
	for _, o := range objs {
		d := Decision{Key: aws.ToString(o.Key), Size: aws.ToInt64(o.Size), LastModified: aws.ToTime(o.LastModified)}
		sha, ok := SHAOfKey(d.Key)
		d.SHA = sha
		switch {
		case !ok:
			d.Reason = KeepNotObject
		case refs[sha]:
			d.Reason = KeepReferenced
		case d.LastModified.IsZero() || opts.Now.Sub(d.LastModified) < grace:
			d.Reason = KeepYoung
		default:
			d.Delete = true
		}
		if d.Delete && opts.Delete {
			deleteIfUnchanged(ctx, api, bucket, opts.Account, &d)
		}
		res.Decisions = append(res.Decisions, d)
	}
	return res, nil
}

// deleteIfUnchanged deletes d's object unless it was written again since the
// listing: a launch with the same settings re-uploads the same key just
// before RunInstances, and its instance may not have been visible to the
// describe.
func deleteIfUnchanged(ctx context.Context, api S3API, bucket, account string, d *Decision) {
	again, err := listConfigObjects(ctx, api, bucket, account, d.Key)
	if err != nil {
		d.Err = err
		return
	}
	for _, o := range again {
		if aws.ToString(o.Key) == d.Key && !aws.ToTime(o.LastModified).Equal(d.LastModified) {
			d.Delete, d.Reason = false, KeepRewritten
			return
		}
	}
	if _, err := api.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(d.Key), ExpectedBucketOwner: aws.String(account),
	}); err != nil {
		d.Err = err
		return
	}
	d.Deleted = true
}

func listConfigObjects(ctx context.Context, api S3API, bucket, account, prefix string) ([]s3types.Object, error) {
	var out []s3types.Object
	in := &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix), ExpectedBucketOwner: aws.String(account)}
	for {
		page, err := api.ListObjectsV2(ctx, in)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Contents...)
		if !aws.ToBool(page.IsTruncated) || aws.ToString(page.NextContinuationToken) == "" {
			break
		}
		in.ContinuationToken = page.NextContinuationToken
	}
	sort.Slice(out, func(i, j int) bool { return aws.ToString(out[i].Key) < aws.ToString(out[j].Key) })
	return out, nil
}

func isCode(err error, code string) bool {
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode() == code
}
