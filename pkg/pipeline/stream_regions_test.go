package pipeline

import (
	"strings"
	"testing"
)

func streamPair(producerRegion, consumerRegion, mode string) *Pipeline {
	out := &DataConfig{Mode: "stream", Protocol: "tcp", Port: 50001}
	in := &DataConfig{Mode: "stream", Protocol: "tcp", SourceStage: "producer"}
	if mode == "s3" {
		out = &DataConfig{Mode: "s3", Paths: []string{"/out/*"}}
		in = &DataConfig{Mode: "s3", SourceStage: "producer"}
	}
	return &Pipeline{
		PipelineID: "p", PipelineName: "p", S3Bucket: "b",
		Stages: []Stage{
			{StageID: "producer", InstanceType: "c5.large", InstanceCount: 1, Region: producerRegion, Command: "true", DataOutput: out},
			{StageID: "consumer", InstanceType: "c5.large", InstanceCount: 1, Region: consumerRegion, Command: "true",
				DependsOn: []string{"producer"}, DataInput: in},
		},
	}
}

// #956: a stream between stages in different regions can never connect: each
// region's stages are in their own default VPC and pipeline security group,
// peer discovery publishes private IPs, and nothing peers the VPCs. Validate
// must say so rather than let the pipeline launch and hang.
func TestValidateRejectsCrossRegionStreaming(t *testing.T) {
	err := streamPair("eu-west-1", "us-west-2", "stream").Validate()
	if err == nil {
		t.Fatal("a cross-region stream validated")
	}
	for _, want := range []string{"consumer", "producer", "eu-west-1", "us-west-2", `"s3"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
	// A stage with no region runs in the default region.
	if err := streamPair("", "eu-west-1", "stream").Validate(); err == nil {
		t.Error("a stream from a region-less (us-east-1) stage to eu-west-1 validated")
	}
}

func TestValidateAllowsSameRegionStreamingAndCrossRegionS3(t *testing.T) {
	for name, p := range map[string]*Pipeline{
		"same region":           streamPair("us-west-2", "us-west-2", "stream"),
		"default region on one": streamPair("", DefaultStageRegion, "stream"),
		"s3 across regions":     streamPair("eu-west-1", "us-west-2", "s3"),
	} {
		if err := p.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
