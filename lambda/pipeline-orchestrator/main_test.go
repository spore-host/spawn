package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/pipeline"
	"github.com/spore-host/spawn/pkg/tagprefix"
)

// ---- fakes ----------------------------------------------------------------

// fakeMarkers answers HeadObject from a set of keys that "exist".
type fakeMarkers struct {
	present map[string]bool
	err     error
}

func (f *fakeMarkers) HeadObject(_ context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.present[aws.ToString(in.Key)] {
		return &s3.HeadObjectOutput{}, nil
	}
	return nil, &s3types.NotFound{}
}

// fakeEC2 records terminations per region and answers DescribeInstances from
// a map of instance id to state.
type fakeEC2 struct {
	mu         sync.Mutex
	states     map[string]string
	terminated map[string][]string // region -> ids
}

type regionalEC2 struct {
	f      *fakeEC2
	region string
}

func (r regionalEC2) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	var insts []ec2types.Instance
	for _, id := range in.InstanceIds {
		st, ok := r.f.states[id]
		if !ok {
			continue
		}
		insts = append(insts, ec2types.Instance{
			InstanceId: aws.String(id),
			State:      &ec2types.InstanceState{Name: ec2types.InstanceStateName(st)},
		})
	}
	return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: insts}}}, nil
}

func (r regionalEC2) TerminateInstances(_ context.Context, in *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	r.f.terminated[r.region] = append(r.f.terminated[r.region], in.InstanceIds...)
	for _, id := range in.InstanceIds {
		r.f.states[id] = "shutting-down"
	}
	return &ec2.TerminateInstancesOutput{}, nil
}

func (f *fakeEC2) terminatedIn(region, id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.terminated[region] {
		if t == id {
			return true
		}
	}
	return false
}

// harness swaps every AWS seam for a fake and restores them afterwards. It
// fails the test if the orchestrator re-invokes itself.
type harness struct {
	ec2       *fakeEC2
	markers   *fakeMarkers
	saved     int
	reinvoked int
	cleaned   int
	prices    map[string]float64 // instance type -> $/h; negative = unpriceable
}

func newHarness(t *testing.T, state *pipeline.PipelineState) *harness {
	t.Helper()
	h := &harness{
		ec2:     &fakeEC2{states: map[string]string{}, terminated: map[string][]string{}},
		markers: &fakeMarkers{present: map[string]bool{}},
	}
	oldMarkers, oldEC2, oldLoad, oldSave, oldReinvoke, oldNotify, oldNet, oldPrice :=
		markers, ec2ForRegion, loadStateFn, saveStateFn, reinvokeFn, notifyFn, cleanupNetFn, priceFn
	t.Cleanup(func() {
		markers, ec2ForRegion, loadStateFn, saveStateFn, reinvokeFn, notifyFn, cleanupNetFn, priceFn =
			oldMarkers, oldEC2, oldLoad, oldSave, oldReinvoke, oldNotify, oldNet, oldPrice
	})
	// $1/instance-hour unless a test sets h.prices.
	h.prices = map[string]float64{}
	priceFn = func(_ context.Context, instanceType, _ string) (float64, string, error) {
		if p, ok := h.prices[instanceType]; ok {
			if p < 0 {
				return 0, "", fmt.Errorf("no price for %s", instanceType)
			}
			return p, "test", nil
		}
		return 1, "test", nil
	}
	markers = h.markers
	ec2ForRegion = func(_ context.Context, region string) (stageEC2API, error) {
		return regionalEC2{f: h.ec2, region: region}, nil
	}
	loadStateFn = func(context.Context, string) (*pipeline.PipelineState, error) {
		return &pipeline.PipelineState{PipelineID: state.PipelineID}, nil
	}
	saveStateFn = func(context.Context, *pipeline.PipelineState) error { h.saved++; return nil }
	reinvokeFn = func(context.Context, string, string) error { h.reinvoked++; return nil }
	notifyFn = func(context.Context, *pipeline.PipelineState, string, string) error { return nil }
	cleanupNetFn = func(context.Context, *pipeline.PipelineState) { h.cleaned++ }
	return h
}

// twoStagePipeline: "train" (eu-west-1) depends on nothing; "eval" depends on
// "train". "train" is running with one instance.
func twoStagePipeline(onFailure string, created time.Time) (*pipeline.PipelineState, *pipeline.Pipeline) {
	def := &pipeline.Pipeline{
		PipelineID: "pl-1", PipelineName: "demo", S3Bucket: "bkt", S3Prefix: "pfx", OnFailure: onFailure,
		Stages: []pipeline.Stage{
			{StageID: "train", InstanceType: "t3.micro", InstanceCount: 1, Region: "eu-west-1", Timeout: "2h", Command: "false"},
			{StageID: "eval", InstanceType: "t3.micro", InstanceCount: 1, Region: "eu-west-1", Command: "true", DependsOn: []string{"train"}},
		},
	}
	launched := time.Now().Add(-5 * time.Minute)
	state := &pipeline.PipelineState{
		PipelineID: "pl-1", PipelineName: "demo", S3Bucket: "bkt", S3Prefix: "pfx", OnFailure: onFailure,
		CreatedAt: created, Status: pipeline.StatusRunning, TotalStages: 2,
		Stages: []pipeline.StageState{
			{StageID: "train", StageIndex: 0, Status: pipeline.StageStatusRunning, LaunchedAt: &launched,
				Instances: []pipeline.InstanceInfo{{InstanceID: "i-train", State: "pending"}}},
			{StageID: "eval", StageIndex: 1, Status: pipeline.StageStatusPending},
		},
	}
	return state, def
}

func mustOrder(t *testing.T, def *pipeline.Pipeline) []string {
	t.Helper()
	order, err := def.GetTopologicalOrder()
	if err != nil {
		t.Fatal(err)
	}
	return order
}

// ---- the orchestrator -----------------------------------------------------

// #912: a FAILED marker must fail the stage, terminate its instance in the
// stage's own region, apply on_failure=stop and finish the pipeline FAILED,
// without re-invoking. It used to be ignored, so the stage stayed running and
// the Lambda re-invoked itself every ~13 minutes forever.
func TestFailedMarkerFailsThePipelineAndTerminatesItsInstances(t *testing.T) {
	state, def := twoStagePipeline("stop", time.Now())
	h := newHarness(t, state)
	h.ec2.states["i-train"] = "running" // the instance has NOT gone; only the marker says it failed
	h.markers.present[pipeline.StageInstanceMarkerKey("pfx", "train", 0, pipeline.MarkerFailed)] = true

	done, err := pollOnce(context.Background(), state, def, mustOrder(t, def), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !done {
		t.Fatal("pollOnce did not finish the pipeline after its only runnable stage failed under on_failure=stop")
	}
	if state.Status != pipeline.StatusFailed {
		t.Errorf("pipeline status = %s, want FAILED", state.Status)
	}
	if got := getStageState(state, "train").Status; got != pipeline.StageStatusFailed {
		t.Errorf("train status = %s, want failed", got)
	}
	if got := getStageState(state, "eval").Status; got != pipeline.StageStatusSkipped {
		t.Errorf("eval status = %s, want skipped (its dependency failed)", got)
	}
	if !h.ec2.terminatedIn("eu-west-1", "i-train") {
		t.Errorf("the failed stage's instance was not terminated in its region eu-west-1; terminations: %v", h.ec2.terminated)
	}
	if h.reinvoked != 0 {
		t.Errorf("re-invoked %d times after the pipeline failed", h.reinvoked)
	}
}

// A FAILED marker wins over COMPLETE: with several instances one may fail while
// another completes, and the stage did not succeed.
func TestFailedMarkerWinsOverComplete(t *testing.T) {
	state, def := twoStagePipeline("stop", time.Now())
	h := newHarness(t, state)
	h.ec2.states["i-train"] = "running"
	h.markers.present[pipeline.StageInstanceMarkerKey("pfx", "train", 0, pipeline.MarkerFailed)] = true
	h.markers.present[pipeline.StageInstanceMarkerKey("pfx", "train", 0, pipeline.MarkerComplete)] = true

	outcome, _, err := checkStageOutcome(context.Background(), state, def.GetStage("train"), getStageState(state, "train"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if outcome != stageFailed {
		t.Errorf("outcome = %v with both markers present, want stageFailed", outcome)
	}
}

// Instances that have all gone without writing COMPLETE are a failure, read from
// EC2, not from the InstanceInfo.State recorded at launch (which said "pending"
// forever). The old fallback counted this as COMPLETED.
func TestStageWhoseInstancesVanishedWithoutCompletingFails(t *testing.T) {
	state, def := twoStagePipeline("stop", time.Now())
	h := newHarness(t, state)
	h.ec2.states["i-train"] = "terminated"

	outcome, _, err := checkStageOutcome(context.Background(), state, def.GetStage("train"), getStageState(state, "train"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if outcome != stageFailed {
		t.Errorf("outcome = %v for a stage whose only instance terminated with no marker, want stageFailed", outcome)
	}
}

// A stage that reports nothing and whose instance still looks alive fails once
// it outlives its TTL.
func TestSilentStageFailsAfterItsTTL(t *testing.T) {
	state, def := twoStagePipeline("stop", time.Now())
	h := newHarness(t, state)
	h.ec2.states["i-train"] = "running"
	st := getStageState(state, "train")

	outcome, _, _ := checkStageOutcome(context.Background(), state, def.GetStage("train"), st, st.LaunchedAt.Add(time.Minute))
	if outcome != stageStillRunning {
		t.Fatalf("outcome = %v one minute in, want still running", outcome)
	}
	outcome, _, _ = checkStageOutcome(context.Background(), state, def.GetStage("train"), st, st.LaunchedAt.Add(stageTTL(def.GetStage("train"))+time.Second))
	if outcome != stageFailed {
		t.Errorf("outcome = %v past the stage TTL, want stageFailed", outcome)
	}
}

// An S3 error that is not "not found" is not evidence either way.
func TestMarkerErrorIsNotReadAsAbsent(t *testing.T) {
	state, def := twoStagePipeline("stop", time.Now())
	h := newHarness(t, state)
	h.ec2.states["i-train"] = "terminated"
	h.markers.err = fmt.Errorf("AccessDenied")

	if _, _, err := checkStageOutcome(context.Background(), state, def.GetStage("train"), getStageState(state, "train"), time.Now()); err == nil {
		t.Error("an S3 access error was swallowed; the stage would be judged on instance state alone")
	}
}

// on_failure=continue: the failed stage's instance is terminated, its
// dependents are skipped, and an independent running stage is left alone.
func TestContinuePolicySkipsDependentsAndSparesIndependents(t *testing.T) {
	state, def := twoStagePipeline("continue", time.Now())
	def.Stages = append(def.Stages, pipeline.Stage{StageID: "other", InstanceType: "t3.micro", InstanceCount: 1, Region: "us-west-2", Command: "sleep 1"})
	launched := time.Now()
	state.Stages = append(state.Stages, pipeline.StageState{StageID: "other", StageIndex: 2, Status: pipeline.StageStatusRunning, LaunchedAt: &launched,
		Instances: []pipeline.InstanceInfo{{InstanceID: "i-other", State: "pending"}}})
	h := newHarness(t, state)
	h.ec2.states["i-train"] = "running"
	h.ec2.states["i-other"] = "running"
	h.markers.present["pfx/stages/train/FAILED"] = true

	done, err := pollOnce(context.Background(), state, def, mustOrder(t, def), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Fatal("pipeline finished although an independent stage is still running under on_failure=continue")
	}
	if !h.ec2.terminatedIn("eu-west-1", "i-train") {
		t.Errorf("failed stage's instance not terminated; terminations: %v", h.ec2.terminated)
	}
	if h.ec2.terminatedIn("us-west-2", "i-other") {
		t.Error("on_failure=continue terminated an independent running stage")
	}
	if got := getStageState(state, "eval").Status; got != pipeline.StageStatusSkipped {
		t.Errorf("eval status = %s, want skipped: it depends on a failed stage and would otherwise stay pending forever", got)
	}
}

// on_failure=stop terminates stages that are still in flight, in their regions.
func TestStopPolicyTerminatesInFlightStages(t *testing.T) {
	state, def := twoStagePipeline("stop", time.Now())
	def.Stages = append(def.Stages, pipeline.Stage{StageID: "other", InstanceType: "t3.micro", InstanceCount: 1, Region: "us-west-2", Command: "sleep 1"})
	launched := time.Now()
	state.Stages = append(state.Stages, pipeline.StageState{StageID: "other", StageIndex: 2, Status: pipeline.StageStatusRunning, LaunchedAt: &launched,
		Instances: []pipeline.InstanceInfo{{InstanceID: "i-other", State: "pending"}}})
	h := newHarness(t, state)
	h.ec2.states["i-train"] = "running"
	h.ec2.states["i-other"] = "running"

	failStage(context.Background(), state, def, getStageState(state, "train"), "boom")

	if !h.ec2.terminatedIn("us-west-2", "i-other") {
		t.Errorf("on_failure=stop left an in-flight stage's instance running; terminations: %v", h.ec2.terminated)
	}
	if !allStagesComplete(state) {
		t.Errorf("stages not all terminal after a stop: %+v", state.Stages)
	}
}

// #912: the orchestrator must stop re-invoking itself once the pipeline is past
// its deadline, failing it and terminating what is left. It used to re-invoke
// every ~13 minutes for as long as a stage stayed "running".
func TestPipelinePastItsDeadlineIsFailedNotReinvoked(t *testing.T) {
	_, def := twoStagePipeline("stop", time.Time{})
	state, _ := twoStagePipeline("stop", time.Now().Add(-1000*time.Hour))
	h := newHarness(t, state)
	h.ec2.states["i-train"] = "running"

	// pollOnce rather than runPipelinePollingLoop: against a revert the loop
	// would poll for its full 13-minute invocation before re-invoking, and the
	// loop's only way out short of that is pollOnce reporting done.
	done, err := pollOnce(context.Background(), state, def, mustOrder(t, def), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !done {
		t.Fatal("pollOnce did not finish a pipeline 1000h past creation, so the loop would re-invoke")
	}
	if h.reinvoked != 0 {
		t.Errorf("re-invoked %d times for a pipeline past its deadline", h.reinvoked)
	}
	if state.Status != pipeline.StatusFailed {
		t.Errorf("status = %s, want FAILED", state.Status)
	}
	if !h.ec2.terminatedIn("eu-west-1", "i-train") {
		t.Errorf("instance of a timed-out pipeline not terminated; terminations: %v", h.ec2.terminated)
	}
	if h.cleaned == 0 {
		t.Error("network resources not cleaned up")
	}
}

// The deadline must not fire on a healthy pipeline: it is at least the sum of
// the stages' own TTLs.
func TestPipelineDeadlineCoversASerialRun(t *testing.T) {
	state, def := twoStagePipeline("stop", time.Now())
	var serial time.Duration
	for i := range def.Stages {
		serial += stageTTL(&def.Stages[i])
	}
	if got := pipelineDeadline(state, def).Sub(state.CreatedAt); got < serial {
		t.Errorf("pipeline deadline %s is shorter than a serial run of its stages (%s)", got, serial)
	}
}

// Budget exceeded: the pipeline is finished and its instances terminated. This
// used to `break` out of the switch only, leaving the loop running, the status
// unsaved and the other stages' instances untouched.
func TestBudgetExceededTerminatesAndFinishes(t *testing.T) {
	state, def := twoStagePipeline("stop", time.Now())
	def.Stages = append(def.Stages, pipeline.Stage{StageID: "other", InstanceType: "t3.micro", InstanceCount: 1, Region: "us-west-2", Command: "sleep 1"})
	// Recent, so "other" is inside its own TTL whichever order the stages are
	// visited in; only "train"'s ten hours are meant to blow the budget.
	launched := time.Now().Add(-time.Minute)
	state.Stages = append(state.Stages, pipeline.StageState{StageID: "other", StageIndex: 2, Status: pipeline.StageStatusRunning, LaunchedAt: &launched,
		Instances: []pipeline.InstanceInfo{{InstanceID: "i-other", State: "pending"}}})
	budget := 0.01
	state.MaxCostUSD = &budget
	h := newHarness(t, state)
	h.ec2.states["i-train"] = "running"
	h.ec2.states["i-other"] = "running"
	h.markers.present[pipeline.StageInstanceMarkerKey("pfx", "train", 0, pipeline.MarkerComplete)] = true
	trainLaunched := time.Now().Add(-10 * time.Hour)
	getStageState(state, "train").LaunchedAt = &trainLaunched

	done, err := pollOnce(context.Background(), state, def, mustOrder(t, def), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !done {
		t.Fatal("pipeline not finished after its budget was exceeded")
	}
	if state.Status != pipeline.StatusCancelled {
		t.Errorf("status = %s, want CANCELLED", state.Status)
	}
	if !h.ec2.terminatedIn("us-west-2", "i-other") {
		t.Errorf("budget-exceeded pipeline left a running stage's instance; terminations: %v", h.ec2.terminated)
	}
}

// ---- tags and RunInstances ------------------------------------------------

func tagMap(tags []ec2types.Tag) map[string]string {
	m := map[string]string{}
	for _, t := range tags {
		m[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return m
}

// #912: the TTL reaper lists only instances tagged <prefix>:managed=true
// (lambda/ttl-reaper scanRegion) and reaps on <prefix>:ttl-deadline. A stage
// instance must carry both, with a deadline after the stage's own timeout (so
// a stage still inside its timeout is never reaped) and before the reaper's
// max-age ceiling would matter.
func TestStageInstanceIsVisibleToTheReaper(t *testing.T) {
	state, def := twoStagePipeline("stop", time.Now())
	stageDef := def.GetStage("train") // timeout 2h
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

	in := buildStageRunInstancesInput(state, stageDef, getStageState(state, "train"), 0, "ami-x", "ud", now)
	var tags map[string]string
	for _, spec := range in.TagSpecifications {
		if spec.ResourceType == ec2types.ResourceTypeInstance {
			tags = tagMap(spec.Tags)
		}
	}
	if tags == nil {
		t.Fatal("RunInstances input has no instance tag specification")
	}
	if got := tags[tagprefix.Tag("managed")]; got != "true" {
		t.Errorf("%s = %q, want \"true\": the reaper's DescribeInstances filters on it and its terminate grant is conditioned on it",
			tagprefix.Tag("managed"), got)
	}
	dl, err := time.Parse(time.RFC3339, tags[tagprefix.Tag("ttl-deadline")])
	if err != nil {
		t.Fatalf("%s = %q, not RFC3339 as the reaper parses it: %v", tagprefix.Tag("ttl-deadline"), tags[tagprefix.Tag("ttl-deadline")], err)
	}
	if !dl.After(now.Add(2 * time.Hour)) {
		t.Errorf("ttl-deadline %s is not after the stage's 2h timeout from %s; the reaper would kill a running stage", dl, now)
	}
	if dl.After(now.Add(2*time.Hour + time.Hour)) {
		t.Errorf("ttl-deadline %s is more than an hour past the stage's timeout", dl)
	}
	if lt, err := time.Parse(time.RFC3339, tags[tagprefix.Tag("launch-time")]); err != nil || !lt.Equal(now) {
		t.Errorf("%s = %q, want the launch time %s (the reaper's max-age anchor)", tagprefix.Tag("launch-time"), tags[tagprefix.Tag("launch-time")], now.Format(time.RFC3339))
	}
	if tags[tagprefix.Tag("pipeline-id")] != "pl-1" || tags[tagprefix.Tag("stage-id")] != "train" {
		t.Errorf("pipeline/stage tags lost: %v", tags)
	}
}

// A stage with no timeout still gets a deadline, from the default.
func TestStageWithoutTimeoutStillGetsADeadline(t *testing.T) {
	state, def := twoStagePipeline("stop", time.Now())
	stageDef := def.GetStage("eval") // no timeout
	tags := tagMap(stageInstanceTags(state, stageDef, getStageState(state, "eval"), 0, time.Now()))
	if _, err := time.Parse(time.RFC3339, tags[tagprefix.Tag("ttl-deadline")]); err != nil {
		t.Errorf("no parseable ttl-deadline for a stage without a timeout: %q", tags[tagprefix.Tag("ttl-deadline")])
	}
}

// The user data halts the instance as its last resort; that is only a
// terminate if the instance is launched with shutdown behaviour terminate.
func TestStageInstanceShutdownTerminates(t *testing.T) {
	state, def := twoStagePipeline("stop", time.Now())
	for _, spot := range []bool{false, true} {
		def.Stages[0].Spot = spot
		in := buildStageRunInstancesInput(state, def.GetStage("train"), getStageState(state, "train"), 0, "ami-x", "ud", time.Now())
		if in.InstanceInitiatedShutdownBehavior != ec2types.ShutdownBehaviorTerminate {
			t.Errorf("spot=%v: InstanceInitiatedShutdownBehavior = %q, want terminate", spot, in.InstanceInitiatedShutdownBehavior)
		}
	}
}

// Every RunInstances in this file must go through buildStageRunInstancesInput,
// so a second launch path cannot skip the tags or the shutdown behaviour.
func TestEveryRunInstancesUsesTheStageInput(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	runs := strings.Count(src, ".RunInstances(ctx, ")
	builds := strings.Count(src, "input := buildStageRunInstancesInput(")
	if runs == 0 {
		t.Fatal("found no RunInstances call; this matcher is stale")
	}
	if runs != builds {
		t.Errorf("%d RunInstances call(s) but %d built by buildStageRunInstancesInput", runs, builds)
	}
}

// ---- the user data, executed ----------------------------------------------

// userDataRun is one execution of the generated stage user data.
type userDataRun struct {
	state        *pipeline.PipelineState
	stageDef     *pipeline.Stage
	stageState   *pipeline.StageState
	region       string
	index        int      // the instance index within its stage
	spored       string   // the file the fake curl serves as the spored binary
	failDownload bool     // every binary download fails
	env          []string // extra environment, e.g. AWS endpoints for a real spored
}

// execUserData executes the generated user data under bash with every external
// command it reaches faked except spored, which is whatever r.spored is, and
// returns the log of what the fakes were asked to do. Paths that need root are
// redirected into a temp dir.
func execUserData(t *testing.T, r userDataRun) (string, error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		// The script is a Linux instance's user-data, and the harness roots it by
		// splicing native paths into it, which bash mangles as C:\... on Windows.
		// The failure-path tests only passed there because the script failed.
		t.Skip("Linux-only: executes the pipeline stage's Linux user-data under bash (#1002)")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	ud, err := generatePipelineUserData(r.state, r.stageDef, r.stageState, r.index, r.region)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(ud)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, "fakebin")
	root := filepath.Join(dir, "root")
	logf := filepath.Join(dir, "calls.log")
	for _, d := range []string{bin, filepath.Join(root, "usr/local/bin"), filepath.Join(root, "opt"), filepath.Join(root, "tmp")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// One pass, so a temp dir that itself lives under /tmp is not rewritten twice.
	script := strings.NewReplacer(
		"/usr/local/bin", root+"/usr/local/bin",
		"/opt/spawn", root+"/opt/spawn",
		"/tmp/", root+"/tmp/",
	).Replace(string(raw))

	fakes := map[string]string{
		// curl: IMDS answers only with a v2 token; binary downloads serve
		// $SPORED_SRC, or fail.
		"curl": `out=""; url=""; tok=""; prev=""
for a in "$@"; do
  case "$prev" in -o) out="$a";; -H) case "$a" in X-aws-ec2-metadata-token:*) tok="${a#*: }";; esac;; esac
  case "$a" in http*) url="$a";; esac
  prev="$a"
done
echo "curl $url" >> "$CALLS"
case "$url" in
  */api/token) echo tok123;;
  */meta-data/instance-id) [ "$tok" = tok123 ] && echo i-0fromimds || exit 22;;
  */meta-data/placement/region) echo eu-west-1;;
  *.sha256) echo abc > "$out";;
  *) [ -n "$FAIL_DOWNLOAD" ] && exit 22; cp "$SPORED_SRC" "$out";;
esac`,
		"aws":       `[ "$1 $2" = "s3 cp" ] && cat >/dev/null; echo "aws $*" >> "$CALLS"`,
		"shutdown":  `echo "shutdown $*" >> "$CALLS"`,
		"poweroff":  `echo "poweroff" >> "$CALLS"`,
		"sleep":     `:`,
		"uname":     `echo x86_64`,
		"sha256sum": `echo "abc  $1"`,
		"timeout":   `[ "$1" = -k ] && shift 2; shift; exec "$@"`,
	}
	for name, body := range fakes {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/bash\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// ec2-metadata is deliberately absent, as on Ubuntu and Debian.
	cmd := exec.Command(bash, "-c", script)
	cmd.Env = append([]string{"PATH=" + bin + ":/usr/bin:/bin", "CALLS=" + logf, "HOME=" + dir,
		"SPORED_SRC=" + r.spored}, r.env...)
	if r.failDownload {
		cmd.Env = append(cmd.Env, "FAIL_DOWNLOAD=1")
	}
	out, runErr := cmd.CombinedOutput()
	calls, _ := os.ReadFile(logf)
	t.Logf("script output:\n%s", out)
	return string(calls), runErr
}

// runUserData runs the "train" stage's user data with a fake spored whose
// whole body is sporedBody, logging its arguments.
func runUserData(t *testing.T, sporedBody string, failDownload bool) (string, error) {
	t.Helper()
	state, def := twoStagePipeline("stop", time.Now())
	fake := filepath.Join(t.TempDir(), "spored")
	if err := os.WriteFile(fake, []byte("#!/bin/bash\necho \"spored $*\" >> \"$CALLS\"\n"+sporedBody+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return execUserData(t, userDataRun{
		state: state, stageDef: def.GetStage("train"), stageState: getStageState(state, "train"),
		region: "eu-west-1", spored: fake, failDownload: failDownload,
	})
}

func assertSelfTerminated(t *testing.T, calls string) {
	t.Helper()
	assertSelfTerminatedWith(t, calls, "eu-west-1")
}

func assertSelfTerminatedWith(t *testing.T, calls, region string) {
	t.Helper()
	if !strings.Contains(calls, "aws ec2 terminate-instances --instance-ids i-0fromimds --region "+region) {
		t.Errorf("instance did not terminate itself through the API with the IMDSv2 instance id; calls:\n%s", calls)
	}
	if !strings.Contains(calls, "shutdown -h now") {
		t.Errorf("instance did not halt as the IAM-free backstop; calls:\n%s", calls)
	}
}

// #912: a stage that fails must write FAILED and terminate. It used to write
// FAILED and exit, leaving the instance running. The stage is run by spored
// (#910); here a fake one exits 3.
func TestUserData_FailedStageTerminates(t *testing.T) {
	calls, err := runUserData(t, "exit 3", false)
	if err == nil {
		t.Error("script exited 0 for a failing stage")
	}
	if !strings.Contains(calls, "aws s3 cp - s3://bkt/"+pipeline.StageInstanceMarkerKey("pfx", "train", 0, pipeline.MarkerFailed)) {
		t.Errorf("fallback FAILED marker not written at the key the orchestrator reads; calls:\n%s", calls)
	}
	assertSelfTerminated(t, calls)
}

// The success path must terminate too, without ec2-metadata (absent here, as
// on Ubuntu), and must not write FAILED. COMPLETE is spored's to write, after
// the outputs (TestStageRunnerEndToEnd_InputsDownloadedOutputsUploaded).
func TestUserData_SuccessfulStageTerminatesWithoutEC2Metadata(t *testing.T) {
	calls, err := runUserData(t, "exit 0", false)
	if err != nil {
		t.Errorf("script failed for a succeeding stage: %v", err)
	}
	if !strings.Contains(calls, "spored run-pipeline-stage") {
		t.Errorf("the stage runner was not run; calls:\n%s", calls)
	}
	if strings.Contains(calls, "/FAILED") {
		t.Errorf("FAILED marker written for a successful stage; calls:\n%s", calls)
	}
	assertSelfTerminated(t, calls)
}

// A bootstrap failure under set -e, before the stage command runs, must
// terminate the instance and report the failure.
func TestUserData_BootstrapFailureTerminates(t *testing.T) {
	calls, err := runUserData(t, "exit 0", true)
	if err == nil {
		t.Error("script exited 0 although spored could not be downloaded")
	}
	if !strings.Contains(calls, "/FAILED") {
		t.Errorf("FAILED marker not written for a bootstrap failure; calls:\n%s", calls)
	}
	assertSelfTerminated(t, calls)
}

// ---- what the stage runner needs from the orchestrator (#910) -------------

// fakeLauncher records RunInstances calls and the order of side effects.
type fakeLauncher struct {
	events *[]string
	inputs []*ec2.RunInstancesInput
}

func (f *fakeLauncher) RunInstances(_ context.Context, in *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
	f.inputs = append(f.inputs, in)
	*f.events = append(*f.events, "run")
	id := fmt.Sprintf("i-%d", len(f.inputs))
	return &ec2.RunInstancesOutput{Instances: []ec2types.Instance{{
		InstanceId: aws.String(id), State: &ec2types.InstanceState{Name: ec2types.InstanceStateNamePending},
	}}}, nil
}

// launchStage must write the pipeline definition to the key every stage
// instance's s3-config-key tag names, before any instance can boot and read
// it, and tag the instances with the bucket and prefix the runner's data
// handler uses, under the scoped instance profile. Without s3-config-key the
// runner refuses to start; without the profile it cannot read or write the
// pipeline's S3 paths.
func TestLaunchStageGivesTheRunnerItsTagsConfigAndProfile(t *testing.T) {
	state, def := twoStagePipeline("stop", time.Now())
	state.InstanceProfile = "spawn-instance-abc12345"
	def.Stages[0].AMI = "ami-given" // no AMI lookup
	def.Stages[0].InstanceCount = 2
	stageState := &pipeline.StageState{StageID: "train", StageIndex: 0, Status: pipeline.StageStatusReady}

	var events []string
	written := map[string][]byte{}
	launcher := &fakeLauncher{events: &events}
	oldPut, oldLaunch, oldAMI, oldPrice := putObjectFn, launchEC2ForRegion, recommendedAMIFn, priceFn
	t.Cleanup(func() {
		putObjectFn, launchEC2ForRegion, recommendedAMIFn, priceFn = oldPut, oldLaunch, oldAMI, oldPrice
	})
	// The stage is priced before launch (#930); the real priceFn calls the
	// AWS Price List.
	priceFn = func(context.Context, string, string) (float64, string, error) { return 1, "test", nil }
	putObjectFn = func(_ context.Context, in *s3.PutObjectInput) error {
		b, _ := io.ReadAll(in.Body)
		written["s3://"+aws.ToString(in.Bucket)+"/"+aws.ToString(in.Key)] = b
		events = append(events, "put")
		return nil
	}
	var launchRegion string
	launchEC2ForRegion = func(_ context.Context, region string) (runInstancesAPI, error) {
		launchRegion = region
		return launcher, nil
	}
	recommendedAMIFn = func(context.Context, string, string) (string, error) {
		t.Error("AMI lookup called although the stage names an AMI")
		return "", nil
	}

	if err := launchStage(context.Background(), state, def, def.GetStage("train"), stageState); err != nil {
		t.Fatal(err)
	}
	if launchRegion != "eu-west-1" {
		t.Errorf("launched in %q, want the stage's region eu-west-1", launchRegion)
	}
	if len(launcher.inputs) != 2 || len(events) == 0 || events[0] != "put" {
		t.Fatalf("want the stage config written before %d launches; events %v", 2, events)
	}
	for i, in := range launcher.inputs {
		tags := tagMap(in.TagSpecifications[0].Tags)
		cfgURL := tags[tagprefix.Tag("s3-config-key")]
		body, ok := written[cfgURL]
		if !ok {
			t.Errorf("instance %d: %s = %q, which nothing was written to (wrote %v)", i, tagprefix.Tag("s3-config-key"), cfgURL, keys(written))
			continue
		}
		var got pipeline.Pipeline
		if err := json.Unmarshal(body, &got); err != nil || got.GetStage("train") == nil {
			t.Errorf("instance %d: the stage config is not the pipeline definition: %v %s", i, err, body)
		}
		if cfgURL != "s3://bkt/"+pipeline.StageConfigKey("pfx", "pl-1") {
			t.Errorf("stage config at %q, want it inside the pipeline's bucket/prefix, which the stage profile is granted", cfgURL)
		}
		if tags[tagprefix.Tag("s3-bucket")] != "bkt" || tags[tagprefix.Tag("s3-prefix")] != "pfx" {
			t.Errorf("instance %d: s3-bucket/s3-prefix tags = %q/%q, want bkt/pfx (the marker and data location the orchestrator reads)",
				i, tags[tagprefix.Tag("s3-bucket")], tags[tagprefix.Tag("s3-prefix")])
		}
		if tags[tagprefix.Tag("instance-index")] != fmt.Sprintf("%d", i) {
			t.Errorf("instance %d: instance-index tag = %q", i, tags[tagprefix.Tag("instance-index")])
		}
		if got := aws.ToString(in.IamInstanceProfile.Name); got != "spawn-instance-abc12345" {
			t.Errorf("instance %d: launched with profile %q, want the pipeline's scoped profile", i, got)
		}
	}
}

// A record written before the scoped profile existed still launches, with the
// shared default.
func TestStageInstanceProfileFallsBackToTheDefault(t *testing.T) {
	if got := stageInstanceProfile(&pipeline.PipelineState{}); got != spawnaws.SporedInstanceProfileName {
		t.Errorf("profile = %q, want %q", got, spawnaws.SporedInstanceProfileName)
	}
}

// The fallback FAILED marker the user data writes is at the orchestrator's key.
func TestUserDataFallbackMarkerIsTheOrchestratorsKey(t *testing.T) {
	state, def := twoStagePipeline("stop", time.Now())
	ud, err := generatePipelineUserData(state, def.GetStage("train"), getStageState(state, "train"), 0, "eu-west-1")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.StdEncoding.DecodeString(ud)
	want := "SPAWN_FAILED_MARKER='s3://bkt/" + pipeline.StageInstanceMarkerKey("pfx", "train", 0, pipeline.MarkerFailed) + "'"
	if !strings.Contains(string(raw), "\n"+want+"\n") {
		t.Errorf("user data does not assign %s", want)
	}
	if !regexp.MustCompile(`(?m)^timeout -k 60 \d+ /usr/local/bin/spored run-pipeline-stage$`).Match(raw) {
		t.Error("user data does not run spored run-pipeline-stage under the stage timeout")
	}
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
