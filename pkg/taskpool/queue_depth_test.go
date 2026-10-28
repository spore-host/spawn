package taskpool

import (
	"context"
	"strconv"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// attrSQS answers GetQueueAttributes with fixed attributes.
type attrSQS struct {
	errSQS
	attrs map[string]string
}

func (a attrSQS) GetQueueAttributes(context.Context, *sqs.GetQueueAttributesInput, ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error) {
	return &sqs.GetQueueAttributesOutput{Attributes: a.attrs}, nil
}

// TestDepthUnknownIsNotZero: a count SQS omits, or returns as something other
// than a count, is reported as unknown (nil), never as 0 (#1362); a count it
// does return is parsed.
func TestDepthUnknownIsNotZero(t *testing.T) {
	const vis, inf = "ApproximateNumberOfMessages", "ApproximateNumberOfMessagesNotVisible"
	cases := []struct {
		name         string
		attrs        map[string]string
		wantVisible  *int
		wantInFlight *int
	}{
		{"both present", map[string]string{vis: "7", inf: "3"}, countOf(7), countOf(3)},
		{"present zeros", map[string]string{vis: "0", inf: "0"}, countOf(0), countOf(0)},
		{"both missing", map[string]string{}, nil, nil},
		{"in-flight missing", map[string]string{vis: "4"}, countOf(4), nil},
		{"visible unparseable", map[string]string{vis: "lots", inf: "2"}, nil, countOf(2)},
		{"empty string", map[string]string{vis: "", inf: "1"}, nil, countOf(1)},
		{"negative", map[string]string{vis: "1", inf: "-1"}, countOf(1), nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := &Queue{client: attrSQS{attrs: c.attrs}, name: "q"}
			v, f, err := q.Depth(context.Background())
			if err != nil {
				t.Fatalf("Depth: %v", err)
			}
			if !sameCount(v, c.wantVisible) || !sameCount(f, c.wantInFlight) {
				t.Errorf("Depth = (%s, %s), want (%s, %s)", showCount(v), showCount(f), showCount(c.wantVisible), showCount(c.wantInFlight))
			}
		})
	}
}

func countOf(n int) *int { return &n }

func sameCount(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func showCount(n *int) string {
	if n == nil {
		return "unknown"
	}
	return strconv.Itoa(*n)
}
