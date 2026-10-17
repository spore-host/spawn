package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/spore-host/spawn/pkg/pipeline"
)

// #1014: `spawn pipeline status` must show the run id that
// `spawn pipeline collect --run` takes, and the pipeline's other runs, in text
// and in JSON alike. It used to show neither; the id was only in the record.

const (
	statusRun  = "20261015t120000z-cccccccc"
	statusOld1 = "20261013t120000z-aaaaaaaa"
	statusOld2 = "20261014t120000z-bbbbbbbb"
)

func statusState(runID string) *pipeline.PipelineState {
	t := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	return &pipeline.PipelineState{
		PipelineID: "pl-1", PipelineName: "demo", RunID: runID, Status: pipeline.StatusRunning,
		S3Bucket: "bkt", S3Prefix: "pfx", CreatedAt: t, UpdatedAt: t, TotalStages: 1,
		Stages: []pipeline.StageState{{StageID: "train", Status: pipeline.StageStatusRunning}},
	}
}

func renderStatus(t *testing.T, state *pipeline.PipelineState, runs []string, runsErr error, asJSON bool) (string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	if err := writePipelineStatus(&out, &errOut, state, runs, runsErr, asJSON); err != nil {
		t.Fatal(err)
	}
	return out.String(), errOut.String()
}

func TestPipelineStatusTextNamesTheRunCollectTakes(t *testing.T) {
	out, _ := renderStatus(t, statusState(statusRun), []string{statusOld1, statusOld2, statusRun}, nil, false)
	if !regexp.MustCompile(`(?m)^Run: ` + statusRun + `$`).MatchString(out) {
		t.Errorf("status does not print \"Run: %s\":\n%s", statusRun, out)
	}
	// The other runs, newest first, and not the current one again.
	if !regexp.MustCompile(`(?m)^Other runs: ` + statusOld2 + ` ` + statusOld1 + `$`).MatchString(out) {
		t.Errorf("status does not list the other runs newest first:\n%s", out)
	}
	if !strings.Contains(out, "spawn pipeline collect pl-1 --run <run-id>") {
		t.Errorf("status does not say how to collect another run:\n%s", out)
	}
}

func TestPipelineStatusWithOnlyTheCurrentRunListsNoOthers(t *testing.T) {
	out, _ := renderStatus(t, statusState(statusRun), []string{statusRun}, nil, false)
	if strings.Contains(out, "Other runs") {
		t.Errorf("status lists other runs when there is only the current one:\n%s", out)
	}
}

func TestPipelineStatusCapsTheOtherRunsLineToTheNewest(t *testing.T) {
	var runs []string
	for i := 0; i < statusOtherRunsShown+2; i++ {
		runs = append(runs, fmt.Sprintf("20261001t%06dz-00000000", i))
	}
	runs = append(runs, statusRun)
	out, _ := renderStatus(t, statusState(statusRun), runs, nil, false)
	line := regexp.MustCompile(`(?m)^Other runs:.*$`).FindString(out)
	if strings.Count(line, "20261001t") != statusOtherRunsShown {
		t.Errorf("other runs line shows %d runs, want %d: %q", strings.Count(line, "20261001t"), statusOtherRunsShown, line)
	}
	if strings.Contains(line, runs[0]) || strings.Contains(line, runs[1]) || !strings.Contains(line, runs[statusOtherRunsShown+1]) {
		t.Errorf("other runs line does not keep the newest: %q", line)
	}
	if !strings.Contains(line, "and 2 older") {
		t.Errorf("other runs line does not count the runs it left out: %q", line)
	}
}

// A record written before run ids has none; collect then takes no --run.
func TestPipelineStatusOfALegacyRecord(t *testing.T) {
	out, _ := renderStatus(t, statusState(""), []string{}, nil, false)
	if !regexp.MustCompile(`(?m)^Run: - `).MatchString(out) {
		t.Errorf("status of a record without a run id does not say so:\n%s", out)
	}
}

// Failing to list the runs must not fail status, nor hide the run id.
func TestPipelineStatusSurvivesARunListingError(t *testing.T) {
	out, errOut := renderStatus(t, statusState(statusRun), nil, errors.New("AccessDenied"), false)
	if !regexp.MustCompile(`(?m)^Run: ` + statusRun + `$`).MatchString(out) {
		t.Errorf("status hid the run id when the runs could not be listed:\n%s", out)
	}
	if !strings.Contains(errOut, "AccessDenied") {
		t.Errorf("status did not report the listing error: %q", errOut)
	}
}

func decodeStatus(t *testing.T, out string) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("status --output json is not a JSON object: %v\n%s", err, out)
	}
	return m
}

func TestPipelineStatusJSONCarriesTheSameRunsAsText(t *testing.T) {
	out, _ := renderStatus(t, statusState(statusRun), []string{statusOld1, statusOld2, statusRun}, nil, true)
	m := decodeStatus(t, out)
	if string(m["run_id"]) != `"`+statusRun+`"` {
		t.Errorf("run_id = %s, want %q", m["run_id"], statusRun)
	}
	var runs []string
	if err := json.Unmarshal(m["runs"], &runs); err != nil || strings.Join(runs, ",") != strings.Join([]string{statusOld1, statusOld2, statusRun}, ",") {
		t.Errorf("runs = %s, want every run oldest first", m["runs"])
	}
	// Still the record: its fields are all there.
	for _, k := range []string{"pipeline_id", "status", "s3_bucket", "stages"} {
		if _, ok := m[k]; !ok {
			t.Errorf("status JSON lacks the record's %q", k)
		}
	}
}

// run_id is always present, "" on a legacy record (the record itself omits it
// when empty), and runs is [] when there are none, null only when unlisted.
func TestPipelineStatusJSONOfALegacyRecordAndAnEmptyListing(t *testing.T) {
	m := decodeStatus(t, func() string { o, _ := renderStatus(t, statusState(""), []string{}, nil, true); return o }())
	if string(m["run_id"]) != `""` {
		t.Errorf("legacy record: run_id = %s, want \"\"", m["run_id"])
	}
	if string(m["runs"]) != `[]` {
		t.Errorf("no runs: runs = %s, want []", m["runs"])
	}
	if _, ok := m["runs_error"]; ok {
		t.Error("runs_error present without an error")
	}
}

func TestPipelineStatusJSONReportsARunListingError(t *testing.T) {
	out, _ := renderStatus(t, statusState(statusRun), []string{"ignored"}, errors.New("AccessDenied"), true)
	m := decodeStatus(t, out)
	if string(m["runs"]) != "null" {
		t.Errorf("runs = %s on a listing error, want null", m["runs"])
	}
	if !strings.Contains(string(m["runs_error"]), "AccessDenied") {
		t.Errorf("runs_error = %s, want the listing error", m["runs_error"])
	}
}

// fakeRunLister answers ListObjectsV2 with common prefixes, as S3 does for a
// "/" delimiter, two pages.
type fakeRunLister struct {
	inputs []*s3.ListObjectsV2Input
	pages  [][]string
}

func (f *fakeRunLister) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	f.inputs = append(f.inputs, in)
	page := len(f.inputs) - 1
	out := &s3.ListObjectsV2Output{}
	for _, p := range f.pages[page] {
		out.CommonPrefixes = append(out.CommonPrefixes, s3types.CommonPrefix{Prefix: aws.String(p)})
	}
	if page+1 < len(f.pages) {
		out.IsTruncated = aws.Bool(true)
		out.NextContinuationToken = aws.String(fmt.Sprintf("tok%d", page+1))
	}
	return out, nil
}

// The runs come from the common prefixes under <s3_prefix>/runs/ in
// s3_bucket, every page, sorted, so status lists what collect --run can read.
func TestStatusRunIDsListsTheRunPrefixes(t *testing.T) {
	f := &fakeRunLister{pages: [][]string{
		{"pfx/runs/" + statusRun + "/"},
		{"pfx/runs/" + statusOld1 + "/", "pfx/runs/" + statusOld2 + "/"},
	}}
	runs, err := statusRunIDs(context.Background(), f, statusState(statusRun))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(runs, ","), strings.Join([]string{statusOld1, statusOld2, statusRun}, ","); got != want {
		t.Errorf("runs = %s, want %s", got, want)
	}
	if len(f.inputs) != 2 {
		t.Fatalf("%d ListObjectsV2 calls, want 2 (every page)", len(f.inputs))
	}
	in := f.inputs[0]
	if aws.ToString(in.Bucket) != "bkt" || aws.ToString(in.Prefix) != "pfx/runs/" || aws.ToString(in.Delimiter) != "/" {
		t.Errorf("listed s3://%s/%s delimiter %q, want s3://bkt/pfx/runs/ delimiter \"/\"",
			aws.ToString(in.Bucket), aws.ToString(in.Prefix), aws.ToString(in.Delimiter))
	}
	// The run prefix is the one the runs' data is written under.
	if want := pipeline.RunKeyPrefix("pfx", statusRun) + "/"; aws.ToString(in.Prefix)+statusRun+"/" != want {
		t.Errorf("listed prefix %q is not the parent of the run prefix %q", aws.ToString(in.Prefix), want)
	}
}

func TestStatusRunIDsOfAPipelineWithoutABucket(t *testing.T) {
	st := statusState(statusRun)
	st.S3Bucket = ""
	f := &fakeRunLister{}
	runs, err := statusRunIDs(context.Background(), f, st)
	if err != nil || runs == nil || len(runs) != 0 || len(f.inputs) != 0 {
		t.Errorf("no bucket: runs=%v err=%v calls=%d, want [] without listing", runs, err, len(f.inputs))
	}
}

// A listing that finds no runs is [], not null, so JSON keeps null for "could
// not list".
func TestStatusRunIDsOfAnEmptyListingIsNotNull(t *testing.T) {
	runs, err := statusRunIDs(context.Background(), &fakeRunLister{pages: [][]string{{}}}, statusState(statusRun))
	if err != nil || runs == nil {
		t.Errorf("empty listing: runs=%#v err=%v, want a non-nil empty slice", runs, err)
	}
}
