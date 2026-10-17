package pipeline

import (
	"encoding/json"
	"fmt"
)

// The S3 layout a pipeline's stages share with the orchestrator. Both sides
// build every key through these functions: the stage runner (spored
// run-pipeline-stage) writes the markers and outputs, the orchestrator Lambda
// reads the markers and writes the stage config, and the stage user data's
// fallback FAILED marker is rendered from them in Go rather than rebuilt in
// bash. Two hand-written copies of this layout is how a stage can report into
// a key nobody reads (#910).

// Stage outcome markers.
const (
	MarkerComplete = "COMPLETE"
	MarkerFailed   = "FAILED"
)

// StageKeyPrefix is the S3 key prefix under which one stage's markers and
// outputs live. The format is "<prefix>/stages/<stage-id>/", with prefix
// joined verbatim, as it always has been, so an empty s3_prefix still yields
// the keys existing pipelines use.
func StageKeyPrefix(prefix, stageID string) string {
	return fmt.Sprintf("%s/stages/%s/", prefix, stageID)
}

// StageOutputPrefix is where a stage's data_output files are uploaded and
// where a downstream stage's data_input reads them from.
func StageOutputPrefix(prefix, stageID string) string {
	return StageKeyPrefix(prefix, stageID) + "output/"
}

// StageMarkerKey is the key of a stage's COMPLETE or FAILED marker.
func StageMarkerKey(prefix, stageID, marker string) string {
	return StageKeyPrefix(prefix, stageID) + marker
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
// Every key above is "<prefix>/...", so "<prefix>/*" covers them, including
// for an empty prefix (keys begin "/stages/").
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
