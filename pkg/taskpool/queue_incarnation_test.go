package taskpool

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// createdSQS answers GetQueueAttributes from attrs, but only with the
// attributes the call asked for, as SQS does.
type createdSQS struct {
	errSQS
	attrs map[string]string
}

func (c createdSQS) GetQueueAttributes(_ context.Context, in *sqs.GetQueueAttributesInput, _ ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error) {
	out := map[string]string{}
	for _, n := range in.AttributeNames {
		if v, ok := c.attrs[string(n)]; ok {
			out[string(n)] = v
		}
	}
	return &sqs.GetQueueAttributesOutput{Attributes: out}, nil
}

// Incarnation is the queue's CreatedTimestamp, which changes when the queue is
// deleted and created again and nowhere else (#1448). Without it there is no
// incarnation to give, and saying so beats inventing one.
func TestIncarnationIsTheQueueCreationTime(t *testing.T) {
	created := string(types.QueueAttributeNameCreatedTimestamp)
	q := &Queue{client: createdSQS{attrs: map[string]string{created: "1761853661", "VisibilityTimeout": "900"}}, name: "q"}
	got, err := q.Incarnation(context.Background())
	if err != nil || got != "1761853661" {
		t.Fatalf("Incarnation = %q, %v; want the queue's CreatedTimestamp", got, err)
	}
	q = &Queue{client: createdSQS{attrs: map[string]string{"VisibilityTimeout": "900"}}, name: "q"}
	if got, err := q.Incarnation(context.Background()); err == nil {
		t.Fatalf("Incarnation with no CreatedTimestamp = %q, want an error", got)
	}
}
