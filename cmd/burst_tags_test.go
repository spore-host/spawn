package cmd

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spore-host/spawn/pkg/tagprefix"
	"github.com/spore-host/spawn/pkg/testutil"
)

// spored registers an EC2 job-array member with the hybrid registry only when it
// carries the burst tag (#904), so every instance `spawn burst` launches must
// carry exactly the key spored reads — tagprefix.Tag("burst") — set to "true",
// alongside the job-array id. Launched against the Substrate emulator.
func TestLaunchBurstInstances_TagsMembersAsBurst(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	client := ec2.NewFromConfig(env.AWSConfig)

	ids, err := launchBurstInstances(ctx, client, &LaunchParams{
		Count:        2,
		InstanceType: "t3.micro",
		AMI:          testutil.RegisterTestAMI(t, client),
		JobArrayID:   "ja-burst",
	})
	if err != nil {
		t.Fatalf("launchBurstInstances: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("launched %d instances, want 2", len(ids))
	}
	for _, id := range ids {
		out, err := client.DescribeTags(ctx, &ec2.DescribeTagsInput{
			Filters: []ec2types.Filter{{Name: aws.String("resource-id"), Values: []string{id}}},
		})
		if err != nil {
			t.Fatalf("DescribeTags(%s): %v", id, err)
		}
		got := map[string]string{}
		for _, tag := range out.Tags {
			got[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
		}
		if got[tagprefix.Tag("burst")] != "true" {
			t.Errorf("%s: %s = %q, want \"true\" (spored would not register it with the hybrid registry)",
				id, tagprefix.Tag("burst"), got[tagprefix.Tag("burst")])
		}
		if got[tagprefix.Tag("job-array-id")] != "ja-burst" {
			t.Errorf("%s: %s = %q, want ja-burst", id, tagprefix.Tag("job-array-id"), got[tagprefix.Tag("job-array-id")])
		}
	}
}
