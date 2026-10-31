package main

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/spore-host/spawn/internal/s3hosts"
)

// #1436: every S3 name a stage instance's user data reaches is the launch
// region's regional one (or the us-east-1 fallback bucket by its own
// regional name), which an S3 gateway endpoint or an interface endpoint with
// private DNS serves. Rendered as it ships, for a region other than us-east-1.
func TestStageUserDataS3HostsAreRegional(t *testing.T) {
	const region = "eu-west-2"
	state, def := twoStagePipeline("stop", time.Now())
	ud, err := generatePipelineUserData(state, def.GetStage("train"), getStageState(state, "train"), 0, region)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(ud)
	if err != nil {
		t.Fatal(err)
	}
	refs, problems := s3hosts.Check(string(raw), region)
	// The regional bucket, the fallback and the FAILED marker's aws s3 cp.
	if len(refs) < 3 {
		t.Errorf("found %d S3 locations, want at least 3; discovery has broken: %v", len(refs), refs)
	}
	for _, p := range problems {
		t.Error(p)
	}
}
