package cmd

import (
	"testing"

	"github.com/spore-host/spawn/pkg/pipeline"
)

// `spawn pipeline launch` must give the stages a profile carrying the
// pipeline's S3 grant: the shared default spored profile has no access to
// pipeline data, so stages could not read inputs or write outputs or markers
// (#911).
func TestPipelineStageIAMConfigGrantsThePipelineData(t *testing.T) {
	p := &pipeline.Pipeline{S3Bucket: "my-data", S3Prefix: "pipelines/ml"}
	cfg := pipelineStageIAMConfig(p)
	if cfg.InlinePolicyJSON != pipeline.StageDataPolicy("my-data", "pipelines/ml") {
		t.Errorf("stage role policy = %q, want the pipeline's StageDataPolicy", cfg.InlinePolicyJSON)
	}
	if len(cfg.TrustServices) != 1 || cfg.TrustServices[0] != "ec2" {
		t.Errorf("trust services = %v, want [ec2]: an instance profile is assumed by EC2", cfg.TrustServices)
	}
	if cfg.RoleName != "" {
		t.Errorf("RoleName = %q; leave it empty so the role is named by policy hash and shared by runs of the same pipeline", cfg.RoleName)
	}
}
