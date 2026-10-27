package main

import (
	"context"
	"log"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/spore-host/spawn/pkg/instancerole"
	"github.com/spore-host/spawn/pkg/poolqueue"
	"github.com/spore-host/spawn/pkg/reaperconfig"
)

// The abandoned-pool sweep (#1351): the SQS queue `spawn pool create` makes
// (spawn-pool-<run id>) and the specs `spawn pool submit` stages, for a pool
// nobody drained. pkg/poolqueue holds the rule and the deletes; this file
// wires it to the reaper's accounts and its REAPER_POOL_QUEUES switch.
//
// Once per account per run, after the regional scans: the worker check covers
// every region enabled in the account (DescribeRegions, plus REAPER_REGIONS),
// since a pool's workers may be stopped in a region the reaper does not scan,
// and the queues are then swept in each scanned region. If the region list or
// any region cannot be read, nothing in that account is deleted that run.
const (
	poolQueuesOff    = reaperconfig.ModeOff
	poolQueuesReport = reaperconfig.ModeReport
	poolQueuesReap   = reaperconfig.ModeReap
)

// sqsAPI is the SQS slice the pool sweep uses: poolqueue.SQSAPI's methods.
type sqsAPI interface {
	ListQueues(ctx context.Context, in *sqs.ListQueuesInput, optFns ...func(*sqs.Options)) (*sqs.ListQueuesOutput, error)
	ListQueueTags(ctx context.Context, in *sqs.ListQueueTagsInput, optFns ...func(*sqs.Options)) (*sqs.ListQueueTagsOutput, error)
	GetQueueAttributes(ctx context.Context, in *sqs.GetQueueAttributesInput, optFns ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error)
	DeleteQueue(ctx context.Context, in *sqs.DeleteQueueInput, optFns ...func(*sqs.Options)) (*sqs.DeleteQueueOutput, error)
}

// poolSpecsAPI is the S3 slice for the pool's staged specs: poolqueue.S3API.
// Separate from s3API so the empty-bucket fakes need not grow.
type poolSpecsAPI interface {
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	DeleteObjects(ctx context.Context, in *s3.DeleteObjectsInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error)
}

// parsePoolQueues maps REAPER_POOL_QUEUES onto a mode. Unset and any
// unrecognised value are 'report', as for the other deletion sweeps added
// after the reaper shipped (REAPER_INSTANCE_ROLES, REAPER_EMPTY_BUCKETS):
// report logs and changes nothing, so an upgrade deletes nothing by itself.
func parsePoolQueues(v string) string {
	if m, ok := reaperconfig.ParseMode(v); ok {
		return m
	}
	return poolQueuesReport
}

// reapPoolQueuesAccount runs the pool sweep for one account.
func (r *reaper) reapPoolQueuesAccount(ctx context.Context, acct account, now time.Time, sum *Summary) {
	if acct.sqsFor == nil || r.poolQueues == poolQueuesOff {
		return
	}
	reap := r.poolQueues == poolQueuesReap && !r.dryRun

	if acct.regionsFor == nil {
		log.Printf("pool-queues: %s: not swept, no way to list the account's enabled regions", acct.label)
		return
	}
	enabled, err := instancerole.EnabledRegions(ctx, acct.regionsFor(), r.regions...)
	if err != nil {
		log.Printf("pool-queues: %s: not swept, could not list enabled regions (%s): %v",
			acct.label, classifyScanError(err), err)
		if classifyScanError(err) != failureDenied {
			sum.Errors++
		}
		return
	}
	regions := make(map[string]instancerole.EC2Describer, len(enabled))
	for _, region := range enabled {
		regions[region] = acct.ec2For(region)
	}
	inUse, err := poolqueue.WorkersInUse(ctx, regions)
	if err != nil {
		// Fail closed: a pool with workers in the region we could not read
		// would look abandoned.
		log.Printf("pool-queues: %s: not swept, could not list instances (%s): %v",
			acct.label, classifyScanError(err), err)
		if classifyScanError(err) != failureDenied {
			sum.Errors++
		}
		return
	}

	denied := false
	for _, region := range r.regions {
		var specs poolqueue.S3API
		if acct.specsFor != nil {
			specs = acct.specsFor(region)
		}
		res, err := poolqueue.Sweep(ctx, acct.sqsFor(region), specs, inUse, poolqueue.Options{
			Account: acct.accountID, Region: region, Now: now, Delete: reap,
		})
		if err != nil {
			cls := classifyScanError(err)
			log.Printf("pool-queues: %s/%s: not swept (%s): %v", acct.label, region, cls, err)
			if cls == failureDenied {
				// The member role predates the #1351 grant: a standing
				// configuration fact, counted apart from Errors.
				denied = true
			} else {
				sum.Errors++
			}
			continue
		}
		for _, d := range res.Queues {
			switch {
			case d.Deleted:
				log.Printf("DELETED pool queue %s in %s/%s (created %s)", d.Name, acct.label, region, stamp(d.Created))
			case d.Delete && d.Err != nil:
				log.Printf("pool-queues: %s/%s: delete queue %s failed: %v", acct.label, region, d.Name, d.Err)
				sum.Errors++
			case d.Delete:
				log.Printf("WOULD delete pool queue %s in %s/%s (created %s; REAPER_POOL_QUEUES=%s, dry-run=%t)",
					d.Name, acct.label, region, stamp(d.Created), r.poolQueues, r.dryRun)
			case d.Err != nil:
				log.Printf("pool-queues: %s/%s: kept queue %s: %s: %v", acct.label, region, d.Name, d.Reason, d.Err)
			}
		}
		for _, d := range res.Specs {
			switch {
			case d.Deleted:
				log.Printf("DELETED %d staged pool spec(s) under s3://%s/%s in %s", d.Objects, d.Bucket, d.Prefix, acct.label)
			case d.Err != nil:
				log.Printf("pool-queues: %s/%s: specs s3://%s/%s: %s: %v", acct.label, region, d.Bucket, d.Prefix, d.Reason, d.Err)
			case d.Delete:
				log.Printf("WOULD delete %d staged pool spec(s) under s3://%s/%s in %s (REAPER_POOL_QUEUES=%s, dry-run=%t)",
					d.Objects, d.Bucket, d.Prefix, acct.label, r.poolQueues, r.dryRun)
			}
		}
		if res.SpecsErr != nil {
			log.Printf("pool-queues: %s/%s: staged specs not checked (%s): %v", acct.label, region, classifyScanError(res.SpecsErr), res.SpecsErr)
		}
		scanned, deleted, would, skipped := res.Count()
		sum.PoolQueuesScanned += scanned
		sum.PoolQueuesDeleted += deleted
		sum.PoolQueuesWouldDelete += would
		sum.PoolQueuesSkipped += skipped
		sd, sw, sf := res.SpecCount()
		sum.PoolSpecsDeleted += sd
		sum.PoolSpecsWouldDelete += sw
		sum.PoolSpecsFailed += sf
	}
	if denied {
		sum.PoolQueuesAccountsDenied++
	}
}
