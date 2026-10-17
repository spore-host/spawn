package pipeline

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spore-host/spawn/pkg/testutil"
)

// ---- #989: spawn pipeline collect ---------------------------------------------

const (
	collectDataBucket   = "collect-data"
	collectResultBucket = "collect-results"
	collectPrefix       = "pipelines/ml"
)

func collectS3(t *testing.T) *s3.Client {
	t.Helper()
	c := testutil.SubstrateServer(t).S3Client()
	for _, b := range []string{collectDataBucket, collectResultBucket} {
		if _, err := c.CreateBucket(context.Background(), &s3.CreateBucketInput{Bucket: aws.String(b)}); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func putKey(t *testing.T, c *s3.Client, bucket, key, body string) {
	t.Helper()
	if _, err := c.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), Body: strings.NewReader(body),
	}); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

// seedRun writes one run's stage data the way the stage runner lays it out,
// through the stage_keys helpers: a single-instance stage, a two-instance
// stage, every marker kind, and the stage config.
func seedRun(t *testing.T, c *s3.Client, runID string) {
	t.Helper()
	run := RunKeyPrefix(collectPrefix, runID)
	put := func(key, body string) { putKey(t, c, collectDataBucket, key, body) }

	put(StageInstanceOutputPrefix(run, "preprocess", 1, 0)+"a.csv", runID+" a")
	put(StageInstanceOutputPrefix(run, "preprocess", 1, 0)+"sub/b.csv", runID+" b")
	put(StageInstanceMarkerKey(run, "preprocess", 0, MarkerComplete), "")
	put(StageMarkerKey(run, "preprocess", MarkerComplete), "")
	for i := 0; i < 2; i++ {
		put(StageInstanceOutputPrefix(run, "train", 2, i)+"best.h5", fmt.Sprintf("%s model %d", runID, i))
		put(StageInstanceMarkerKey(run, "train", i, MarkerComplete), "")
	}
	put(StageInstanceMarkerKey(run, "evaluate", 0, MarkerFailed), "")
	put(StageConfigKey(run, "ml"), "{}")
}

func collectState(runID string) *PipelineState {
	return &PipelineState{
		PipelineID: "ml", RunID: runID, Status: StatusCompleted,
		S3Bucket: collectDataBucket, S3Prefix: collectPrefix,
		// Set, as every example that used it did: collect read this, and
		// nothing writes it.
		ResultS3Bucket: collectResultBucket, ResultS3Prefix: "pipelines/results",
	}
}

func filesIn(t *testing.T, root string) map[string]string {
	t.Helper()
	got := map[string]string{}
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			rel, _ := filepath.Rel(root, p)
			b, _ := os.ReadFile(p)
			got[filepath.ToSlash(rel)] = string(b)
		}
		return nil
	})
	return got
}

func keysOf(m map[string]string) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// collect downloads the stage outputs of the run in the record, from
// s3_bucket, each to <output-dir>/<stage>/<path below output/>, and nothing
// else: not the markers, not the stage config, not another run's outputs, and
// not from result_s3_bucket, which nothing writes. It used to list
// result_s3_bucket whenever it was set, and found nothing.
func TestCollectDownloadsTheRunsStageOutputs(t *testing.T) {
	c := collectS3(t)
	seedRun(t, c, "r1")
	seedRun(t, c, "r2")

	dir := t.TempDir()
	var out bytes.Buffer
	if err := CollectRunOutputs(context.Background(), c, collectState("r2"), CollectOptions{OutputDir: dir}, &out); err != nil {
		t.Fatalf("collect: %v\n%s", err, out.String())
	}
	want := map[string]string{
		"preprocess/a.csv":     "r2 a",
		"preprocess/sub/b.csv": "r2 b",
		"train/0/best.h5":      "r2 model 0",
		"train/1/best.h5":      "r2 model 1",
	}
	got := filesIn(t, dir)
	if strings.Join(keysOf(got), " ") != strings.Join(keysOf(want), " ") {
		t.Fatalf("collected %v, want %v\n%s", keysOf(got), keysOf(want), out.String())
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q (wrong run?)", k, got[k], v)
		}
	}
	if !strings.Contains(out.String(), "deprecated") {
		t.Errorf("collect did not say result_s3_bucket is deprecated:\n%s", out.String())
	}
}

// --run collects an earlier run of the same pipeline id; the record holds only
// the latest.
func TestCollectRunFlagSelectsAnEarlierRun(t *testing.T) {
	c := collectS3(t)
	seedRun(t, c, "r1")
	seedRun(t, c, "r2")

	dir := t.TempDir()
	if err := CollectRunOutputs(context.Background(), c, collectState("r2"), CollectOptions{RunID: "r1", OutputDir: dir}, io.Discard); err != nil {
		t.Fatal(err)
	}
	got := filesIn(t, dir)
	if got["preprocess/a.csv"] != "r1 a" || got["train/1/best.h5"] != "r1 model 1" || len(got) != 4 {
		t.Errorf("--run r1 collected %v", got)
	}
}

// --run is joined into an S3 prefix; a value that is not a run id is refused.
func TestCollectRejectsARunThatIsNotARunID(t *testing.T) {
	c := collectS3(t)
	for _, bad := range []string{"../r1", "r1/../../x", "R1", "a/b"} {
		err := CollectRunOutputs(context.Background(), c, collectState("r2"), CollectOptions{RunID: bad, OutputDir: t.TempDir()}, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "not a run id") {
			t.Errorf("--run %q: err = %v, want a refusal", bad, err)
		}
	}
}

func TestCollectStageFlag(t *testing.T) {
	c := collectS3(t)
	seedRun(t, c, "r2")

	dir := t.TempDir()
	if err := CollectRunOutputs(context.Background(), c, collectState("r2"), CollectOptions{StageID: "train", OutputDir: dir}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := keysOf(filesIn(t, dir)); strings.Join(got, " ") != "train/0/best.h5 train/1/best.h5" {
		t.Errorf("--stage train collected %v", got)
	}
}

// More than one ListObjectsV2 page (1000 keys) is collected in full. It used to
// make one call and report the first 1000 as everything.
func TestCollectPaginates(t *testing.T) {
	c := collectS3(t)
	out := StageOutputPrefix(RunKeyPrefix(collectPrefix, "r2"), "shard")
	const n = 1234
	for i := 0; i < n; i++ {
		putKey(t, c, collectDataBucket, fmt.Sprintf("%spart-%04d", out, i), "x")
	}

	dir := t.TempDir()
	if err := CollectRunOutputs(context.Background(), c, collectState("r2"), CollectOptions{OutputDir: dir}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := len(filesIn(t, dir)); got != n {
		t.Errorf("collected %d files, want %d", got, n)
	}
}

// Security: an object key containing ".." would have been written to
// <output-dir>/<key>, outside the directory. Such a key is refused, and
// nothing at all is downloaded, not even the run's legitimate outputs.
func TestCollectRefusesKeysThatLeaveTheOutputDirectory(t *testing.T) {
	run := RunKeyPrefix(collectPrefix, "r2")
	for _, rel := range []string{
		// The old code wrote <output-dir>/<key>. The key is 7 segments deep
		// at output/, so 9 ".." land in parent/a/b, outside the output
		// directory but inside the test's temp dir, where the walk below
		// finds it.
		strings.Repeat("../", 9) + "escaped.txt",
		"sub/" + strings.Repeat("../", 10) + "escaped.txt",
		"../train/0/best.h5", // inside the output directory, but in another stage's
	} {
		t.Run(rel, func(t *testing.T) {
			c := collectS3(t)
			seedRun(t, c, "r2")
			// "-first.txt" lists before the ".." key, so the old code had
			// already created the directories the ".." climbs out of, and
			// the escaped file really was written.
			putKey(t, c, collectDataBucket, StageOutputPrefix(run, "preprocess")+"-first.txt", "ok")
			putKey(t, c, collectDataBucket, StageOutputPrefix(run, "preprocess")+rel, "pwned")

			parent := t.TempDir()
			dir := filepath.Join(parent, "a", "b", "c", "results")
			err := CollectRunOutputs(context.Background(), c, collectState("r2"), CollectOptions{OutputDir: dir}, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "outside the output directory") {
				t.Errorf("err = %v, want a refusal", err)
			}
			// Nothing is written anywhere under parent: the refusal comes
			// before any download.
			_ = filepath.Walk(parent, func(p string, info os.FileInfo, err error) error {
				if err == nil && !info.IsDir() {
					t.Errorf("collect wrote %s", p)
				}
				return nil
			})
		})
	}
}

// The stage segment of a key is checked too, not only the part below output/.
func TestCollectRefusesADotDotStageID(t *testing.T) {
	c := collectS3(t)
	run := RunKeyPrefix(collectPrefix, "r2")
	putKey(t, c, collectDataBucket, StagesPrefix(run)+"../output/escaped.txt", "pwned")

	parent := t.TempDir()
	dir := filepath.Join(parent, "results")
	err := CollectRunOutputs(context.Background(), c, collectState("r2"), CollectOptions{OutputDir: dir}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "outside the output directory") {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if _, err := os.Stat(filepath.Join(parent, "escaped.txt")); err == nil {
		t.Error("collect wrote outside the output directory")
	}
}

// A run with no outputs says so, and names the runs that do exist.
func TestCollectNothingFoundListsRuns(t *testing.T) {
	c := collectS3(t)
	seedRun(t, c, "r1")

	var out bytes.Buffer
	if err := CollectRunOutputs(context.Background(), c, collectState("r9"), CollectOptions{OutputDir: t.TempDir()}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No stage outputs found") || !strings.Contains(out.String(), "r1") {
		t.Errorf("output does not report the empty run and the runs that exist:\n%s", out.String())
	}
}

// A symlink already in the output directory does not lead a download out of
// it: the files are written through an os.Root.
func TestCollectDoesNotFollowASymlinkOutOfTheOutputDirectory(t *testing.T) {
	c := collectS3(t)
	seedRun(t, c, "r2")

	parent := t.TempDir()
	outside := filepath.Join(parent, "outside")
	dir := filepath.Join(parent, "results")
	for _, d := range []string{outside, dir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(dir, "preprocess")); err != nil {
		t.Fatal(err)
	}

	err := CollectRunOutputs(context.Background(), c, collectState("r2"), CollectOptions{OutputDir: dir}, io.Discard)
	if err == nil {
		t.Error("collect wrote through a symlink out of the output directory without an error")
	}
	if got := filesIn(t, outside); len(got) != 0 {
		t.Errorf("collect wrote %v outside the output directory, through a symlink", keysOf(got))
	}
}
