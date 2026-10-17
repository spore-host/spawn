package pipeline

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// The S3 layout a pipeline's stages share with the orchestrator. Both sides
// build every key through these functions: the stage runner (spored
// run-pipeline-stage) writes the markers and outputs, the orchestrator Lambda
// reads the markers and writes the stage config, and the stage user data's
// fallback FAILED marker is rendered from them in Go rather than rebuilt in
// bash. Two hand-written copies of this layout is how a stage can report into
// a key nobody reads (#910).

// RunKeyPrefix is the key prefix under which one RUN of a pipeline keeps its
// stage markers, outputs and stage config: "<prefix>/runs/<run-id>". Every
// other key function takes this, not the bare s3_prefix, as its prefix.
//
// The layout used to be keyed by s3_prefix alone, so a second run under the
// same prefix found the first run's COMPLETE markers and marked its stages
// complete before they had done anything, and its dependents downloaded the
// first run's outputs; a stale FAILED failed a fresh run (#943). A record
// written before run ids existed has runID "", and keeps the old layout, so a
// pipeline in flight across the upgrade still finds its markers.
func RunKeyPrefix(prefix, runID string) string {
	if runID == "" {
		return prefix
	}
	return prefix + "/runs/" + runID
}

// DataPrefix is the key prefix of this run's stage data: RunKeyPrefix of the
// pipeline's s3_prefix and run id.
func (s *PipelineState) DataPrefix() string {
	return RunKeyPrefix(s.S3Prefix, s.RunID)
}

// NewRunID returns a fresh run id: the UTC launch time, so runs sort by when
// they started, and 32 random bits, so two launches in the same second still
// differ. Lower case and [0-9a-z-] only, because it ends up in S3 keys, EC2
// tag values and security and placement group names.
func NewRunID(now time.Time) (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate run id: %w", err)
	}
	return now.UTC().Format("20060102t150405z") + "-" + hex.EncodeToString(b[:]), nil
}

// Stage outcome markers.
const (
	MarkerComplete = "COMPLETE"
	MarkerFailed   = "FAILED"
)

// StageKeyPrefix is the S3 key prefix under which one stage's markers and
// outputs live. The format is "<prefix>/stages/<stage-id>/", with prefix
// joined verbatim, as it always has been, so an empty s3_prefix still yields
// the keys existing pipelines use. prefix is the run's prefix (RunKeyPrefix),
// not the bare s3_prefix.
func StageKeyPrefix(prefix, stageID string) string {
	return fmt.Sprintf("%s/stages/%s/", prefix, stageID)
}

// StageOutputPrefix is where a stage's data_output files are uploaded and
// where a downstream stage's data_input reads them from.
func StageOutputPrefix(prefix, stageID string) string {
	return StageKeyPrefix(prefix, stageID) + "output/"
}

// StageMarkerKey is the key of a stage-level COMPLETE or FAILED marker. Stage
// instances no longer write it: with instance_count > 1 the first instance to
// finish marked the whole stage complete (#930). The orchestrator still reads
// it, for instances launched before per-instance markers existed.
func StageMarkerKey(prefix, stageID, marker string) string {
	return StageKeyPrefix(prefix, stageID) + marker
}

// StageInstanceMarkerKey is the key of one stage instance's COMPLETE or FAILED
// marker: "<prefix>/stages/<stage-id>/<instance-index>/<marker>". A stage is
// complete when every instance has written COMPLETE, and failed as soon as any
// one writes FAILED (#930).
func StageInstanceMarkerKey(prefix, stageID string, instanceIndex int, marker string) string {
	return fmt.Sprintf("%s%d/%s", StageKeyPrefix(prefix, stageID), instanceIndex, marker)
}

// StageConfigKey is where the orchestrator writes the pipeline definition for
// the stage runner to read, inside the pipeline's own bucket and prefix so the
// stage instance profile's S3 grant (StageDataPolicy) covers it. The copy the
// CLI uploads lives in the spawn-pipelines-<region> bucket, which holds every
// pipeline's definition and so is not granted to stage instances.
func StageConfigKey(prefix, pipelineID string) string {
	return fmt.Sprintf("%s/config/%s.json", prefix, pipelineID)
}

// StageDataPolicy is the IAM policy a pipeline's stage instances need for the
// data handoff: read the stage config, read upstream outputs, write their own
// outputs and markers, all under s3://bucket/prefix/, and list that prefix to
// find upstream outputs. Nothing outside the prefix is granted.
//
// Every key above is "<prefix>/..." or "<prefix>/runs/<run-id>/...", so
// "<prefix>/*" covers them, including for an empty prefix (keys begin
// "/runs/" or "/stages/").
func StageDataPolicy(bucket, prefix string) string {
	doc := map[string]interface{}{
		"Version": "2012-10-17",
		"Statement": []interface{}{
			map[string]interface{}{
				"Effect":   "Allow",
				"Action":   []string{"s3:GetObject", "s3:PutObject"},
				"Resource": fmt.Sprintf("arn:aws:s3:::%s/%s/*", bucket, prefix),
			},
			map[string]interface{}{
				"Effect":   "Allow",
				"Action":   []string{"s3:ListBucket"},
				"Resource": fmt.Sprintf("arn:aws:s3:::%s", bucket),
				"Condition": map[string]interface{}{
					"StringLike": map[string]interface{}{
						"s3:prefix": prefix + "/*",
					},
				},
			},
		},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return "" // static shape; never expected
	}
	return string(b)
}
