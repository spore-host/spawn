package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spore-host/spawn/pkg/pipeline"
)

// ---- #929: network resources live in each stage's region -------------------

// fakeNet records network calls per region.
type fakeNet struct {
	mu      sync.Mutex
	calls   []string // "<region> <op> <id>"
	ingress map[string][]ec2types.IpPermission
	next    int
}

type regionalNet struct {
	f      *fakeNet
	region string
}

func (r regionalNet) log(op, id string) {
	r.f.calls = append(r.f.calls, r.region+" "+op+" "+id)
}

func (r regionalNet) CreateSecurityGroup(_ context.Context, in *ec2.CreateSecurityGroupInput, _ ...func(*ec2.Options)) (*ec2.CreateSecurityGroupOutput, error) {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	r.f.next++
	id := fmt.Sprintf("sg-%s-%d", r.region, r.f.next)
	r.log("create-sg", id)
	return &ec2.CreateSecurityGroupOutput{GroupId: aws.String(id)}, nil
}

func (r regionalNet) AuthorizeSecurityGroupIngress(_ context.Context, in *ec2.AuthorizeSecurityGroupIngressInput, _ ...func(*ec2.Options)) (*ec2.AuthorizeSecurityGroupIngressOutput, error) {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	r.log("authorize", aws.ToString(in.GroupId))
	r.f.ingress[aws.ToString(in.GroupId)] = in.IpPermissions
	return &ec2.AuthorizeSecurityGroupIngressOutput{}, nil
}

func (r regionalNet) DeleteSecurityGroup(_ context.Context, in *ec2.DeleteSecurityGroupInput, _ ...func(*ec2.Options)) (*ec2.DeleteSecurityGroupOutput, error) {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	r.log("delete-sg", aws.ToString(in.GroupId))
	return &ec2.DeleteSecurityGroupOutput{}, nil
}

func (r regionalNet) CreatePlacementGroup(_ context.Context, in *ec2.CreatePlacementGroupInput, _ ...func(*ec2.Options)) (*ec2.CreatePlacementGroupOutput, error) {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	r.log("create-pg", aws.ToString(in.GroupName))
	return &ec2.CreatePlacementGroupOutput{}, nil
}

func (r regionalNet) DeletePlacementGroup(_ context.Context, in *ec2.DeletePlacementGroupInput, _ ...func(*ec2.Options)) (*ec2.DeletePlacementGroupOutput, error) {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	r.log("delete-pg", aws.ToString(in.GroupName))
	return &ec2.DeletePlacementGroupOutput{}, nil
}

func (f *fakeNet) has(call string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == call {
			return true
		}
	}
	return false
}

func installFakeNet(t *testing.T, lambdaRegion string) *fakeNet {
	t.Helper()
	f := &fakeNet{ingress: map[string][]ec2types.IpPermission{}}
	oldNet, oldLegacy := netEC2ForRegion, legacyNetworkRegion
	t.Cleanup(func() { netEC2ForRegion, legacyNetworkRegion = oldNet, oldLegacy })
	netEC2ForRegion = func(_ context.Context, region string) (networkEC2API, error) {
		return regionalNet{f: f, region: region}, nil
	}
	legacyNetworkRegion = func() string { return lambdaRegion }
	return f
}

// A streaming pipeline whose stages are in eu-west-1 and us-west-2 (neither
// the Lambda's us-east-1); "train" is EFA.
func multiRegionStreamingPipeline() (*pipeline.PipelineState, *pipeline.Pipeline) {
	def := &pipeline.Pipeline{
		PipelineID: "pl-net", PipelineName: "net", S3Bucket: "bkt", S3Prefix: "pfx",
		Stages: []pipeline.Stage{
			{StageID: "producer", InstanceType: "c5.large", InstanceCount: 1, Region: "eu-west-1", Command: "true",
				DataOutput: &pipeline.DataConfig{Mode: "stream", Protocol: "tcp", Port: 50001}},
			{StageID: "train", InstanceType: "p4d.24xlarge", InstanceCount: 2, Region: "us-west-2", Command: "true", EFAEnabled: true,
				DataInput: &pipeline.DataConfig{Mode: "stream", Protocol: "tcp", SourceStage: "producer"}},
		},
	}
	state := &pipeline.PipelineState{PipelineID: "pl-net", PipelineName: "net", S3Bucket: "bkt", S3Prefix: "pfx"}
	return state, def
}

// #929: the security group must exist in every stage's region and the
// placement group in every EFA stage's region, and each stage must be launched
// with the group from its own region. They used to be created once, in the
// Lambda's region, and handed to RunInstances in the stage's region.
func TestNetworkResourcesAreCreatedAndUsedInEachStagesRegion(t *testing.T) {
	f := installFakeNet(t, "us-east-1")
	state, def := multiRegionStreamingPipeline()

	if err := setupNetworkResources(context.Background(), state, def); err != nil {
		t.Fatal(err)
	}
	for _, region := range []string{"eu-west-1", "us-west-2"} {
		sg := state.SecurityGroups[region]
		if !strings.HasPrefix(sg, "sg-"+region+"-") {
			t.Errorf("security group for %s = %q, not one created in %s; calls %v", region, sg, region, f.calls)
		}
	}
	if !f.has("us-west-2 create-pg spawn-pipeline-pl-net") {
		t.Errorf("placement group not created in the EFA stage's region us-west-2; calls %v", f.calls)
	}
	if f.has("eu-west-1 create-pg spawn-pipeline-pl-net") {
		t.Errorf("placement group created in eu-west-1, which has no EFA stage; calls %v", f.calls)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "us-east-1 ") {
			t.Errorf("a network resource was created in the Lambda's region, where no stage runs: %s", c)
		}
	}

	for _, stageID := range []string{"producer", "train"} {
		st := def.GetStage(stageID)
		in := buildStageRunInstancesInput(state, st, &pipeline.StageState{StageID: stageID}, 0, "ami-x", "ud", time.Now())
		want := state.SecurityGroups[st.Region]
		if len(in.SecurityGroupIds) != 1 || in.SecurityGroupIds[0] != want {
			t.Errorf("stage %s (%s) launched with security groups %v, want [%s] from its own region", stageID, st.Region, in.SecurityGroupIds, want)
		}
	}
	trainIn := buildStageRunInstancesInput(state, def.GetStage("train"), &pipeline.StageState{StageID: "train"}, 0, "ami-x", "ud", time.Now())
	if trainIn.Placement == nil || aws.ToString(trainIn.Placement.GroupName) != state.PlacementGroups["us-west-2"] {
		t.Errorf("EFA stage placement = %+v, want the us-west-2 group %q", trainIn.Placement, state.PlacementGroups["us-west-2"])
	}

	// Cleanup deletes each group in the region it was created in.
	cleanupNetworkResources(context.Background(), state)
	for region, sg := range state.SecurityGroups {
		if !f.has(region + " delete-sg " + sg) {
			t.Errorf("security group %s not deleted in %s; calls %v", sg, region, f.calls)
		}
	}
	if !f.has("us-west-2 delete-pg spawn-pipeline-pl-net") {
		t.Errorf("placement group not deleted in us-west-2; calls %v", f.calls)
	}
}

// #929: nothing in the streaming group is open to an address range. SSH was
// open to 0.0.0.0/0; it, and the streaming ports, are from the group itself.
func TestStreamingSecurityGroupAdmitsOnlyItsOwnMembers(t *testing.T) {
	f := installFakeNet(t, "us-east-1")
	state, def := multiRegionStreamingPipeline()
	if err := setupNetworkResources(context.Background(), state, def); err != nil {
		t.Fatal(err)
	}
	for region, sg := range state.SecurityGroups {
		perms := f.ingress[sg]
		if len(perms) == 0 {
			t.Errorf("%s: no ingress authorized on %s", region, sg)
		}
		ssh := false
		for _, p := range perms {
			if len(p.IpRanges) > 0 || len(p.Ipv6Ranges) > 0 || len(p.PrefixListIds) > 0 {
				t.Errorf("%s: ingress %d-%d is open to an address range: %+v %+v", region,
					aws.ToInt32(p.FromPort), aws.ToInt32(p.ToPort), p.IpRanges, p.Ipv6Ranges)
			}
			if len(p.UserIdGroupPairs) != 1 || aws.ToString(p.UserIdGroupPairs[0].GroupId) != sg {
				t.Errorf("%s: ingress %d-%d is not limited to the group itself: %+v", region,
					aws.ToInt32(p.FromPort), aws.ToInt32(p.ToPort), p.UserIdGroupPairs)
			}
			if aws.ToInt32(p.FromPort) <= 22 && 22 <= aws.ToInt32(p.ToPort) {
				ssh = true
			}
		}
		if !ssh {
			t.Errorf("%s: SSH between the pipeline's own instances is no longer allowed", region)
		}
	}
}

// A pipeline set up before #929 has one legacy group, in the Lambda's region.
// It is used only for stages in that region, and cleaned up there.
func TestLegacySingleGroupIsUsedOnlyInTheLambdasRegion(t *testing.T) {
	f := installFakeNet(t, "us-east-1")
	//lint:ignore SA1019 builds the legacy pre-#929 state this test is about
	state := &pipeline.PipelineState{SecurityGroupID: "sg-legacy", PlacementGroupID: "pg-legacy"}
	if got := securityGroupFor(state, "us-east-1"); got != "sg-legacy" {
		t.Errorf("legacy group in its own region = %q, want sg-legacy", got)
	}
	if got := securityGroupFor(state, "eu-west-1"); got != "" {
		t.Errorf("legacy us-east-1 group handed to a eu-west-1 launch: %q", got)
	}
	if got := placementGroupFor(state, "eu-west-1"); got != "" {
		t.Errorf("legacy us-east-1 placement group handed to a eu-west-1 launch: %q", got)
	}
	cleanupNetworkResources(context.Background(), state)
	if !f.has("us-east-1 delete-sg sg-legacy") || !f.has("us-east-1 delete-pg pg-legacy") {
		t.Errorf("legacy groups not cleaned up in the Lambda's region; calls %v", f.calls)
	}
}

// A setup that fails part way leaves what it created recorded, so it is
// cleaned up.
func TestPartialNetworkSetupIsRecorded(t *testing.T) {
	f := installFakeNet(t, "us-east-1")
	state, def := multiRegionStreamingPipeline()
	netEC2ForRegion = func(_ context.Context, region string) (networkEC2API, error) {
		if region == "us-west-2" {
			return nil, fmt.Errorf("no credentials for %s", region)
		}
		return regionalNet{f: f, region: region}, nil
	}
	if err := setupNetworkResources(context.Background(), state, def); err == nil {
		t.Fatal("setup succeeded although one region was unreachable")
	}
	if state.SecurityGroups["eu-west-1"] == "" {
		t.Errorf("the eu-west-1 group created before the failure is not recorded, so nothing will delete it: %v", state.SecurityGroups)
	}
}

// ---- #930: every instance must complete -----------------------------------

func fourInstanceStage() (*pipeline.PipelineState, *pipeline.Pipeline) {
	state, def := twoStagePipeline("stop", time.Now())
	def.Stages[0].InstanceCount = 4
	st := getStageState(state, "train")
	st.Instances = nil
	for i := 0; i < 4; i++ {
		st.Instances = append(st.Instances, pipeline.InstanceInfo{InstanceID: fmt.Sprintf("i-%d", i), State: "pending"})
	}
	return state, def
}

func markComplete(h *harness, idx ...int) {
	for _, i := range idx {
		h.markers.present[pipeline.StageInstanceMarkerKey("pfx", "train", i, pipeline.MarkerComplete)] = true
	}
}

// #930: with instance_count 4 the stage is complete only when all four have
// written COMPLETE, and its dependent does not launch before. The first
// instance to finish used to mark the whole stage complete.
func TestStageCompletesOnlyWhenEveryInstanceHas(t *testing.T) {
	state, def := fourInstanceStage()
	h := newHarness(t, state)
	for i := 0; i < 4; i++ {
		h.ec2.states[fmt.Sprintf("i-%d", i)] = "running"
	}
	markComplete(h, 0, 1, 2)

	if _, err := pollOnce(context.Background(), state, def, mustOrder(t, def), time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := getStageState(state, "train").Status; got != pipeline.StageStatusRunning {
		t.Fatalf("train = %s with 3 of 4 instances complete, want running", got)
	}
	if got := getStageState(state, "eval").Status; got != pipeline.StageStatusPending {
		t.Fatalf("eval = %s while its dependency still has an instance running, want pending", got)
	}

	markComplete(h, 3)
	if _, err := pollOnce(context.Background(), state, def, mustOrder(t, def), time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := getStageState(state, "train").Status; got != pipeline.StageStatusCompleted {
		t.Errorf("train = %s with all 4 instances complete, want completed", got)
	}
}

// Any one instance's FAILED fails the stage, even when the others completed.
func TestOneInstanceFailedFailsTheStage(t *testing.T) {
	state, def := fourInstanceStage()
	h := newHarness(t, state)
	for i := 0; i < 4; i++ {
		h.ec2.states[fmt.Sprintf("i-%d", i)] = "running"
	}
	markComplete(h, 0, 1, 3)
	h.markers.present[pipeline.StageInstanceMarkerKey("pfx", "train", 2, pipeline.MarkerFailed)] = true

	outcome, reason, err := checkStageOutcome(context.Background(), state, def.GetStage("train"), getStageState(state, "train"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if outcome != stageFailed || !strings.Contains(reason, "instance 2") {
		t.Errorf("outcome = %v (%q), want failed naming instance 2", outcome, reason)
	}
}

// One instance gone without COMPLETE (a spot interruption) fails the stage: it
// can never complete. The old rule waited for all instances to be gone.
func TestOneInstanceGoneWithoutCompletingFailsTheStage(t *testing.T) {
	state, def := fourInstanceStage()
	h := newHarness(t, state)
	for i := 0; i < 4; i++ {
		h.ec2.states[fmt.Sprintf("i-%d", i)] = "running"
	}
	h.ec2.states["i-1"] = "terminated"
	markComplete(h, 0)

	outcome, _, err := checkStageOutcome(context.Background(), state, def.GetStage("train"), getStageState(state, "train"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if outcome != stageFailed {
		t.Errorf("outcome = %v with instance 1 terminated and no COMPLETE from it, want failed", outcome)
	}

	// A completed instance that has since terminated itself is not a failure.
	h.ec2.states["i-1"] = "running"
	h.ec2.states["i-0"] = "terminated"
	outcome, _, _ = checkStageOutcome(context.Background(), state, def.GetStage("train"), getStageState(state, "train"), time.Now())
	if outcome != stageStillRunning {
		t.Errorf("outcome = %v after a COMPLETE instance terminated itself, want still running", outcome)
	}
}

// Instances launched before #930 write the stage-level key. Their FAILED still
// fails the stage; their COMPLETE completes only a single-instance stage, where
// it means every instance.
func TestLegacyStageLevelMarkers(t *testing.T) {
	state, def := fourInstanceStage()
	h := newHarness(t, state)
	for i := 0; i < 4; i++ {
		h.ec2.states[fmt.Sprintf("i-%d", i)] = "running"
	}
	h.markers.present[pipeline.StageMarkerKey("pfx", "train", pipeline.MarkerComplete)] = true
	if outcome, _, _ := checkStageOutcome(context.Background(), state, def.GetStage("train"), getStageState(state, "train"), time.Now()); outcome != stageStillRunning {
		t.Errorf("a legacy stage-level COMPLETE completed a 4-instance stage (outcome %v): that is #930", outcome)
	}

	single, sdef := twoStagePipeline("stop", time.Now())
	hs := newHarness(t, single)
	hs.ec2.states["i-train"] = "running"
	hs.markers.present[pipeline.StageMarkerKey("pfx", "train", pipeline.MarkerComplete)] = true
	if outcome, _, _ := checkStageOutcome(context.Background(), single, sdef.GetStage("train"), getStageState(single, "train"), time.Now()); outcome != stageCompleted {
		t.Errorf("a legacy stage-level COMPLETE did not complete a single-instance stage (outcome %v)", outcome)
	}
	hs.markers.present[pipeline.StageMarkerKey("pfx", "train", pipeline.MarkerFailed)] = true
	if outcome, _, _ := checkStageOutcome(context.Background(), single, sdef.GetStage("train"), getStageState(single, "train"), time.Now()); outcome != stageFailed {
		t.Errorf("a legacy stage-level FAILED was ignored (outcome %v)", outcome)
	}
}

// Each instance's user data writes its fallback FAILED at its own key.
func TestUserDataFallbackMarkerIsPerInstance(t *testing.T) {
	state, def := fourInstanceStage()
	keys := map[string]bool{}
	for i := 0; i < 4; i++ {
		ud, err := generatePipelineUserData(state, def.GetStage("train"), getStageState(state, "train"), i, "eu-west-1")
		if err != nil {
			t.Fatal(err)
		}
		raw := decodeUD(t, ud)
		want := "SPAWN_FAILED_MARKER='s3://bkt/" + pipeline.StageInstanceMarkerKey("pfx", "train", i, pipeline.MarkerFailed) + "'"
		if !strings.Contains(raw, "\n"+want+"\n") {
			t.Errorf("instance %d: user data does not assign %s", i, want)
		}
		keys[want] = true
	}
	if len(keys) != 4 {
		t.Errorf("4 instances share %d fallback marker keys", len(keys))
	}
}

// ---- #930: the budget is priced, and counts running stages ----------------

// #930: four p4d.24xlarge (~$32.77/h on-demand) running for an hour have cost
// ~$131, past a $100 budget, although no stage has finished. The flat
// $0.10/instance-hour, counted only at completion, saw $0.
func TestBudgetIsEnforcedAtRealRatesWhileStagesRun(t *testing.T) {
	state, def := fourInstanceStage()
	def.Stages[0].InstanceType = "p4d.24xlarge"
	def.Stages[0].Timeout = "12h"
	h := newHarness(t, state)
	h.prices["p4d.24xlarge"] = 32.7726
	for i := 0; i < 4; i++ {
		h.ec2.states[fmt.Sprintf("i-%d", i)] = "running"
	}
	launched := time.Now().Add(-time.Hour)
	getStageState(state, "train").LaunchedAt = &launched
	budget := 100.0
	state.MaxCostUSD = &budget

	done, err := pollOnce(context.Background(), state, def, mustOrder(t, def), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !done || state.Status != pipeline.StatusCancelled {
		t.Fatalf("done=%v status=%s: four p4d.24xlarge for an hour (~$131) did not trip a $100 budget", done, state.Status)
	}
	for i := 0; i < 4; i++ {
		if !h.ec2.terminatedIn("eu-west-1", fmt.Sprintf("i-%d", i)) {
			t.Errorf("i-%d not terminated after the budget was exceeded", i)
		}
	}
	if st := getStageState(state, "train"); st.StageCostUSD < 100 || st.PriceSource != "test" {
		t.Errorf("stage cost = $%.2f (source %q), want the real-rate estimate, over $100", st.StageCostUSD, st.PriceSource)
	}
}

// Under budget, a running stage is not cancelled.
func TestBudgetNotTrippedUnderIt(t *testing.T) {
	state, def := fourInstanceStage()
	h := newHarness(t, state)
	h.prices["t3.micro"] = 0.0104
	for i := 0; i < 4; i++ {
		h.ec2.states[fmt.Sprintf("i-%d", i)] = "running"
	}
	budget := 100.0
	state.MaxCostUSD = &budget
	done, _ := pollOnce(context.Background(), state, def, mustOrder(t, def), time.Now())
	if done {
		t.Errorf("pipeline finished (%s) well under its budget", state.Status)
	}
}

// A budget that cannot be priced cannot be enforced: the stage launches
// nothing and fails. Without a budget the stage launches unpriced.
func TestUnpriceableStageWithABudgetLaunchesNothing(t *testing.T) {
	state, def := twoStagePipeline("stop", time.Now())
	h := newHarness(t, state)
	h.prices["t3.micro"] = -1
	def.Stages[0].AMI = "ami-given"
	var events []string
	launcher := &fakeLauncher{events: &events}
	oldPut, oldLaunch := putObjectFn, launchEC2ForRegion
	t.Cleanup(func() { putObjectFn, launchEC2ForRegion = oldPut, oldLaunch })
	putObjectFn = func(context.Context, *s3.PutObjectInput) error { return nil }
	launchEC2ForRegion = func(context.Context, string) (runInstancesAPI, error) { return launcher, nil }

	budget := 10.0
	state.MaxCostUSD = &budget
	err := launchStage(context.Background(), state, def, def.GetStage("train"), &pipeline.StageState{StageID: "train"})
	if err == nil || len(launcher.inputs) != 0 {
		t.Errorf("unpriceable stage with a budget: err=%v, launches=%d; want an error and no launch", err, len(launcher.inputs))
	}

	state.MaxCostUSD = nil
	if err := launchStage(context.Background(), state, def, def.GetStage("train"), &pipeline.StageState{StageID: "train"}); err != nil {
		t.Errorf("unpriceable stage without a budget refused: %v", err)
	}
	if len(launcher.inputs) != 1 {
		t.Errorf("launches = %d without a budget, want 1", len(launcher.inputs))
	}
}

// Every cost the orchestrator reports to the user is labelled an estimate.
func TestReportedCostsAreLabelledEstimates(t *testing.T) {
	state, def := twoStagePipeline("stop", time.Now())
	h := newHarness(t, state)
	var msgs []string
	notifyFn = func(_ context.Context, _ *pipeline.PipelineState, subject, msg string) error {
		msgs = append(msgs, msg)
		return nil
	}
	h.ec2.states["i-train"] = "running"
	markComplete(h, 0)
	getStageState(state, "eval").Status = pipeline.StageStatusCompleted
	if _, err := pollOnce(context.Background(), state, def, mustOrder(t, def), time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(msgs) == 0 {
		t.Fatal("no completion notification")
	}
	for _, m := range msgs {
		if strings.Contains(m, "Total cost") || !strings.Contains(m, "Estimated cost") || !strings.Contains(m, costEstimateNote) {
			t.Errorf("notification does not present the cost as an estimate:\n%s", m)
		}
	}
}

func decodeUD(t *testing.T, ud string) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(ud)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
