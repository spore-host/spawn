package main

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/spore-host/spawn/pkg/bucketsweep"
	"github.com/spore-host/spawn/pkg/reaperconfig"
)

// s3API is the S3 slice the empty-bucket sweep needs (#1301): the methods of
// bucketsweep.API, declared here so the IAM drift gate in pkg/reaperiam, which
// reads this package's *API interfaces, sees every S3 call the reaper makes.
type s3API interface {
	HeadBucket(ctx context.Context, in *s3.HeadBucketInput, optFns ...func(*s3.Options)) (*s3.HeadBucketOutput, error)
	GetBucketTagging(ctx context.Context, in *s3.GetBucketTaggingInput, optFns ...func(*s3.Options)) (*s3.GetBucketTaggingOutput, error)
	PutBucketTagging(ctx context.Context, in *s3.PutBucketTaggingInput, optFns ...func(*s3.Options)) (*s3.PutBucketTaggingOutput, error)
	DeleteBucketTagging(ctx context.Context, in *s3.DeleteBucketTaggingInput, optFns ...func(*s3.Options)) (*s3.DeleteBucketTaggingOutput, error)
	ListObjectVersions(ctx context.Context, in *s3.ListObjectVersionsInput, optFns ...func(*s3.Options)) (*s3.ListObjectVersionsOutput, error)
	ListMultipartUploads(ctx context.Context, in *s3.ListMultipartUploadsInput, optFns ...func(*s3.Options)) (*s3.ListMultipartUploadsOutput, error)
	GetBucketLifecycleConfiguration(ctx context.Context, in *s3.GetBucketLifecycleConfigurationInput, optFns ...func(*s3.Options)) (*s3.GetBucketLifecycleConfigurationOutput, error)
	DeleteBucket(ctx context.Context, in *s3.DeleteBucketInput, optFns ...func(*s3.Options)) (*s3.DeleteBucketOutput, error)
}

// The three states of the empty-bucket sweep (REAPER_EMPTY_BUCKETS), staged
// like REAPER_SPOT_REQUESTS and for the same reason. 'report' (the default)
// logs what it would do and writes nothing, not even the spawn:empty-since
// tag, so arming 'reap' is what starts a bucket's grace.
const (
	emptyBucketsOff    = reaperconfig.ModeOff
	emptyBucketsReport = reaperconfig.ModeReport
	emptyBucketsReap   = reaperconfig.ModeReap
)

// parseEmptyBuckets maps REAPER_EMPTY_BUCKETS onto a mode. Unset or
// unrecognised is 'report'.
func parseEmptyBuckets(v string) string {
	if m, ok := reaperconfig.ParseMode(v); ok {
		return m
	}
	return emptyBucketsReport
}

// emptyBucketsReportOnly reports whether this cycle may only report. DryRun
// wins, as for every other sweep.
func (r *reaper) emptyBucketsReportOnly() bool {
	return r.dryRun || r.emptyBuckets != emptyBucketsReap
}

func (r *reaper) emptyBucketsHeldBy() string {
	if r.dryRun {
		return "REAPER_DRY_RUN=true"
	}
	return "REAPER_EMPTY_BUCKETS=" + r.emptyBuckets
}

// reapEmptyBucketsRegion sweeps the account's spawn-staging-<account>-<region>
// and spawn-results-<account>-<region> (#1301): deletes one that has been
// empty for bucketsweep.Grace, by the rules in pkg/bucketsweep. Only those two
// exact names are looked at, so the sweep lists no buckets and needs no
// s3:ListAllMyBuckets; every call names the account as the expected owner.
func (r *reaper) reapEmptyBucketsRegion(ctx context.Context, acct account, region string, now time.Time, sum *Summary) {
	if r.emptyBuckets == emptyBucketsOff || acct.s3For == nil {
		return
	}
	if acct.accountID == "" {
		log.Printf("empty-buckets: %s/%s: account id unknown, so its buckets cannot be named; skipped", acct.label, region)
		return
	}
	reportOnly := r.emptyBucketsReportOnly()
	api := acct.s3For(region)
	for _, b := range bucketsweep.Buckets(acct.accountID, region) {
		res := bucketsweep.Sweep(ctx, api, b, bucketsweep.Options{
			Account: acct.accountID, Now: now, Writes: !reportOnly, Delete: !reportOnly,
		})
		if res.Action == bucketsweep.Absent {
			continue
		}
		sum.EmptyBucketsScanned++
		switch res.Action {
		case bucketsweep.Stamped:
			if reportOnly {
				log.Printf("empty-buckets: %s/%s: %s is empty; WOULD stamp %s (held back by %s)",
					acct.label, region, b.Name, bucketsweep.EmptySinceTagKey, r.emptyBucketsHeldBy())
				continue
			}
			sum.EmptyBucketsStamped++
			log.Printf("empty-buckets: %s/%s: %s is empty; stamped %s=%s, deletable after %s",
				acct.label, region, b.Name, bucketsweep.EmptySinceTagKey, res.EmptySince.Format(time.RFC3339),
				res.EmptySince.Add(bucketsweep.Grace).Format(time.RFC3339))
		case bucketsweep.WouldDelete:
			sum.EmptyBucketsWouldDelete++
			log.Printf("empty-buckets: %s/%s: WOULD delete %s, empty since %s (held back by %s)",
				acct.label, region, b.Name, res.EmptySince.Format(time.RFC3339), r.emptyBucketsHeldBy())
		case bucketsweep.Deleted:
			sum.EmptyBucketsDeleted++
			log.Printf("empty-buckets: %s/%s: DELETED %s, empty since %s; spawn re-creates it on next use",
				acct.label, region, b.Name, res.EmptySince.Format(time.RFC3339))
		case bucketsweep.Failed:
			sum.EmptyBucketsFailed++
			hint := ""
			if bucketDenied(res.Err) {
				hint = " (the role may predate the #1301 S3 grants: redeploy the spawn-ttl-reaper-ec2 stack, or the reaper with ScanSelf)"
			}
			log.Printf("empty-buckets: %s/%s: %s: %s: %v%s", acct.label, region, b.Name, res.Reason, res.Err, hint)
		}
	}
}

// bucketDenied: AccessDenied from a body-carrying S3 call, or the bare 403
// HeadBucket answers with (code "Forbidden"), which classifyScanError, written
// for EC2, does not know.
func bucketDenied(err error) bool {
	var apiErr smithy.APIError
	return classifyScanError(err) == failureDenied || (errors.As(err, &apiErr) && apiErr.ErrorCode() == "Forbidden")
}
