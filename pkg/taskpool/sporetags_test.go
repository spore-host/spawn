package taskpool

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	libtags "github.com/spore-host/libs/tags"
	"github.com/spore-host/libs/tags/tagtest"

	"github.com/spore-host/spawn/internal/sdkanswer"
)

// A pool's queue carries spore-host:managed-by=spawn and its region as
// spore-host:scope (#1652), checked by the tagtest Detector on a real client.
func TestSporeTags_PoolQueue(t *testing.T) {
	var got *sqs.CreateQueueInput
	cfg := sdkanswer.Config("eu-west-1", func(service, op string, in any) (any, error) {
		if v, ok := in.(*sqs.CreateQueueInput); ok {
			got = v
			return &sqs.CreateQueueOutput{QueueUrl: aws.String("https://sqs/q")}, nil
		}
		return nil, nil
	})
	d := tagtest.New(t, libtags.ToolSpawn)
	d.Install(&cfg)
	if _, err := CreateQueue(context.Background(), sqs.NewFromConfig(cfg), "run-1", 30); err != nil {
		t.Fatal(err)
	}
	if len(d.Calls()) != 1 || d.Calls()[0].Operation != "CreateQueue" {
		t.Fatalf("the Detector saw %+v, want one CreateQueue", d.Calls())
	}
	if got.Tags[libtags.Scope] != "eu-west-1" {
		t.Errorf("queue tags = %v, want scope eu-west-1", got.Tags)
	}
}
