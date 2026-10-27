package reaperiam

import (
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/poolqueue"
)

// TestSQSGrantsAreScopedToPoolQueues is #1351's IAM rail. The reaper may read
// and delete spawn's run-scoped pool queues and nothing else: every sqs:
// action it holds is on exactly the spawn-pool-* ARN (derived from
// poolqueue.NamePrefix, not typed), except ListQueues, which has no
// resource-level ARN and returns names only. The sqs: actions are
// discovered, not listed, so a verb added later inherits the rule.
func TestSQSGrantsAreScopedToPoolQueues(t *testing.T) {
	wantARN := "arn:aws:sqs:*:*:" + poolqueue.NamePrefix + "*"
	if PoolQueueARN != wantARN {
		t.Fatalf("PoolQueueARN = %s, but poolqueue.NamePrefix makes it %s", PoolQueueARN, wantARN)
	}
	if !iamGlobMatches(PoolQueueARN, "arn:aws:sqs:us-east-1:111122223333:"+poolqueue.QueueName("run-1")) {
		t.Errorf("%s does not match a pool queue's ARN", PoolQueueARN)
	}
	for _, other := range []string{
		"arn:aws:sqs:us-east-1:111122223333:my-app-queue",
		"arn:aws:sqs:us-east-1:111122223333:spawn-other",
		"arn:aws:sqs:us-east-1:111122223333:spawnpool-x",
	} {
		if iamGlobMatches(PoolQueueARN, other) {
			t.Errorf("%s matches %s, a queue that is not a pool's", PoolQueueARN, other)
		}
	}
	seen := map[string]bool{}
	for _, st := range ScanSelfStatements() {
		for _, a := range st.Action {
			if !strings.HasPrefix(a, "sqs:") {
				continue
			}
			seen[a] = true
			switch {
			case a == "sqs:ListQueues":
				if st.Resource != "*" || len(st.Action) != 1 {
					t.Errorf("sqs:ListQueues must be alone on '*' (it has no resource ARN), got %v on %s", st.Action, st.Resource)
				}
			case st.Resource != PoolQueueARN:
				t.Errorf("%s is granted on %s; only %s is allowed", a, st.Resource, PoolQueueARN)
			}
		}
	}
	for _, a := range []string{"sqs:ListQueues", "sqs:GetQueueAttributes", "sqs:ListQueueTags", "sqs:DeleteQueue"} {
		if !seen[a] {
			t.Errorf("%s, which the pool sweep calls, is not granted", a)
		}
	}
}
