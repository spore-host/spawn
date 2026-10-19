package cmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
)

func fakeLister(insts []aws.InstanceInfo, err error) instanceLister {
	return func(_ context.Context, region, filter string) ([]aws.InstanceInfo, error) {
		if filter != "all" {
			// A terminated instance is invisible to the default filter, which
			// is exactly the case this lookup exists for.
			return nil, errors.New("task status must list with the \"all\" state filter, got " + filter)
		}
		return insts, err
	}
}

// spawn#802: a task whose instance was terminated without writing a record
// must not be reported as running.
func TestReportTaskWithoutRecord_TerminatedInstanceIsNotRunning(t *testing.T) {
	ec2 := []aws.InstanceInfo{{
		InstanceID: "i-009c", Name: "cookbook-fenicsx-r1", State: "terminated",
		StateTransitionReason: "User initiated (2026-10-11 03:23:47 GMT)",
		Tags:                  map[string]string{"spawn:task-id": "cookbook-fenicsx-r1"},
	}}
	var out bytes.Buffer
	code := reportTaskWithoutRecord(context.Background(), fakeLister(ec2, nil), "us-west-2", "cookbook-fenicsx-r1", &out, false)
	if code == taskStatusExitRunning {
		t.Fatalf("terminated task reported as running (exit %d): %q", code, out.String())
	}
	if code != taskStatusExitFailed {
		t.Errorf("want exit %d (failed: no outputs), got %d", taskStatusExitFailed, code)
	}
	if strings.Contains(out.String(), ": running") {
		t.Errorf("output says running: %q", out.String())
	}
	for _, want := range []string{"terminated", "i-009c", "User initiated"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output %q should contain %q", out.String(), want)
		}
	}
}

func TestReportTaskWithoutRecord_LiveInstanceIsRunning(t *testing.T) {
	now := time.Now()
	ec2 := []aws.InstanceInfo{
		{InstanceID: "i-old", State: "terminated", LaunchTime: now.Add(-time.Hour), Tags: map[string]string{"spawn:task-id": "t1"}},
		{InstanceID: "i-new", State: "running", LaunchTime: now, Tags: map[string]string{"spawn:task-id": "t1"}},
	}
	var out bytes.Buffer
	if code := reportTaskWithoutRecord(context.Background(), fakeLister(ec2, nil), "us-west-2", "t1", &out, false); code != taskStatusExitRunning {
		t.Errorf("a re-run with a live instance is running, got %d: %q", code, out.String())
	}
}

// No instance at all, or a failed lookup: unknown, never running.
func TestReportTaskWithoutRecord_NoInstanceOrLookupErrorIsUnknown(t *testing.T) {
	other := []aws.InstanceInfo{{InstanceID: "i-x", Name: "other", State: "running", Tags: map[string]string{"spawn:task-id": "other"}}}
	for name, l := range map[string]instanceLister{
		"no match":     fakeLister(other, nil),
		"lookup error": fakeLister(nil, errors.New("AccessDenied")),
	} {
		var out bytes.Buffer
		code := reportTaskWithoutRecord(context.Background(), l, "us-west-2", "t1", &out, false)
		if code != taskStatusExitError || strings.Contains(out.String(), ": running") {
			t.Errorf("%s: want unknown/exit %d, got %d %q", name, taskStatusExitError, code, out.String())
		}
	}
}

// An instance tagged for a different task must not match by Name; the tag wins.
func TestTaskInstances_TagIsAuthoritativeNameIsFallback(t *testing.T) {
	all := []aws.InstanceInfo{
		{InstanceID: "i-1", Name: "t1", Tags: map[string]string{"spawn:task-id": "t2"}},
		{InstanceID: "i-2", Name: "t1", Tags: map[string]string{}},
		{InstanceID: "i-3", Name: "zz", Tags: map[string]string{"spawn:task-id": "t1"}},
	}
	got := taskInstances(all, "t1")
	if len(got) != 2 || got[0].InstanceID != "i-2" || got[1].InstanceID != "i-3" {
		t.Errorf("got %+v", got)
	}
}
