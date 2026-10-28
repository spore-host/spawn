package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/pipeline"
)

// TestNestedSpawnStageProfileIsNeverLaunched is #1372's pipeline gate: a
// pipeline record whose instance_profile is a nested-spawn role
// (spawn-instance-nested-*) launches no stage instance, counted at the
// RunInstances seam. The control is the same stage with the spawn-instance-*
// profile `spawn pipeline launch` records, which does launch.
func TestNestedSpawnStageProfileIsNeverLaunched(t *testing.T) {
	oldPut, oldLaunch, oldAMI, oldPrice := putObjectFn, launchEC2ForRegion, recommendedAMIFn, priceFn
	t.Cleanup(func() {
		putObjectFn, launchEC2ForRegion, recommendedAMIFn, priceFn = oldPut, oldLaunch, oldAMI, oldPrice
	})
	priceFn = func(context.Context, string, string) (float64, string, error) { return 1, "test", nil }
	putObjectFn = func(context.Context, *s3.PutObjectInput) error { return nil }
	recommendedAMIFn = func(context.Context, string, string) (string, error) { return "ami-looked-up", nil }

	for _, tc := range []struct {
		profile string
		refuse  bool
	}{
		{spawnaws.NestedSpawnRoleName(), true},
		{spawnaws.NestedSpawnRolePrefix + "0123abcd", true},
		{"spawn-instance-abc12345", false},
	} {
		var events []string
		launcher := &fakeLauncher{events: &events}
		launchEC2ForRegion = func(context.Context, string) (runInstancesAPI, error) { return launcher, nil }

		state, def := twoStagePipeline("stop", time.Now())
		state.InstanceProfile = tc.profile
		def.Stages[0].AMI = "ami-given"
		stageState := &pipeline.StageState{StageID: "train", StageIndex: 0, Status: pipeline.StageStatusReady}
		err := launchStage(context.Background(), state, def, def.GetStage("train"), stageState)
		if !tc.refuse {
			if err != nil || len(launcher.inputs) == 0 {
				t.Errorf("%s: err = %v, %d launches: the control did not launch, so the gate is blind", tc.profile, err, len(launcher.inputs))
			}
			continue
		}
		if len(launcher.inputs) != 0 {
			t.Errorf("%s: %d RunInstances call(s) for a nested-spawn stage profile, want 0", tc.profile, len(launcher.inputs))
		}
		if err == nil || !strings.Contains(err.Error(), "nested spawn isn't available for pipeline stages") ||
			!strings.Contains(err.Error(), "--ttl") {
			t.Errorf("%s: err = %v, want the nested-spawn refusal naming --nested-spawn's --ttl", tc.profile, err)
		}
	}
}
