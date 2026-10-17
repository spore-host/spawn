package cmd

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/pipeline"
)

// #956: every efa_enabled stage's type is checked for EFA in the stage's own
// region before the launch creates anything; other stages are not checked.
func TestLaunchChecksEFAStagesBeforeCreatingAnything(t *testing.T) {
	p := &pipeline.Pipeline{Stages: []pipeline.Stage{
		{StageID: "prep", InstanceType: "t3.micro"},
		{StageID: "train", InstanceType: "t3.micro", EFAEnabled: true},
		{StageID: "mpi", InstanceType: "c5n.18xlarge", Region: "eu-west-1", EFAEnabled: true},
	}}
	var checked []string
	check := func(_ context.Context, instanceType, region string) error {
		checked = append(checked, instanceType+"@"+region)
		if instanceType == "t3.micro" {
			return errors.New("instance type t3.micro does not support EFA")
		}
		return nil
	}
	err := checkEFAStages(context.Background(), p, check)
	if err == nil || !strings.Contains(err.Error(), "train") || !strings.Contains(err.Error(), "does not support EFA") {
		t.Errorf("err = %v, want the train stage refused for lacking EFA", err)
	}
	if strings.Join(checked, ",") != "t3.micro@us-east-1" {
		t.Errorf("checked %v, want only the EFA stage train, in the default region", checked)
	}
	p.Stages[1].InstanceType = "p4d.24xlarge"
	checked = nil
	if err := checkEFAStages(context.Background(), p, check); err != nil {
		t.Fatal(err)
	}
	if strings.Join(checked, ",") != "p4d.24xlarge@us-east-1,c5n.18xlarge@eu-west-1" {
		t.Errorf("checked %v", checked)
	}
}

// The check runs in `spawn pipeline launch`, before the first thing it creates
// (the stage instance profile). Matched on the invocations, in order.
func TestLaunchRunsTheEFACheckFirst(t *testing.T) {
	b, err := os.ReadFile("pipeline.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	start := strings.Index(src, "func runLaunchPipeline(")
	if start < 0 {
		t.Fatal("runLaunchPipeline not found; this matcher is stale")
	}
	body := src[start:]
	check := strings.Index(body, "checkEFAStages(ctx, p, awsClient.ValidateInstanceTypeForEFAInRegion)")
	create := strings.Index(body, "awsClient.CreateOrGetInstanceProfile(ctx,")
	if create < 0 {
		t.Fatal("the instance profile creation was not found; this matcher is stale")
	}
	if check < 0 || check > create {
		t.Errorf("runLaunchPipeline does not check EFA stages before creating the instance profile (check at %d, create at %d)", check, create)
	}
}
