package main

import (
	"testing"
	"time"

	libtags "github.com/spore-host/libs/tags"
)

// A pipeline's notification topic is created on every notification, and SNS
// refuses a CreateTopic whose tags differ from the existing topic's. So its
// spore.host tags (#1652) must be the same on every call for one pipeline:
// spore-host:created is the pipeline's CreatedAt, not the time of the call.
func TestPipelineTopicTagsAreStablePerPipeline(t *testing.T) {
	created := time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC)
	a := pipelineTopicTags("us-east-1", created)
	b := pipelineTopicTags("us-east-1", created)
	if len(a) != len(b) {
		t.Fatalf("two calls gave %v and %v", a, b)
	}
	for k, v := range a {
		if b[k] != v {
			t.Errorf("%s: %q then %q", k, v, b[k])
		}
	}
	if a[libtags.ManagedBy] != "spawn" || a[libtags.Created] != "2026-11-01T09:30:00Z" || a[libtags.Role] != "" {
		t.Errorf("topic tags = %v, want managed-by=spawn created at the pipeline's CreatedAt, no role", a)
	}
}
