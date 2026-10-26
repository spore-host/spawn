package cmd

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/taskproto"
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

// spawn#1295: a terminated task with no record names the spawn:flush-status
// tag spored wrote, and where the flush hook's output went.
func TestReportTaskWithoutRecord_ShowsFlushStatus(t *testing.T) {
	ec2 := []aws.InstanceInfo{{
		InstanceID: "i-0f1", State: "terminated",
		Tags: map[string]string{"spawn:task-id": "t1", taskproto.FlushStatusTag: "failed:upload-failed"},
	}}
	var out bytes.Buffer
	code := reportTaskWithoutRecord(context.Background(), fakeLister(ec2, nil), "us-west-2", "t1", &out, false)
	if code != taskStatusExitFailed {
		t.Errorf("exit = %d, want %d", code, taskStatusExitFailed)
	}
	for _, want := range []string{"Flush: failed:upload-failed", "aws ec2 get-console-output --instance-id i-0f1 --region us-west-2"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output %q should contain %q", out.String(), want)
		}
	}
}

// A flush that reported ok but left no record where status looked points at
// the results prefix, not at a lost log.
func TestReportTaskWithoutRecord_FlushOKPointsAtResultsPrefix(t *testing.T) {
	ec2 := []aws.InstanceInfo{{
		InstanceID: "i-0f2", State: "terminated",
		Tags: map[string]string{"spawn:task-id": "t1", taskproto.FlushStatusTag: taskproto.FlushStatusOK},
	}}
	var out bytes.Buffer
	reportTaskWithoutRecord(context.Background(), fakeLister(ec2, nil), "us-west-2", "t1", &out, false)
	if !strings.Contains(out.String(), "Flush: ok") || !strings.Contains(out.String(), "--results-prefix") {
		t.Errorf("output %q should report flush ok and suggest --results-prefix", out.String())
	}
}

// An ended instance without the tag says so, instead of naming a flush outcome.
func TestReportTaskWithoutRecord_UntaggedSaysNotRecorded(t *testing.T) {
	ec2 := []aws.InstanceInfo{{InstanceID: "i-0f3", State: "terminated", Tags: map[string]string{"spawn:task-id": "t1"}}}
	var out bytes.Buffer
	reportTaskWithoutRecord(context.Background(), fakeLister(ec2, nil), "us-west-2", "t1", &out, false)
	if !strings.Contains(out.String(), "Flush: not recorded on instance i-0f3") {
		t.Errorf("output %q should say the flush status is not recorded", out.String())
	}
}

// spawn#1295: once EC2 has forgotten the instance, its tag is gone with it, and
// status must say so plainly rather than say nothing.
func TestReportTaskWithoutRecord_NoInstanceSaysFlushStatusUnavailable(t *testing.T) {
	var out bytes.Buffer
	reportTaskWithoutRecord(context.Background(), fakeLister(nil, nil), "us-west-2", "t1", &out, false)
	if !strings.Contains(out.String(), "Flush: no longer available") {
		t.Errorf("output %q should say the flush status is no longer available", out.String())
	}
}

// spawn#1295: a completion record is followed by a compact Flush line, from
// the same tag, or saying it is gone.
func TestPrintRecordFlushStatus(t *testing.T) {
	tagged := []aws.InstanceInfo{{
		InstanceID: "i-0f4", State: "terminated",
		Tags: map[string]string{"spawn:task-id": "t1", taskproto.FlushStatusTag: "skipped:hook-missing"},
	}}
	cases := map[string]struct {
		insts []aws.InstanceInfo
		want  string
	}{
		"tag present":    {tagged, "Flush:      skipped:hook-missing (instance i-0f4)\n"},
		"instance gone":  {nil, "Flush:      no longer available"},
		"other task ran": {[]aws.InstanceInfo{{InstanceID: "i-x", State: "terminated", Tags: map[string]string{"spawn:task-id": "t2", taskproto.FlushStatusTag: "ok"}}}, "Flush:      no longer available"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			printRecordFlushStatus(context.Background(), &out, fakeLister(c.insts, nil), "us-west-2", "t1")
			if !strings.HasPrefix(out.String(), c.want) {
				t.Errorf("output %q, want prefix %q", out.String(), c.want)
			}
		})
	}
	var out bytes.Buffer
	printRecordFlushStatus(context.Background(), &out, fakeLister(nil, errors.New("throttled")), "us-west-2", "t1")
	if out.Len() != 0 {
		t.Errorf("a failed lookup must print nothing under a record, printed %q", out.String())
	}
}

// TestTaskStatusRunE_CallsTheFlushReaders: the helpers above are only reached
// if `task status` calls them, which its RunE needs a real AWS client to show.
// So check the invocations in its source: taskStatusCmd's own literal must
// call printRecordFlushStatus (the record path, #1295) and
// reportTaskWithoutRecord (the no-record path, #802).
func TestTaskStatusRunE_CallsTheFlushReaders(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "task.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var lit ast.Node
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, sp := range gd.Specs {
			vs, ok := sp.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, n := range vs.Names {
				if n.Name == "taskStatusCmd" && i < len(vs.Values) {
					lit = vs.Values[i]
				}
			}
		}
	}
	if lit == nil {
		t.Fatal("taskStatusCmd not found in task.go")
	}
	called := map[string]bool{}
	ast.Inspect(lit, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if id, ok := call.Fun.(*ast.Ident); ok {
				called[id.Name] = true
			}
		}
		return true
	})
	for _, want := range []string{"printRecordFlushStatus", "reportTaskWithoutRecord"} {
		if !called[want] {
			t.Errorf("taskStatusCmd does not call %s", want)
		}
	}
}
