package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/spore-host/spawn/pkg/reaperconfig"
	"github.com/spore-host/spawn/pkg/schedulesweep"
)

// The orphaned schedule params sweep (#1398): schedules/<id>/params.yaml in a
// spawn-schedules-* bucket whose schedule record is gone. pkg/schedulesweep
// holds the rule and the deletes; this file wires it to the reaper's own
// credentials and its REAPER_SCHEDULE_PARAMS switch.
//
// Once per run, not per member account: the schedules (table and buckets) are
// in the reaper's OWN account, the infra account `spawn schedule create`
// writes to, the same place the role sweep reads them from (#1378). So it
// runs on the base credentials, and the member accounts' role is unchanged.
// If the table cannot be read completely, nothing is deleted that run and the
// run is counted in schedule_params_unreadable.
const (
	scheduleParamsOff    = reaperconfig.ModeOff
	scheduleParamsReport = reaperconfig.ModeReport
	scheduleParamsReap   = reaperconfig.ModeReap
)

// parseScheduleParams maps REAPER_SCHEDULE_PARAMS onto a mode. Unset and any
// unrecognised value are 'report', as for the other deletion sweeps added
// after the reaper shipped: report logs and changes nothing, so an upgrade
// deletes nothing by itself.
func parseScheduleParams(v string) string {
	if m, ok := reaperconfig.ParseMode(v); ok {
		return m
	}
	return scheduleParamsReport
}

// selfAccount resolves the account of cfg's credentials (STS
// GetCallerIdentity, which needs no permission): the expected owner of the
// schedules buckets.
func selfAccount(cfg aws.Config) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		out, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
		if err != nil {
			return "", err
		}
		if aws.ToString(out.Account) == "" {
			return "", errors.New("GetCallerIdentity returned no account")
		}
		return aws.ToString(out.Account), nil
	}
}

// reapScheduleParams runs the params sweep once.
func (r *reaper) reapScheduleParams(ctx context.Context, now time.Time, sum *Summary) {
	if r.scheduleParams == scheduleParamsOff {
		return
	}
	if r.schedParams == nil || r.schedParamsAccount == nil {
		log.Printf("schedule-params: not swept, no schedules clients configured")
		return
	}
	reap := r.scheduleParams == scheduleParamsReap && !r.dryRun

	unreadable := func(what string, err error) {
		cls := classifyScanError(err)
		log.Printf("schedule-params: not swept, %s (%s); nothing was deleted: %v", what, cls, err)
		sum.ScheduleParamsUnreadable++
		if cls != failureDenied {
			sum.Errors++
		}
	}
	account, err := r.schedParamsAccount(ctx)
	if err != nil {
		unreadable("could not resolve this account (the buckets' expected owner)", err)
		return
	}
	res, err := schedulesweep.Sweep(ctx, *r.schedParams, schedulesweep.Options{Account: account, Now: now, Delete: reap})
	if err != nil {
		what := "could not list the schedules buckets"
		if errors.Is(err, schedulesweep.ErrTableUnreadable) {
			what = "the schedules table could not be read completely"
		}
		unreadable(what, err)
		return
	}
	for _, b := range res.Foreign {
		log.Printf("schedule-params: kept bucket %s: not a name spawn gives a schedules bucket", b)
	}
	for _, e := range res.Errors {
		log.Printf("schedule-params: bucket %s not swept (%s): %v", e.Bucket, classifyScanError(e.Err), e.Err)
	}
	for _, d := range res.Objects {
		loc := fmt.Sprintf("s3://%s/%s", d.Bucket, d.Key)
		switch {
		case d.Deleted:
			log.Printf("DELETED schedule params %s (last modified %s; no schedule record names it)", loc, stamp(d.LastModified))
		case d.Delete && d.Err != nil:
			log.Printf("schedule-params: delete %s failed: %v", loc, d.Err)
			sum.Errors++
		case d.Delete:
			log.Printf("WOULD delete schedule params %s (last modified %s; REAPER_SCHEDULE_PARAMS=%s, dry-run=%t)",
				loc, stamp(d.LastModified), r.scheduleParams, r.dryRun)
		case d.Err != nil:
			log.Printf("schedule-params: kept %s: %s: %v", loc, d.Reason, d.Err)
		}
	}
	scanned, deleted, would, skipped, failed := res.Count()
	sum.ScheduleParamsScanned += scanned
	sum.ScheduleParamsDeleted += deleted
	sum.ScheduleParamsWouldDelete += would
	sum.ScheduleParamsSkipped += skipped
	sum.ScheduleParamsFailed += failed
}
