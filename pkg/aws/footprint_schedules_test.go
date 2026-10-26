package aws

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"
	schtypes "github.com/aws/aws-sdk-go-v2/service/scheduler/types"
	spawnscheduler "github.com/spore-host/spawn/pkg/scheduler"
)

// capturedSchedule is what `spawn schedule` really sends to CreateSchedule,
// captured from pkg/scheduler's own request (no network), so the footprint is
// tested against the schedule spawn makes rather than a hand-written copy.
type capturedSchedule struct {
	name, description, targetARN string
}

type captureTransport struct {
	mu  sync.Mutex
	got []capturedSchedule
}

func (c *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body := []byte("{}")
	if strings.HasPrefix(req.URL.Host, "scheduler.") && req.Method == http.MethodPost {
		var in struct {
			Description string
			Target      struct{ Arn string }
		}
		b, _ := io.ReadAll(req.Body)
		_ = json.Unmarshal(b, &in)
		name, _ := url.PathUnescape(strings.TrimPrefix(req.URL.EscapedPath(), "/schedules/"))
		c.mu.Lock()
		c.got = append(c.got, capturedSchedule{name, in.Description, in.Target.Arn})
		c.mu.Unlock()
		body = []byte(`{"ScheduleArn":"arn:aws:scheduler:us-east-1:111122223333:schedule/default/` + name + `"}`)
	}
	return &http.Response{
		StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body)),
		Header: http.Header{"Content-Type": []string{"application/x-amz-json-1.0"}}, Request: req,
	}, nil
}

func captureSpawnSchedule(t *testing.T, lambdaARN string) capturedSchedule {
	t.Helper()
	tr := &captureTransport{}
	cfg := awssdk.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		HTTPClient:  &http.Client{Transport: tr},
	}
	c := spawnscheduler.NewClient(cfg, lambdaARN, "arn:aws:iam::111122223333:role/r", "111122223333")
	rec := &spawnscheduler.ScheduleRecord{
		ScheduleID: spawnscheduler.GenerateScheduleID(), ScheduleName: "nightly",
		ScheduleExpression: "rate(1 day)",
	}
	if _, err := c.CreateSchedule(context.Background(), rec); err != nil {
		t.Fatalf("CreateSchedule against the capture transport: %v", err)
	}
	if len(tr.got) != 1 || tr.got[0].name == "" || tr.got[0].description == "" {
		t.Fatalf("did not capture spawn's CreateSchedule request: %+v", tr.got)
	}
	return tr.got[0]
}

type fakeSchedules struct {
	pages    [][]schtypes.ScheduleSummary
	desc     map[string]string // "group/name" -> Description
	listErr  error
	getCalls []string
}

func (f *fakeSchedules) ListSchedules(_ context.Context, in *scheduler.ListSchedulesInput, _ ...func(*scheduler.Options)) (*scheduler.ListSchedulesOutput, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	if in.GroupName != nil {
		return nil, errors.New("test: ListSchedules restricted to one group; the footprint must list every group")
	}
	i := 0
	if tok := awssdk.ToString(in.NextToken); tok != "" {
		i = int(tok[0] - '0')
	}
	out := &scheduler.ListSchedulesOutput{Schedules: f.pages[i]}
	if i+1 < len(f.pages) {
		out.NextToken = awssdk.String(string(rune('0' + i + 1)))
	}
	return out, nil
}

func (f *fakeSchedules) GetSchedule(_ context.Context, in *scheduler.GetScheduleInput, _ ...func(*scheduler.Options)) (*scheduler.GetScheduleOutput, error) {
	key := awssdk.ToString(in.GroupName) + "/" + awssdk.ToString(in.Name)
	f.getCalls = append(f.getCalls, key)
	return &scheduler.GetScheduleOutput{Description: awssdk.String(f.desc[key])}, nil
}

func summary(group, name, target string) schtypes.ScheduleSummary {
	return schtypes.ScheduleSummary{
		Name: awssdk.String(name), GroupName: awssdk.String(group), State: schtypes.ScheduleStateEnabled,
		Target: &schtypes.TargetSummary{Arn: awssdk.String(target)},
	}
}

// #1219: `spawn footprint` must list the EventBridge Scheduler schedules
// `spawn schedule` creates, which are named sched-YYYYMMDD-HHMMSS in the
// default group and match no name prefix.
func TestFootprintSeesSpawnSchedules(t *testing.T) {
	// The ARN cmd/schedule.go targets, and a renamed handler (a deployment that
	// set SPAWN_LAMBDA_NAME), whose schedule only its description identifies.
	viaTarget := captureSpawnSchedule(t, "arn:aws:lambda:us-east-1:111122223333:function:scheduler-handler")
	renamed := captureSpawnSchedule(t, "arn:aws:lambda:us-east-1:111122223333:function:my-handler")
	renamed.name = "sched-20991231-235959" // distinct from viaTarget's second-resolution ID

	f := &fakeSchedules{
		pages: [][]schtypes.ScheduleSummary{
			{summary("default", "nightly-backup", "arn:aws:lambda:us-east-1:111122223333:function:backup")},
			{
				summary("default", "sched-20200101-000000", "arn:aws:lambda:us-east-1:111122223333:function:other"),
				summary("spawn-pipelines", "anything", "arn:aws:sqs:us-east-1:111122223333:q"),
			},
			{
				summary("default", viaTarget.name, viaTarget.targetARN),
				summary("default", renamed.name, renamed.targetARN),
			},
		},
		desc: map[string]string{
			"default/" + renamed.name:       renamed.description,
			"default/sched-20200101-000000": "someone else's schedule",
		},
	}
	rows, notes := scanSchedules(context.Background(), f, "us-east-1")
	if len(notes) != 0 {
		t.Fatalf("notes %v", notes)
	}
	got := map[string]string{}
	for _, r := range rows {
		if r.Service != "scheduler" || r.Kind != "schedule" || r.Region != "us-east-1" {
			t.Errorf("row %+v: want service scheduler, kind schedule, region us-east-1", r)
		}
		got[r.Name] = r.Why
	}
	want := map[string]string{viaTarget.name: "target", renamed.name: "description", "anything": "name"}
	for n, why := range want {
		if got[n] != why {
			t.Errorf("schedule %s: why=%q, want %q (rows %v)", n, got[n], why, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("footprint reported %v, want exactly %v", got, want)
	}
	for _, k := range f.getCalls {
		if strings.HasSuffix(k, "/nightly-backup") {
			t.Errorf("GetSchedule called for %s, which is not shaped like a spawn schedule ID", k)
		}
	}
}

// #1219: a principal without scheduler:ListSchedules gets a note naming the
// permission, not an abort and not a silent empty section.
func TestFootprintSchedulesDeniedIsANote(t *testing.T) {
	f := &fakeSchedules{listErr: errors.New("AccessDeniedException: not authorized")}
	rows, notes := scanSchedules(context.Background(), f, "us-east-1")
	if len(rows) != 0 {
		t.Errorf("rows %v from a denied listing", rows)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "scheduler:ListSchedules") {
		t.Errorf("notes %v, want one naming scheduler:ListSchedules", notes)
	}
}

// #1219: ScanControlPlane must call scanSchedules with a Scheduler client, or
// the tests above prove a function nothing runs.
func TestScanControlPlaneScansSchedules(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRootFromPkgAWS(t), "pkg", "aws", "footprint.go"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?s)func \(c \*Client\) ScanControlPlane\(.*?\n\}\n`)
	body := re.Find(b)
	if body == nil {
		t.Fatal("ScanControlPlane not found in pkg/aws/footprint.go")
	}
	if !regexp.MustCompile(`(?m)^\s*\w+, \w+ := scanSchedules\(ctx, scheduler\.NewFromConfig\(cfg\), region\)`).Match(body) {
		t.Error("ScanControlPlane does not call scanSchedules(ctx, scheduler.NewFromConfig(cfg), region)")
	}
}
