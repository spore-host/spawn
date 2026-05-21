package pipeline

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spore-host/spawn/pkg/testutil"
)

func TestQueryPipelineInstances_TagBasedPeerList(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()

	pipelineID := "pipe-peer-001"
	stageID := "stage-preprocess"

	ec2Client := env.EC2Client()

	// Launch two instances in the same pipeline stage.
	_, err := ec2Client.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-12345678"),
		InstanceType: ec2types.InstanceTypeT3Micro,
		MinCount:     aws.Int32(2),
		MaxCount:     aws.Int32(2),
		TagSpecifications: []ec2types.TagSpecification{
			{
				ResourceType: ec2types.ResourceTypeInstance,
				Tags: []ec2types.Tag{
					{Key: aws.String("spawn:pipeline-id"), Value: aws.String(pipelineID)},
					{Key: aws.String("spawn:stage-id"), Value: aws.String(stageID)},
					{Key: aws.String("spawn:stage-index"), Value: aws.String("0")},
					{Key: aws.String("spawn:instance-index"), Value: aws.String("0")},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("RunInstances: %v", err)
	}

	peers, err := queryPipelineInstances(ctx, ec2Client, pipelineID)
	if err != nil {
		t.Fatalf("queryPipelineInstances() error = %v", err)
	}
	if len(peers) == 0 {
		t.Fatal("queryPipelineInstances() returned 0 peers, want >= 1")
	}
	for _, p := range peers {
		if p.StageID != stageID {
			t.Errorf("peer.StageID = %q, want %q", p.StageID, stageID)
		}
		if p.InstanceID == "" {
			t.Error("peer.InstanceID is empty")
		}
	}
}

func TestQueryPipelineInstances_MultiStage(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()

	pipelineID := "pipe-multistage-001"
	ec2Client := env.EC2Client()

	stages := []struct {
		id    string
		count int32
	}{
		{"stage-ingest", 1},
		{"stage-process", 2},
	}

	for _, s := range stages {
		_, err := ec2Client.RunInstances(ctx, &ec2.RunInstancesInput{
			ImageId:      aws.String("ami-12345678"),
			InstanceType: ec2types.InstanceTypeT3Micro,
			MinCount:     aws.Int32(s.count),
			MaxCount:     aws.Int32(s.count),
			TagSpecifications: []ec2types.TagSpecification{
				{
					ResourceType: ec2types.ResourceTypeInstance,
					Tags: []ec2types.Tag{
						{Key: aws.String("spawn:pipeline-id"), Value: aws.String(pipelineID)},
						{Key: aws.String("spawn:stage-id"), Value: aws.String(s.id)},
					},
				},
			},
		})
		if err != nil {
			t.Fatalf("RunInstances for stage %s: %v", s.id, err)
		}
	}

	peers, err := queryPipelineInstances(ctx, ec2Client, pipelineID)
	if err != nil {
		t.Fatalf("queryPipelineInstances() error = %v", err)
	}

	total := int32(0)
	for _, s := range stages {
		total += s.count
	}
	if int32(len(peers)) != total {
		t.Errorf("got %d peers, want %d", len(peers), total)
	}
}

func TestQueryPipelineInstances_EmptyPipeline(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()

	peers, err := queryPipelineInstances(ctx, env.EC2Client(), "nonexistent-pipeline")
	if err != nil {
		t.Fatalf("queryPipelineInstances() error = %v", err)
	}
	if len(peers) != 0 {
		t.Errorf("got %d peers for nonexistent pipeline, want 0", len(peers))
	}
}
