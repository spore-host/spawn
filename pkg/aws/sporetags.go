// The spore.host tags (libs/tags) on what spawn creates (#1652). See
// pkg/sporetags for the keys and the precedence rule.

package aws

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/spore-host/libs/tags/iamtags"
	"github.com/spore-host/libs/tags/s3tags"

	"github.com/spore-host/spawn/pkg/sporetags"
)

// recordCallerUserID keeps the caller's UserId from a GetCallerIdentity spawn
// made anyway, for spore-host:owner.
func (c *Client) recordCallerUserID(id *string) {
	if v := aws.ToString(id); v != "" {
		c.callerUserID.Store("id", v)
	}
}

// ownerTag is the spore-host:owner value for what this client creates: the
// caller's aws:userid when an earlier STS call recorded it, else "" (the tag
// is then left out rather than paid for with another call).
func (c *Client) ownerTag() string {
	if v, ok := c.callerUserID.Load("id"); ok {
		return v.(string)
	}
	return ""
}

// sporeTags is the spore.host tag set for a resource this client creates now
// in scope (a region, or sporetags.ScopeGlobal for IAM).
func (c *Client) sporeTags(scope string) map[string]string {
	return sporetags.For(scope, c.ownerTag())
}

// appendMissingTags appends to ts each key of m that ts does not already
// carry, in key order: the keys already in ts (spawn's own, and a caller's)
// win. This is how the spore-host:* set joins an EC2 tag list without
// replacing a caller-supplied spore-host:managed-by.
func appendMissingTags(ts []ec2types.Tag, m map[string]string) []ec2types.Tag {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		ts = appendTagUnique(ts, k, m[k])
	}
	return ts
}

// tagWarned holds the tagging operations a warning has been printed for, so a
// launch that converges several resources without the permission says so once.
var tagWarned sync.Map

// warnTagging reports, once per operation, that a resource could not be given
// the spore.host tags. It never fails the caller: the resource works without
// them, and is only invisible to leave-no-trace cleanup until a later run
// with the permission tags it.
var warnTagging = func(op, what string, err error) {
	if _, dup := tagWarned.LoadOrStore(op, true); dup {
		return
	}
	fmt.Fprintf(os.Stderr, "⚠️  could not tag %s with the spore-host:* tags (%s: %v); it works as before, but "+
		"leave-no-trace cleanup will not find it until a run allowed %s tags it (#1652)\n", what, op, err, op)
}

// warnConverge is warnTagging for a converge (tagging a resource that
// exists), except that an AccessDenied is not reported: the converge is
// opportunistic and runs on every use, so a principal that may not tag (the
// documented user baseline grants no iam:TagPolicy, for one) would see the
// warning on every launch for a resource that works the same either way. The
// create that left the resource untagged warned once already.
func warnConverge(op, what string, err error) {
	if IsAccessDenied(err) {
		return
	}
	warnTagging(op, what, err)
}

// The IAM tagging calls used to converge an existing resource's tags. They
// are optional interfaces, asserted on the IAM client a function was given:
// *iam.Client has them all, and a test fake that does not skips the converge.
type (
	roleTagger interface {
		TagRole(context.Context, *iam.TagRoleInput, ...func(*iam.Options)) (*iam.TagRoleOutput, error)
	}
	profileTagger interface {
		TagInstanceProfile(context.Context, *iam.TagInstanceProfileInput, ...func(*iam.Options)) (*iam.TagInstanceProfileOutput, error)
	}
	policyTagger interface {
		TagPolicy(context.Context, *iam.TagPolicyInput, ...func(*iam.Options)) (*iam.TagPolicyOutput, error)
	}
)

// convergeRoleTags adds to the existing role the desired keys it lacks
// (sporetags.Converge: nothing it carries is changed). Best-effort.
func convergeRoleTags(ctx context.Context, api any, role string, existing []iamtypes.Tag, desired map[string]string) {
	add := sporetags.Converge(iamtags.ToMap(existing), desired)
	t, ok := api.(roleTagger)
	if len(add) == 0 || !ok {
		return
	}
	if _, err := t.TagRole(ctx, &iam.TagRoleInput{RoleName: aws.String(role), Tags: iamtags.FromMap(add)}); err != nil {
		warnConverge("iam:TagRole", "IAM role "+role, err)
	}
}

// convergeProfileTags is convergeRoleTags for an instance profile.
func convergeProfileTags(ctx context.Context, api any, profile string, existing []iamtypes.Tag, desired map[string]string) {
	add := sporetags.Converge(iamtags.ToMap(existing), desired)
	t, ok := api.(profileTagger)
	if len(add) == 0 || !ok {
		return
	}
	if _, err := t.TagInstanceProfile(ctx, &iam.TagInstanceProfileInput{InstanceProfileName: aws.String(profile), Tags: iamtags.FromMap(add)}); err != nil {
		warnConverge("iam:TagInstanceProfile", "instance profile "+profile, err)
	}
}

// convergePolicyTags is convergeRoleTags for a managed policy.
func convergePolicyTags(ctx context.Context, api any, arn string, existing []iamtypes.Tag, desired map[string]string) {
	add := sporetags.Converge(iamtags.ToMap(existing), desired)
	t, ok := api.(policyTagger)
	if len(add) == 0 || !ok {
		return
	}
	if _, err := t.TagPolicy(ctx, &iam.TagPolicyInput{PolicyArn: aws.String(arn), Tags: iamtags.FromMap(add)}); err != nil {
		warnConverge("iam:TagPolicy", "managed policy "+arn, err)
	}
}

// createPolicyTagged is CreatePolicy with in's tags, and once more without
// them when IAM refuses the tagged call as AccessDenied: a principal granted
// iam:CreatePolicy but not iam:TagPolicy, which tag-on-create also needs,
// still gets the policy, untagged and warned about, rather than losing it.
func createPolicyTagged(ctx context.Context, create func(*iam.CreatePolicyInput) error, in *iam.CreatePolicyInput) error {
	err := create(in)
	if err == nil || len(in.Tags) == 0 || !IsAccessDenied(err) {
		return err
	}
	untagged := *in
	untagged.Tags = nil
	if uerr := create(&untagged); uerr != nil {
		return uerr
	}
	warnTagging("iam:TagPolicy", "managed policy "+aws.ToString(in.PolicyName), err)
	return nil
}

// appendMissingS3Tags is appendMissingTags for S3 tags.
func appendMissingS3Tags(ts []s3types.Tag, m map[string]string) []s3types.Tag {
	have := make(map[string]bool, len(ts))
	for _, t := range ts {
		have[aws.ToString(t.Key)] = true
	}
	for _, t := range s3tags.FromMap(m) {
		if !have[aws.ToString(t.Key)] {
			ts = append(ts, t)
		}
	}
	return ts
}

// bucketTagAPI is the S3 slice convergeBucketTags uses.
type bucketTagAPI interface {
	GetBucketTagging(context.Context, *s3.GetBucketTaggingInput, ...func(*s3.Options)) (*s3.GetBucketTaggingOutput, error)
	PutBucketTagging(context.Context, *s3.PutBucketTaggingInput, ...func(*s3.Options)) (*s3.PutBucketTaggingOutput, error)
}

// convergeBucketTags adds to an existing bucket that spawn made (it carries
// spawn:managed=true) the desired tags it lacks, keeping every tag it has.
// PutBucketTagging replaces the whole set, so the write is the bucket's tags
// plus the additions. A bucket spawn did not make is left alone, and so is
// any failure: the bucket works untagged. owner, when set, is the expected
// bucket owner.
func convergeBucketTags(ctx context.Context, api bucketTagAPI, bucket, owner string, desired map[string]string) {
	var exp *string
	if owner != "" {
		exp = aws.String(owner)
	}
	got, err := api.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: aws.String(bucket), ExpectedBucketOwner: exp})
	if err != nil {
		return
	}
	existing := s3tags.ToMap(got.TagSet)
	add := sporetags.Converge(existing, desired)
	if existing["spawn:managed"] != "true" || len(add) == 0 {
		return
	}
	if _, err := api.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{Bucket: aws.String(bucket), ExpectedBucketOwner: exp,
		Tagging: &s3types.Tagging{TagSet: appendMissingS3Tags(got.TagSet, add)}}); err != nil {
		warnConverge("s3:PutBucketTagging", "S3 bucket "+bucket, err)
	}
}
