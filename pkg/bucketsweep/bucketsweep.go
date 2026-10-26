// Package bucketsweep decides when one of the two per-account, per-region
// buckets spawn creates may be deleted, and deletes it (#1301).
//
// spawn creates spawn-staging-<account>-<region> (the task wrapper and flush
// hook, every object expiring after a day, #1293) and
// spawn-results-<account>-<region> (task records and outputs) on first use,
// and before this nothing ever removed either. An empty one is clutter, and
// anything spore.host creates it must remove when it is no longer needed
// (spore-host#610). The data in a results bucket is never deleted here: only
// a bucket that is already empty is, and deleting data stays opt-in (#1076).
//
// A bucket is deleted only when ALL of these hold:
//
//   - it is exactly spawn-staging-<account>-<region> or
//     spawn-results-<account>-<region> for the account and region swept, and
//     every call names that account as the expected bucket owner, so a bucket
//     of that name owned by anyone else is refused by S3 itself;
//   - it is tagged spawn:managed=true. The one exception is a results bucket
//     with no tag at all on that exact name: CreateS3BucketIfNotExists has
//     always tagged it, but ignored a tagging failure, so an untagged one can
//     be spawn's. The exact name and the owner check are what make it ours;
//   - it is not holding an FSx stack's configuration (spawn:fsx-stack-name),
//     and, for a results bucket, has no lifecycle rules: a re-created bucket
//     would not get them back, so deleting it would quietly drop the tiering
//     `spawn results lifecycle --apply` installed;
//   - it is empty: no object versions, no delete markers (so a versioned
//     bucket counts), and no multipart upload in progress. The results
//     bucket is listed to find out. The staging bucket is never listed —
//     its unguessable keys are what keeps one task's staged wrapper from
//     another task's role (#1274), and no role spawn makes may list it — so
//     its emptiness is S3's own check: DeleteBucket refuses a bucket holding
//     any object, version or delete marker (BucketNotEmpty), and the
//     refusal counts as "in use". Its objects all expire after a day, and a
//     launch removes the stamp, so its grace measures a week with no launch;
//   - it has been seen empty for at least the grace. S3 records no "empty
//     since", so the first sweep to find it empty tags it spawn:empty-since
//     (RFC 3339, UTC), a sweep that finds it non-empty removes the tag, and a
//     launch that uses the bucket removes it too (ClearEmptySince). Deletion
//     waits until the tag is older than the grace.
//
// S3 refuses to delete a bucket that is not empty, so an object written between
// the check and the delete fails the delete rather than being lost.
//
// Deleting either bucket is safe because spawn re-creates it on next use:
// EnsureStagingBucket and CreateS3BucketIfNotExists in pkg/aws.
//
// Kept free of pkg/aws so the TTL reaper (a nested module) can import it.
package bucketsweep

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/spore-host/spawn/pkg/taskproto"
)

const (
	// ManagedTagKey is spawn's ownership tag.
	ManagedTagKey = "spawn:managed"
	// EmptySinceTagKey records when a sweep first found the bucket empty.
	EmptySinceTagKey = "spawn:empty-since"
	// FSxStackTagKey marks a bucket holding an FSx stack's configuration
	// (pkg/aws.CreateS3BucketWithTags). Such a bucket is never deleted here.
	FSxStackTagKey = "spawn:fsx-stack-name"

	// Grace is how long a bucket must have been empty before it is deleted.
	// A week: long past the staging bucket's one-day object expiry, and the
	// reaper's default max age, so no instance a task launched can still be
	// running when its bucket goes.
	Grace = 7 * 24 * time.Hour
)

// Kind is which of spawn's buckets this is.
type Kind string

const (
	KindStaging Kind = "staging"
	KindResults Kind = "results"
)

// Bucket is one bucket to sweep.
type Bucket struct {
	Name   string
	Region string
	Kind   Kind
}

// Buckets is the set a sweep of account in region looks at: the staging and
// results buckets spawn creates there. Nothing else is ever considered.
func Buckets(account, region string) []Bucket {
	return []Bucket{
		{Name: taskproto.StagingBucket(account, region), Region: region, Kind: KindStaging},
		{Name: taskproto.DefaultResultsBucket(account, region), Region: region, Kind: KindResults},
	}
}

// IsSweptName reports whether name is one of the buckets Buckets returns for
// some account and region, by prefix. For inventories (spawn footprint).
func IsSweptName(name string) bool {
	return strings.HasPrefix(name, taskproto.StagingBucketPrefix) || strings.HasPrefix(name, "spawn-results-")
}

// API is the S3 slice a sweep uses. *s3.Client satisfies it.
type API interface {
	HeadBucket(ctx context.Context, in *s3.HeadBucketInput, optFns ...func(*s3.Options)) (*s3.HeadBucketOutput, error)
	GetBucketTagging(ctx context.Context, in *s3.GetBucketTaggingInput, optFns ...func(*s3.Options)) (*s3.GetBucketTaggingOutput, error)
	PutBucketTagging(ctx context.Context, in *s3.PutBucketTaggingInput, optFns ...func(*s3.Options)) (*s3.PutBucketTaggingOutput, error)
	DeleteBucketTagging(ctx context.Context, in *s3.DeleteBucketTaggingInput, optFns ...func(*s3.Options)) (*s3.DeleteBucketTaggingOutput, error)
	ListObjectVersions(ctx context.Context, in *s3.ListObjectVersionsInput, optFns ...func(*s3.Options)) (*s3.ListObjectVersionsOutput, error)
	ListMultipartUploads(ctx context.Context, in *s3.ListMultipartUploadsInput, optFns ...func(*s3.Options)) (*s3.ListMultipartUploadsOutput, error)
	GetBucketLifecycleConfiguration(ctx context.Context, in *s3.GetBucketLifecycleConfigurationInput, optFns ...func(*s3.Options)) (*s3.GetBucketLifecycleConfigurationOutput, error)
	DeleteBucket(ctx context.Context, in *s3.DeleteBucketInput, optFns ...func(*s3.Options)) (*s3.DeleteBucketOutput, error)
}

// ClientsFor returns a regional S3 client factory over cfg's credentials.
// Each bucket lives in the region its name ends in, and S3 answers a request
// for it from any other region with a redirect, not the bucket.
func ClientsFor(cfg aws.Config) func(region string) API {
	return func(region string) API {
		c := cfg.Copy()
		c.Region = region
		return s3.NewFromConfig(c)
	}
}

// Action is what a sweep did, or would do, with one bucket.
type Action string

const (
	// Absent: there is no such bucket.
	Absent Action = "absent"
	// Kept: not spawn's to delete (Reason says why). Never deleted.
	Kept Action = "kept"
	// InUse: it holds something. Its spawn:empty-since stamp, if any, was
	// removed (or would be, without Writes).
	InUse Action = "in-use"
	// Stamped: empty, and seen so for the first time: spawn:empty-since was
	// written (or would be, without Writes).
	Stamped Action = "stamped"
	// Waiting: empty, stamped, and not yet past the grace.
	Waiting Action = "waiting"
	// WouldDelete: qualifies for deletion, and Delete was not set.
	WouldDelete Action = "would-delete"
	// Deleted: deleted.
	Deleted Action = "deleted"
	// Failed: an S3 call failed (Err). Nothing further was done.
	Failed Action = "failed"
)

// Options controls what a sweep may change.
type Options struct {
	// Account owns the buckets; every call sends it as ExpectedBucketOwner.
	Account string
	// Now is the sweep's clock.
	Now time.Time
	// Grace is how long a bucket must have been empty; zero means Grace.
	Grace time.Duration
	// Writes lets the sweep write and remove the spawn:empty-since tag.
	// Without it the sweep changes nothing at all.
	Writes bool
	// Delete lets the sweep delete a bucket that qualifies. Needs Writes.
	Delete bool
}

// Result is one bucket's outcome.
type Result struct {
	Bucket Bucket
	Action Action
	// Reason explains Kept, InUse and Failed in a phrase.
	Reason string
	// EmptySince is the stamp in force, when there is one.
	EmptySince time.Time
	Err        error
}

// Sweep examines one bucket and, as opts allows, stamps, unstamps or deletes it.
func Sweep(ctx context.Context, api API, b Bucket, opts Options) Result {
	res := Result{Bucket: b}
	if opts.Account == "" {
		return failed(res, "no owning account to check", errors.New("bucketsweep: Options.Account is required"))
	}
	grace := opts.Grace
	if grace <= 0 {
		grace = Grace
	}
	owner := aws.String(opts.Account)
	name := aws.String(b.Name)

	if !isSpawnName(b, opts.Account) {
		return kept(res, "not spawn's "+string(b.Kind)+" bucket for account "+opts.Account+" in "+b.Region)
	}

	// The staging bucket is never listed, not even HeadBucket (which IAM
	// authorizes as s3:ListBucket): its keys are what keeps one task's staged
	// wrapper from another task's role (#1274), so no role spawn makes may
	// list it. Its existence comes from the tagging read, and its emptiness
	// from S3 itself, which refuses to delete a bucket holding anything.
	if b.Kind == KindResults {
		// HeadBucket says "no such bucket" (404) apart from "not yours, or not
		// allowed" (403), which a missing bucket must never be confused with.
		if _, err := api.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: name, ExpectedBucketOwner: owner}); err != nil {
			if isCode(err, "NotFound") || isCode(err, "NoSuchBucket") {
				res.Action = Absent
				return res
			}
			return failed(res, "not usable as account "+opts.Account+"'s bucket (it may belong to another account)", err)
		}
	}
	tags, err := getTags(ctx, api, b.Name, opts.Account)
	switch {
	case isCode(err, "NoSuchBucket"):
		res.Action = Absent
		return res
	case err != nil:
		return failed(res, "could not read its tags as account "+opts.Account+"'s bucket (it may belong to another account)", err)
	}
	if ts, ok := parseStamp(tags[EmptySinceTagKey]); ok {
		res.EmptySince = ts
	}

	switch {
	case tags[ManagedTagKey] == "true":
	case len(without(tags, EmptySinceTagKey)) == 0 && b.Kind == KindResults:
		// Untagged on spawn's exact name (apart from this package's own stamp),
		// and owned by this account, since the tagging read above named it as
		// the expected owner: a results bucket whose creation-time tagging
		// failed, which CreateS3BucketIfNotExists ignored.
	default:
		return kept(res, "not tagged "+ManagedTagKey+"=true")
	}

	// Emptiness before the remaining keep rules, so a stamp is removed from a
	// bucket in use whatever else is true of it: a stale stamp left on a kept
	// bucket would let it be deleted the moment it stopped being kept.
	if b.Kind == KindResults {
		busy, why, err := occupied(ctx, api, name, owner)
		if err != nil {
			return failed(res, "could not check whether it is empty", err)
		}
		if busy {
			return inUse(ctx, api, res, tags, why, opts)
		}
	}

	if tags[FSxStackTagKey] != "" {
		return kept(res, "holds FSx stack "+tags[FSxStackTagKey]+"'s configuration in its tags")
	}
	if b.Kind == KindResults {
		lc, err := api.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: name, ExpectedBucketOwner: owner})
		switch {
		case isCode(err, "NoSuchLifecycleConfiguration"):
		case err != nil:
			return failed(res, "could not read its lifecycle configuration", err)
		case len(lc.Rules) > 0:
			return kept(res, fmt.Sprintf("has %d lifecycle rule(s) that a re-created bucket would not get back", len(lc.Rules)))
		}
	}

	if res.EmptySince.IsZero() || res.EmptySince.After(opts.Now) {
		// No usable stamp (absent, unparseable, or in the future): this is the
		// first empty sighting, and the grace starts now.
		res.Action, res.EmptySince = Stamped, opts.Now.UTC()
		if opts.Writes {
			tags[EmptySinceTagKey] = opts.Now.UTC().Format(time.RFC3339)
			if err := writeTags(ctx, api, b.Name, opts.Account, tags); err != nil {
				return failed(res, "could not write "+EmptySinceTagKey, err)
			}
		}
		return res
	}
	if opts.Now.Sub(res.EmptySince) < grace {
		res.Action = Waiting
		return res
	}
	if !opts.Delete || !opts.Writes {
		res.Action = WouldDelete
		return res
	}
	if _, err := api.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: name, ExpectedBucketOwner: owner}); err != nil {
		if isCode(err, "BucketNotEmpty") {
			// For the staging bucket this is the emptiness check; for the
			// results bucket, something was written since the listing.
			return inUse(ctx, api, res, tags, "S3 refused to delete it: it is not empty", opts)
		}
		return failed(res, "delete failed", err)
	}
	res.Action = Deleted
	return res
}

// ClearEmptySince removes spawn:empty-since from bucket, if it carries one: a
// launch about to use the bucket calls it, so a bucket in use is never aged
// toward deletion between two sweeps that both happened to find it empty.
// owner may be empty when the caller does not know it; S3 then checks nothing.
func ClearEmptySince(ctx context.Context, api TagAPI, bucket, owner string) error {
	tags, err := getTags(ctx, api, bucket, owner)
	if err != nil {
		if isCode(err, "NoSuchBucket") {
			return nil
		}
		return err
	}
	if _, ok := tags[EmptySinceTagKey]; !ok {
		return nil
	}
	return writeTags(ctx, api, bucket, owner, without(tags, EmptySinceTagKey))
}

// TagAPI is the slice ClearEmptySince needs.
type TagAPI interface {
	GetBucketTagging(ctx context.Context, in *s3.GetBucketTaggingInput, optFns ...func(*s3.Options)) (*s3.GetBucketTaggingOutput, error)
	PutBucketTagging(ctx context.Context, in *s3.PutBucketTaggingInput, optFns ...func(*s3.Options)) (*s3.PutBucketTaggingOutput, error)
	DeleteBucketTagging(ctx context.Context, in *s3.DeleteBucketTaggingInput, optFns ...func(*s3.Options)) (*s3.DeleteBucketTaggingOutput, error)
}

// EmptySince parses a bucket's spawn:empty-since tag value, for inventories.
func EmptySince(v string) (time.Time, bool) { return parseStamp(v) }

func isSpawnName(b Bucket, account string) bool {
	for _, want := range Buckets(account, b.Region) {
		if want.Kind == b.Kind && want.Name == b.Name {
			return true
		}
	}
	return false
}

// occupied reports whether the bucket holds anything: any object version or
// delete marker (one call covers unversioned buckets too, whose objects are
// listed as versions), or an in-progress multipart upload.
func occupied(ctx context.Context, api API, name, owner *string) (bool, string, error) {
	v, err := api.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: name, ExpectedBucketOwner: owner, MaxKeys: aws.Int32(1)})
	if err != nil {
		return false, "", err
	}
	switch {
	case len(v.Versions) > 0:
		return true, "holds objects or object versions", nil
	case len(v.DeleteMarkers) > 0:
		return true, "holds delete markers", nil
	}
	u, err := api.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: name, ExpectedBucketOwner: owner, MaxUploads: aws.Int32(1)})
	if err != nil {
		return false, "", err
	}
	if len(u.Uploads) > 0 {
		return true, "has a multipart upload in progress", nil
	}
	return false, "", nil
}

func getTags(ctx context.Context, api TagAPI, bucket, owner string) (map[string]string, error) {
	in := &s3.GetBucketTaggingInput{Bucket: aws.String(bucket)}
	if owner != "" {
		in.ExpectedBucketOwner = aws.String(owner)
	}
	out, err := api.GetBucketTagging(ctx, in)
	tags := map[string]string{}
	if isCode(err, "NoSuchTagSet") {
		return tags, nil
	}
	if err != nil {
		return nil, err
	}
	for _, t := range out.TagSet {
		tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return tags, nil
}

// writeTags replaces the bucket's whole tag set, which is the only way S3
// allows: so it always writes back every tag it read.
func writeTags(ctx context.Context, api TagAPI, bucket, owner string, tags map[string]string) error {
	var o *string
	if owner != "" {
		o = aws.String(owner)
	}
	if len(tags) == 0 {
		_, err := api.DeleteBucketTagging(ctx, &s3.DeleteBucketTaggingInput{Bucket: aws.String(bucket), ExpectedBucketOwner: o})
		return err
	}
	set := make([]types.Tag, 0, len(tags))
	for k, v := range tags {
		set = append(set, types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	_, err := api.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{Bucket: aws.String(bucket), ExpectedBucketOwner: o, Tagging: &types.Tagging{TagSet: set}})
	return err
}

func without(tags map[string]string, key string) map[string]string {
	out := make(map[string]string, len(tags))
	for k, v := range tags {
		if k != key {
			out[k] = v
		}
	}
	return out
}

func parseStamp(v string) (time.Time, bool) {
	if v == "" {
		return time.Time{}, false
	}
	ts, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, false
	}
	return ts.UTC(), true
}

func isCode(err error, code string) bool {
	var apiErr smithy.APIError
	return err != nil && errors.As(err, &apiErr) && apiErr.ErrorCode() == code
}

// inUse records a bucket that holds something, and removes its stamp so the
// grace restarts when it next empties.
func inUse(ctx context.Context, api API, res Result, tags map[string]string, why string, opts Options) Result {
	res.Action, res.Reason = InUse, why
	if _, stamped := tags[EmptySinceTagKey]; stamped && opts.Writes {
		if err := writeTags(ctx, api, res.Bucket.Name, opts.Account, without(tags, EmptySinceTagKey)); err != nil {
			return failed(res, "could not remove "+EmptySinceTagKey, err)
		}
	}
	res.EmptySince = time.Time{}
	return res
}

func kept(r Result, why string) Result {
	r.Action, r.Reason = Kept, why
	return r
}

func failed(r Result, why string, err error) Result {
	r.Action, r.Reason, r.Err = Failed, why, err
	return r
}
