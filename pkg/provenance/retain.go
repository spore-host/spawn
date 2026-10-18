package provenance

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// RetainAPI is what setting spawn:retain needs. IAM: s3:GetObjectTagging and
// s3:PutObjectTagging on the objects, s3:ListBucket for a prefix.
type RetainAPI interface {
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	GetObjectTagging(ctx context.Context, in *s3.GetObjectTaggingInput, optFns ...func(*s3.Options)) (*s3.GetObjectTaggingOutput, error)
	PutObjectTagging(ctx context.Context, in *s3.PutObjectTaggingInput, optFns ...func(*s3.Options)) (*s3.PutObjectTaggingOutput, error)
}

// RetainResult is one object's outcome.
type RetainResult struct {
	Key     string `json:"key"`
	Outcome string `json:"outcome"` // set, unchanged, not-a-spawn-output, error
	Error   string `json:"error,omitempty"`
}

// Retain outcomes.
const (
	RetainSet       = "set"
	RetainUnchanged = "unchanged"
	RetainNotOutput = "not-a-spawn-output"
	RetainError     = "error"
)

// maxObjectTags is S3's limit on tags per object.
const maxObjectTags = 10

// SetRetain sets spawn:retain on one object, or on every object under a prefix
// when target ends in "/": "true" keeps it out of every tiering and deletion
// rule, "false" lets the rules apply. With "false" it is also how outputs
// written before spawn:retain existed (v0.132.x) are opted in (Decisions 7).
//
// PutObjectTagging replaces an object's whole tag set, so the existing tags are
// read and written back with only spawn:retain changed. A bare
// put-object-tagging would wipe the regeneration tags the rules match on.
//
// Only spawn outputs (objects tagged spawn:run-id) are touched. Anything else
// is reported and left alone: the rules can't match it anyway, and changing
// tags on an object spawn didn't write isn't this command's business.
//
// The read and the write are two calls, so a tag change made by someone else
// between them is lost. The rules only ever read these tags; nothing else in
// spawn writes them.
func SetRetain(ctx context.Context, api RetainAPI, bucket, target string, retain bool) ([]RetainResult, error) {
	keys := []string{target}
	if target == "" || strings.HasSuffix(target, "/") {
		var err error
		if keys, err = listKeys(ctx, api, bucket, target); err != nil {
			return nil, err
		}
		if len(keys) == 0 {
			return nil, fmt.Errorf("no objects under s3://%s/%s", bucket, target)
		}
	}
	want := "false"
	if retain {
		want = "true"
	}
	out := make([]RetainResult, 0, len(keys))
	for _, k := range keys {
		out = append(out, setRetainOne(ctx, api, bucket, k, want))
	}
	return out, nil
}

func setRetainOne(ctx context.Context, api RetainAPI, bucket, key, want string) RetainResult {
	res := RetainResult{Key: key}
	fail := func(err error) RetainResult { res.Outcome, res.Error = RetainError, err.Error(); return res }
	got, err := api.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return fail(fmt.Errorf("read tags: %w", err))
	}
	tags := map[string]string{}
	for _, t := range got.TagSet {
		tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	if tags[TagRunID] == "" {
		res.Outcome = RetainNotOutput
		return res
	}
	if tags[TagRetain] == want {
		res.Outcome = RetainUnchanged
		return res
	}
	tags[TagRetain] = want
	if len(tags) > maxObjectTags {
		return fail(fmt.Errorf("adding %s would give it %d tags; S3 allows %d", TagRetain, len(tags), maxObjectTags))
	}
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	set := make([]s3types.Tag, 0, len(keys))
	for _, k := range keys {
		set = append(set, s3types.Tag{Key: aws.String(k), Value: aws.String(tags[k])})
	}
	in := &s3.PutObjectTaggingInput{Bucket: aws.String(bucket), Key: aws.String(key), Tagging: &s3types.Tagging{TagSet: set}}
	if got.VersionId != nil {
		in.VersionId = got.VersionId // the version whose tags were read, not whatever is current by now
	}
	if _, err := api.PutObjectTagging(ctx, in); err != nil {
		return fail(fmt.Errorf("write tags: %w", err))
	}
	res.Outcome = RetainSet
	return res
}

func listKeys(ctx context.Context, api RetainAPI, bucket, prefix string) ([]string, error) {
	in := &s3.ListObjectsV2Input{Bucket: aws.String(bucket)}
	if prefix != "" {
		in.Prefix = aws.String(prefix)
	}
	var keys []string
	p := s3.NewListObjectsV2Paginator(api, in)
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list s3://%s/%s: %w", bucket, prefix, err)
		}
		for _, o := range page.Contents {
			keys = append(keys, aws.ToString(o.Key))
		}
	}
	return keys, nil
}
