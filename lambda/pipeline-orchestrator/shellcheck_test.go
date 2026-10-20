package main

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/spore-host/spawn/internal/shellcheckgate"
)

// shellcheck on the pipeline stage user-data (#689 part C), for the first
// instance and a later one (the index is rendered into marker keys).
func TestPipelineUserData_Shellcheck(t *testing.T) {
	state, def := twoStagePipeline("stop", time.Now())
	for _, idx := range []int{0, 3} {
		ud, err := generatePipelineUserData(state, def.GetStage("train"), getStageState(state, "train"), idx, "eu-west-1")
		if err != nil {
			t.Fatal(err)
		}
		raw, err := base64.StdEncoding.DecodeString(ud)
		if err != nil {
			t.Fatalf("user data is not base64: %v", err)
		}
		shellcheckgate.Check(t, "lambda/pipeline-orchestrator.generatePipelineUserData", string(raw))
	}
}
