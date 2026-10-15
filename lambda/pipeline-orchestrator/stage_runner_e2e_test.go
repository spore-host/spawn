package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spore-host/spawn/pkg/pipeline"
	"github.com/spore-host/spawn/pkg/testutil"
)

// The stage user data, the real spored stage runner and the orchestrator's
// marker reader, end to end against the in-process Substrate emulator, with a
// fake IMDS and no AWS. This is the #910/#911 property: a stage downloads its
// data_input, its data_output reaches S3, and the marker it writes is the one
// the orchestrator reads.

var (
	sporedOnce sync.Once
	sporedBin  string
	sporedErr  error
	sporedOut  []byte
)

// buildSpored builds cmd/spored for the host, once per test binary.
func buildSpored(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stage user data is bash")
	}
	sporedOnce.Do(func() {
		dir, err := os.MkdirTemp("", "spored-e2e-")
		if err != nil {
			sporedErr = err
			return
		}
		sporedBin = filepath.Join(dir, "spored")
		cmd := exec.Command("go", "build", "-o", sporedBin, "../../cmd/spored")
		sporedOut, sporedErr = cmd.CombinedOutput()
	})
	if sporedErr != nil {
		t.Fatalf("building spored: %v\n%s", sporedErr, sporedOut)
	}
	return sporedBin
}

// fakeIMDS answers the IMDSv2 calls spored's SDK makes: a token, and the
// identity document naming the instance and region.
func fakeIMDS(t *testing.T, instanceID, region string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/latest/api/token":
			w.Header().Set("X-Aws-Ec2-Metadata-Token-Ttl-Seconds", "21600")
			_, _ = bytes.NewBufferString("tok").WriteTo(w) // a constant test token
		case r.URL.Path == "/latest/dynamic/instance-identity/document":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{
				"instanceId": instanceID, "region": region, "accountId": "123456789012",
				"availabilityZone": region + "a", "instanceType": "t3.micro",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

type e2eStage struct {
	calls    string
	runErr   error
	s3       *s3.Client
	state    *pipeline.PipelineState
	def      *pipeline.Pipeline
	inDir    string
	outDir   string
	instance string
	index    int
}

// runStageEndToEnd seeds Substrate with an upstream stage's output, launches a
// "train" stage instance in the emulator with the orchestrator's own
// RunInstances input (so its tags are exactly the ones the orchestrator
// writes), writes the stage config the way launchStage does, and executes the
// stage's user data with the real spored.
func runStageEndToEnd(t *testing.T, command string, index int) *e2eStage {
	t.Helper()
	return runStageEndToEndInRun(t, command, index, "")
}

// runStageEndToEndInRun is runStageEndToEnd for one run of the pipeline. With a
// run id, a stale upstream output is also left where a previous run under the
// same s3_prefix would have written it, which the stage must not receive.
func runStageEndToEndInRun(t *testing.T, command string, index int, runID string) *e2eStage {
	t.Helper()
	spored := buildSpored(t)
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	s3c := env.S3Client()
	work := t.TempDir()
	inDir := filepath.Join(work, "in")
	outDir := filepath.Join(work, "out")

	def := &pipeline.Pipeline{
		PipelineID: "pl-e2e", PipelineName: "e2e", S3Bucket: "bkt", S3Prefix: "pfx", OnFailure: "stop",
		Stages: []pipeline.Stage{
			{StageID: "preprocess", InstanceType: "t3.micro", InstanceCount: 1, Region: "us-east-1", Command: "true",
				DataOutput: &pipeline.DataConfig{Mode: "s3", Paths: []string{"/data/*"}}},
			{StageID: "train", InstanceType: "t3.micro", InstanceCount: index + 1, Region: "us-east-1", Timeout: "10m",
				Command:    strings.NewReplacer("$IN", inDir, "$OUT", outDir).Replace(command),
				DependsOn:  []string{"preprocess"},
				DataInput:  &pipeline.DataConfig{Mode: "s3", SourceStage: "preprocess", DestPath: inDir},
				DataOutput: &pipeline.DataConfig{Mode: "s3", Paths: []string{outDir + "/*"}},
				Env:        map[string]string{"GREETING": "from-env"}},
		},
	}
	if err := def.Validate(); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	state := &pipeline.PipelineState{
		PipelineID: def.PipelineID, PipelineName: def.PipelineName, S3Bucket: "bkt", S3Prefix: "pfx",
		OnFailure: "stop", CreatedAt: now, Status: pipeline.StatusRunning, TotalStages: 2,
		InstanceProfile: "spawn-instance-e2e", RunID: runID,
		Stages: []pipeline.StageState{
			{StageID: "preprocess", StageIndex: 0, Status: pipeline.StageStatusCompleted},
			{StageID: "train", StageIndex: 1, Status: pipeline.StageStatusRunning, LaunchedAt: &now},
		},
	}

	if _, err := s3c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("bkt")}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	// The upstream stage's output, where preprocess's runner would have put it.
	if _, err := s3c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("bkt"),
		Key:    aws.String(pipeline.StageOutputPrefix(state.DataPrefix(), "preprocess") + "data.csv"),
		Body:   strings.NewReader("hello pipeline\n"),
	}); err != nil {
		t.Fatalf("seed upstream output: %v", err)
	}
	if runID != "" {
		for _, stale := range []string{
			pipeline.StageOutputPrefix("pfx", "preprocess") + "data.csv",
			pipeline.StageOutputPrefix(pipeline.RunKeyPrefix("pfx", "previous-run"), "preprocess") + "data.csv",
		} {
			if _, err := s3c.PutObject(ctx, &s3.PutObjectInput{
				Bucket: aws.String("bkt"), Key: aws.String(stale), Body: strings.NewReader("STALE\n"),
			}); err != nil {
				t.Fatalf("seed stale output: %v", err)
			}
		}
	}

	// The orchestrator's side: the stage config, then the launch.
	oldPut := putObjectFn
	t.Cleanup(func() { putObjectFn = oldPut })
	putObjectFn = func(ctx context.Context, in *s3.PutObjectInput) error {
		_, err := s3c.PutObject(ctx, in)
		return err
	}
	if err := putStageConfig(ctx, state, def); err != nil {
		t.Fatalf("putStageConfig: %v", err)
	}
	ec2c := env.EC2Client()
	ami := testutil.RegisterTestAMI(t, ec2c)
	in := buildStageRunInstancesInput(state, def.GetStage("train"), getStageState(state, "train"), index, ami, "ud", now)
	in.IamInstanceProfile = nil // the emulator has no such profile; the profile is asserted elsewhere
	out, err := ec2c.RunInstances(ctx, in)
	if err != nil {
		t.Fatalf("RunInstances (emulator): %v", err)
	}
	instanceID := aws.ToString(out.Instances[0].InstanceId)

	calls, runErr := execUserData(t, userDataRun{
		state: state, stageDef: def.GetStage("train"), stageState: getStageState(state, "train"),
		region: "us-east-1", index: index, spored: spored,
		env: []string{
			"AWS_EC2_METADATA_SERVICE_ENDPOINT=" + fakeIMDS(t, instanceID, "us-east-1"),
			"AWS_ENDPOINT_URL=" + env.URL,
			"AWS_ACCESS_KEY_ID=test", "AWS_SECRET_ACCESS_KEY=test", "AWS_SESSION_TOKEN=test",
			"AWS_CONFIG_FILE=/dev/null", "AWS_SHARED_CREDENTIALS_FILE=/dev/null",
		},
	})
	return &e2eStage{calls: calls, runErr: runErr, s3: s3c, state: state, def: def,
		inDir: inDir, outDir: outDir, instance: instanceID, index: index}
}

func (e *e2eStage) object(t *testing.T, key string) (string, bool) {
	t.Helper()
	out, err := e.s3.GetObject(context.Background(), &s3.GetObjectInput{Bucket: aws.String("bkt"), Key: aws.String(key)})
	if err != nil {
		return "", false
	}
	defer func() { _ = out.Body.Close() }()
	var b bytes.Buffer
	_, _ = b.ReadFrom(out.Body)
	return b.String(), true
}

// markerSeen asks the orchestrator's own reader (markerExists, through the
// markers seam) whether the marker is there.
func (e *e2eStage) markerSeen(t *testing.T, marker string) bool {
	t.Helper()
	old := markers
	defer func() { markers = old }()
	markers = e.s3
	ok, err := markerExists(context.Background(), e.state, pipeline.StageInstanceMarkerKey(e.state.DataPrefix(), "train", e.index, marker))
	if err != nil {
		t.Fatalf("markerExists(%s): %v", marker, err)
	}
	return ok
}

// #911: data_input is downloaded before the command runs, data_output is
// uploaded after it, and COMPLETE is written where the orchestrator reads it.
func TestStageRunnerEndToEnd_InputsDownloadedOutputsUploaded(t *testing.T) {
	e := runStageEndToEnd(t, `mkdir -p $OUT && tr a-z A-Z < $IN/data.csv > $OUT/result.txt && echo "$GREETING" >> $OUT/result.txt`, 0)
	if e.runErr != nil {
		t.Fatalf("stage user data failed: %v\ncalls:\n%s", e.runErr, e.calls)
	}

	// The input reached the instance.
	if b, err := os.ReadFile(filepath.Join(e.inDir, "data.csv")); err != nil || string(b) != "hello pipeline\n" {
		t.Errorf("data_input not downloaded: %q, %v", b, err)
	}

	// The output reached S3, under the stage's output prefix, which is where a
	// downstream stage's data_input reads it from.
	list, err := e.s3.ListObjectsV2(context.Background(), &s3.ListObjectsV2Input{
		Bucket: aws.String("bkt"), Prefix: aws.String(pipeline.StageOutputPrefix("pfx", "train")),
	})
	if err != nil {
		t.Fatal(err)
	}
	var got string
	for _, o := range list.Contents {
		if strings.HasSuffix(aws.ToString(o.Key), "/result.txt") {
			got, _ = e.object(t, aws.ToString(o.Key))
		}
	}
	if got != "HELLO PIPELINE\nfrom-env\n" {
		t.Errorf("data_output in S3 = %q, want the command's output (input upper-cased, then the stage env)", got)
	}

	if !e.markerSeen(t, pipeline.MarkerComplete) {
		t.Error("the orchestrator does not see a COMPLETE marker for a stage that succeeded")
	}
	old := markers
	markers = e.s3
	defer func() { markers = old }()
	if outcome, _, err := checkStageOutcome(context.Background(), e.state, e.def.GetStage("train"), getStageState(e.state, "train"), time.Now()); err != nil || outcome != stageCompleted {
		t.Errorf("the orchestrator's verdict on the stage = %v (%v), want completed", outcome, err)
	}
	if e.markerSeen(t, pipeline.MarkerFailed) {
		t.Error("FAILED marker written for a stage that succeeded")
	}
	if strings.Contains(e.calls, "/FAILED") {
		t.Errorf("the user data wrote its fallback FAILED marker for a stage that succeeded; calls:\n%s", e.calls)
	}
	assertSelfTerminatedWith(t, e.calls, "us-east-1")
}

// A failing command: the runner writes FAILED where the orchestrator reads it,
// uploads nothing, and the instance still terminates. Run as instance 1 of a
// two-instance stage, so the marker must carry the instance index from the
// instance's tags (#930), and the orchestrator must fail the stage on it.
func TestStageRunnerEndToEnd_FailedCommandWritesFailed(t *testing.T) {
	e := runStageEndToEnd(t, `mkdir -p $OUT && echo partial > $OUT/result.txt && exit 3`, 1)
	if e.runErr == nil {
		t.Error("stage user data exited 0 for a failing command")
	}
	body, ok := e.object(t, pipeline.StageInstanceMarkerKey("pfx", "train", 1, pipeline.MarkerFailed))
	if !ok {
		t.Fatalf("spored wrote no FAILED marker; calls:\n%s", e.calls)
	}
	if !strings.Contains(body, "exit status 3") {
		t.Errorf("FAILED marker body = %q, want the command's failure", body)
	}
	if !e.markerSeen(t, pipeline.MarkerFailed) {
		t.Error("the orchestrator does not see the FAILED marker spored wrote")
	}
	if e.markerSeen(t, pipeline.MarkerComplete) {
		t.Error("COMPLETE marker written for a failed stage")
	}
	old := markers
	markers = e.s3
	defer func() { markers = old }()
	if outcome, _, err := checkStageOutcome(context.Background(), e.state, e.def.GetStage("train"), getStageState(e.state, "train"), time.Now()); err != nil || outcome != stageFailed {
		t.Errorf("the orchestrator's verdict on the stage = %v (%v), want failed", outcome, err)
	}
	list, err := e.s3.ListObjectsV2(context.Background(), &s3.ListObjectsV2Input{
		Bucket: aws.String("bkt"), Prefix: aws.String(pipeline.StageOutputPrefix("pfx", "train")),
	})
	if err == nil && len(list.Contents) > 0 {
		t.Errorf("a failed stage's outputs were uploaded for downstream stages: %d objects", len(list.Contents))
	}
	assertSelfTerminatedWith(t, e.calls, "us-east-1")
}

// #943: a stage of one run reads its inputs from, and reports into, that run
// only. A previous run under the same s3_prefix left its outputs at the old
// stage keys and under its own run; the real spored, given only its instance's
// tags, must download this run's input and write COMPLETE under this run.
func TestStageRunnerEndToEnd_StaysInsideItsRun(t *testing.T) {
	e := runStageEndToEndInRun(t, `mkdir -p $OUT && cat $IN/data.csv > $OUT/result.txt`, 0, "20261015t120000z-0000beef")
	if e.runErr != nil {
		t.Fatalf("stage user data failed: %v\ncalls:\n%s", e.runErr, e.calls)
	}
	if b, err := os.ReadFile(filepath.Join(e.inDir, "data.csv")); err != nil || string(b) != "hello pipeline\n" {
		t.Errorf("the stage received %q (%v), want this run's upstream output, not a previous run's", b, err)
	}
	if !e.markerSeen(t, pipeline.MarkerComplete) {
		t.Error("COMPLETE not written under this run's prefix")
	}
	if _, ok := e.object(t, pipeline.StageInstanceMarkerKey("pfx", "train", 0, pipeline.MarkerComplete)); ok {
		t.Error("COMPLETE written at the shared s3_prefix keys, where the next run would read it")
	}
	list, err := e.s3.ListObjectsV2(context.Background(), &s3.ListObjectsV2Input{
		Bucket: aws.String("bkt"), Prefix: aws.String(pipeline.StageOutputPrefix(e.state.DataPrefix(), "train")),
	})
	if err != nil || len(list.Contents) != 1 || !strings.HasSuffix(aws.ToString(list.Contents[0].Key), "/result.txt") {
		t.Errorf("this run's outputs under %s: %v (%v), want result.txt", pipeline.StageOutputPrefix(e.state.DataPrefix(), "train"), list, err)
	}
}

// #944: through the real spored, an instance of a multi-instance stage uploads
// under its own index, and an absolute data_output pattern keys files by their
// path relative to the pattern's directory, not by their whole path.
func TestStageRunnerEndToEnd_OutputsAreKeyedByInstanceAndPatternDir(t *testing.T) {
	for _, index := range []int{0, 1} {
		e := runStageEndToEnd(t, `mkdir -p $OUT && cp $IN/data.csv $OUT/result.txt`, index)
		if e.runErr != nil {
			t.Fatalf("instance %d: stage user data failed: %v\ncalls:\n%s", index, e.runErr, e.calls)
		}
		// runStageEndToEnd gives "train" index+1 instances.
		want := pipeline.StageOutputPrefix("pfx", "train") + "result.txt"
		if index > 0 {
			want = pipeline.StageOutputPrefix("pfx", "train") + fmt.Sprintf("%d/result.txt", index)
		}
		if got, ok := e.object(t, want); !ok || got != "hello pipeline\n" {
			list, _ := e.s3.ListObjectsV2(context.Background(), &s3.ListObjectsV2Input{Bucket: aws.String("bkt"), Prefix: aws.String("pfx/")})
			var keys []string
			for _, o := range list.Contents {
				keys = append(keys, aws.ToString(o.Key))
			}
			t.Errorf("instance %d of %d: no output at %s; keys %v", index, index+1, want, keys)
		}
	}
}
