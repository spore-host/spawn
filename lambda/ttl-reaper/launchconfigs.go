package main

import (
	"context"
	"log"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spore-host/spawn/pkg/launchconfig"
	"github.com/spore-host/spawn/pkg/reaperconfig"
)

// The launch config object sweep (#1359): the named deleter of the
// launch-config/config-<sha256>.json objects a launch stores in the account's
// staging bucket. pkg/launchconfig holds the rule and the deletes; this file
// wires it to the reaper's accounts, regions and its REAPER_LAUNCH_CONFIGS
// switch.
//
// Per scanned region, after the instance reaps: the region's instances and
// Spot requests that still carry a spawn:config, then the objects in that
// region's staging bucket. If the references cannot be read, nothing in that
// region is deleted that run. The staging bucket's 30-day lifecycle rule is
// the backstop for a region the reaper does not scan.
const (
	launchConfigsOff    = reaperconfig.ModeOff
	launchConfigsReport = reaperconfig.ModeReport
	launchConfigsReap   = reaperconfig.ModeReap
)

// launchConfigS3API is the S3 slice the sweep uses: launchconfig.S3API's
// methods, declared here so the IAM drift gate in pkg/reaperiam sees them.
type launchConfigS3API interface {
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	DeleteObject(ctx context.Context, in *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
}

// parseLaunchConfigs maps REAPER_LAUNCH_CONFIGS onto a mode. Unset and any
// unrecognised value are 'report', as for every deletion sweep added after the
// reaper shipped: an upgrade deletes nothing by itself.
func parseLaunchConfigs(v string) string {
	if m, ok := reaperconfig.ParseMode(v); ok {
		return m
	}
	return launchConfigsReport
}

// reapLaunchConfigsRegion runs the sweep for one account and region.
func (r *reaper) reapLaunchConfigsRegion(ctx context.Context, acct account, region string, now time.Time, sum *Summary) {
	if r.launchConfigs == launchConfigsOff || acct.configsFor == nil || acct.spotFor == nil {
		return
	}
	if acct.accountID == "" {
		log.Printf("launch-configs: %s/%s: account id unknown, so its staging bucket cannot be named; skipped", acct.label, region)
		return
	}
	reap := r.launchConfigs == launchConfigsReap && !r.dryRun

	refs, err := launchconfig.Referenced(ctx, acct.spotFor(region), launchconfig.Tag)
	if err != nil {
		// Fail closed: an object an instance we could not see still needs
		// would look unreferenced.
		cls := classifyScanError(err)
		log.Printf("launch-configs: %s/%s: not swept, could not read which instances use them (%s): %v", acct.label, region, cls, err)
		if cls != failureDenied {
			sum.Errors++
		}
		return
	}
	res, err := launchconfig.Sweep(ctx, acct.configsFor(region), refs, launchconfig.SweepOptions{
		Account: acct.accountID, Region: region, Now: now, Delete: reap,
	})
	if err != nil {
		cls := classifyScanError(err)
		log.Printf("launch-configs: %s/%s: not swept (%s): %v", acct.label, region, cls, err)
		if cls == failureDenied {
			// The member role predates the #1359 grant: a standing
			// configuration fact, counted apart from Errors.
			sum.LaunchConfigsAccountsDenied++
		} else {
			sum.Errors++
		}
		return
	}
	for _, d := range res.Decisions {
		switch {
		case d.Deleted:
			log.Printf("DELETED launch config object s3://%s/%s in %s (written %s, no instance or Spot request references it)",
				res.Bucket, d.Key, acct.label, stamp(d.LastModified))
		case d.Delete && d.Err != nil:
			log.Printf("launch-configs: %s/%s: delete s3://%s/%s failed: %v", acct.label, region, res.Bucket, d.Key, d.Err)
			sum.Errors++
		case d.Delete:
			log.Printf("WOULD delete launch config object s3://%s/%s in %s (written %s; REAPER_LAUNCH_CONFIGS=%s, dry-run=%t)",
				res.Bucket, d.Key, acct.label, stamp(d.LastModified), r.launchConfigs, r.dryRun)
		}
	}
	listed, deleted, would, failed := res.Count()
	sum.LaunchConfigsScanned += listed
	sum.LaunchConfigsDeleted += deleted
	sum.LaunchConfigsWouldDelete += would
	sum.LaunchConfigsFailed += failed
}
