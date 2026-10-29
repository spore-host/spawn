package main

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/aws/smithy-go"
	"github.com/spore-host/spawn/pkg/schedulesweep/schedulesweeptest"
)

// paramsReaper is the shared scenario as the reaper's own account. No member
// account and every other sweep off, so only the params sweep acts.
func paramsReaper(mode string, dryRun bool) (*reaper, *schedulesweeptest.Scenario) {
	s := schedulesweeptest.New()
	c := s.Clients()
	r := &reaper{
		maxAge:             defaultMaxAge,
		dryRun:             dryRun,
		netResources:       netResourcesOff,
		spotRequests:       spotRequestsOff,
		elasticIPs:         elasticIPsOff,
		instanceRoles:      instanceRolesOff,
		emptyBuckets:       emptyBucketsOff,
		poolQueues:         poolQueuesOff,
		scheduleParams:     mode,
		schedParams:        &c,
		schedParamsAccount: func(context.Context) (string, error) { return schedulesweeptest.Account, nil },
	}
	return r, s
}

// TestScheduleParamsSweepAgreesWithTheCLI: reap, through the reaper's own
// run, deletes exactly what the shared scenario says, which is also what
// cmd's TestCleanupScheduleParams holds `spawn cleanup --schedule-params`
// to. Params a record names (by id or by key), young params, current and
// pre-#1399 queue configs, untagged or mismatched objects and anything in a
// misnamed bucket all survive.
func TestScheduleParamsSweepAgreesWithTheCLI(t *testing.T) {
	r, s := paramsReaper(scheduleParamsReap, false)
	var sum Summary
	out := captureLogs(func() { sum, _ = r.run(context.Background()) })
	got := sortedCopy(s.S3.Deleted)
	if strings.Join(got, ",") != strings.Join(schedulesweeptest.WantDeleted, ",") {
		t.Errorf("reap deleted %v, want %v\n%s", got, schedulesweeptest.WantDeleted, out)
	}
	if sum.ScheduleParamsScanned != schedulesweeptest.WantScanned || sum.ScheduleParamsDeleted != 2 ||
		sum.ScheduleParamsWouldDelete != 0 || sum.ScheduleParamsSkipped != schedulesweeptest.WantScanned-2 ||
		sum.ScheduleParamsFailed != 0 || sum.ScheduleParamsUnreadable != 0 {
		t.Errorf("summary scanned/deleted/would/skipped/failed/unreadable = %d/%d/%d/%d/%d/%d",
			sum.ScheduleParamsScanned, sum.ScheduleParamsDeleted, sum.ScheduleParamsWouldDelete,
			sum.ScheduleParamsSkipped, sum.ScheduleParamsFailed, sum.ScheduleParamsUnreadable)
	}
	for _, k := range schedulesweeptest.WantDeleted {
		if !strings.Contains(out, "DELETED schedule params s3://"+k) {
			t.Errorf("no DELETED line for %s:\n%s", k, out)
		}
	}
	if len(s.S3.BadOwner) != 0 || len(s.S3.WrongRegion) != 0 {
		t.Errorf("S3 calls without the account as owner %v, or in the wrong region %v", s.S3.BadOwner, s.S3.WrongRegion)
	}
}

var wouldParamsRe = regexp.MustCompile(`WOULD delete schedule params s3://(\S+)`)

// report (the default) and a dry run over reap change nothing and log
// exactly what reap deletes; off reads nothing at all.
func TestScheduleParamsSweepModes(t *testing.T) {
	if parseScheduleParams("") != scheduleParamsReport {
		t.Errorf("unset REAPER_SCHEDULE_PARAMS = %q, want report", parseScheduleParams(""))
	}
	for _, c := range []struct {
		name   string
		mode   string
		dryRun bool
	}{{"report", scheduleParamsReport, false}, {"dry run beats reap", scheduleParamsReap, true}} {
		t.Run(c.name, func(t *testing.T) {
			r, s := paramsReaper(c.mode, c.dryRun)
			var sum Summary
			out := captureLogs(func() { sum, _ = r.run(context.Background()) })
			if len(s.S3.Deleted) != 0 {
				t.Errorf("deleted %v", s.S3.Deleted)
			}
			var would []string
			for _, m := range wouldParamsRe.FindAllStringSubmatch(out, -1) {
				would = append(would, m[1])
			}
			sort.Strings(would)
			if strings.Join(would, ",") != strings.Join(schedulesweeptest.WantDeleted, ",") {
				t.Errorf("WOULD lines name %v, want %v\n%s", would, schedulesweeptest.WantDeleted, out)
			}
			if sum.ScheduleParamsWouldDelete != len(schedulesweeptest.WantDeleted) || sum.ScheduleParamsDeleted != 0 {
				t.Errorf("would/deleted = %d/%d", sum.ScheduleParamsWouldDelete, sum.ScheduleParamsDeleted)
			}
		})
	}
	t.Run("off", func(t *testing.T) {
		r, s := paramsReaper(scheduleParamsOff, false)
		out := captureLogs(func() { _, _ = r.run(context.Background()) })
		if s.DB.Scans != 0 || len(s.S3.Deleted) != 0 || strings.Contains(out, "schedule params") {
			t.Errorf("off scanned the table %d time(s), deleted %v:\n%s", s.DB.Scans, s.S3.Deleted, out)
		}
	})
}

// Fail closed: a schedules table the reaper cannot read completely deletes
// nothing, and the run is counted in schedule_params_unreadable (and in
// Errors, unless the cause is AccessDenied: a standing configuration fact).
func TestScheduleParamsUnreadableTableDeletesNothing(t *testing.T) {
	for _, c := range []struct {
		name       string
		err        error
		later      bool
		wantErrors int
	}{
		{"throttled", errors.New("throttled"), false, 1},
		{"a later page", errors.New("throttled"), true, 1},
		{"denied", &smithy.GenericAPIError{Code: "AccessDeniedException"}, false, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, s := paramsReaper(scheduleParamsReap, false)
			s.DB.Err, s.DB.ErrAfterFirstPage = c.err, c.later
			var sum Summary
			out := captureLogs(func() { sum, _ = r.run(context.Background()) })
			if len(s.S3.Deleted) != 0 {
				t.Errorf("deleted %v with the table unreadable", s.S3.Deleted)
			}
			if sum.ScheduleParamsUnreadable != 1 || sum.Errors != c.wantErrors {
				t.Errorf("unreadable/errors = %d/%d, want 1/%d\n%s", sum.ScheduleParamsUnreadable, sum.Errors, c.wantErrors, out)
			}
			if !strings.Contains(out, "nothing was deleted") {
				t.Errorf("no log line says nothing was deleted:\n%s", out)
			}
		})
	}
}
