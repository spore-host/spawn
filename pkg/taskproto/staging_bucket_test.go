package taskproto

import (
	"strings"
	"testing"
)

// TestValidateRefusesTheStagingBucket: no TaskSpec may name a spawn staging
// bucket, in any field that becomes a grant on the task's role or a place it
// writes (#1274). Any account and region: the prefix is the rule.
func TestValidateRefusesTheStagingBucket(t *testing.T) {
	base := func() *TaskSpec {
		return &TaskSpec{
			TaskID: "t1", Command: []string{"true"},
			Lifecycle: Lifecycle{TTL: "1h"},
		}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("the base spec is invalid: %v", err)
	}
	staging := "s3://" + StagingBucket("999999999999", "eu-west-1")
	for name, mutate := range map[string]func(*TaskSpec){
		"input source":       func(s *TaskSpec) { s.Inputs = []Manifest{{Source: staging + "/t/x", Destination: "/x"}} },
		"output destination": func(s *TaskSpec) { s.Outputs = []Manifest{{Source: "/out", Destination: staging + "/out/"}} },
		"s3_read_write":      func(s *TaskSpec) { s.Resources.S3ReadWrite = []string{staging} },
		"results_prefix":     func(s *TaskSpec) { s.ResultsPrefix = staging + "/results" },
	} {
		t.Run(name, func(t *testing.T) {
			s := base()
			mutate(s)
			err := s.Validate()
			if err == nil || !strings.Contains(err.Error(), "spawn staging bucket") {
				t.Errorf("Validate = %v, want a refusal naming the staging bucket", err)
			}
		})
	}
	s := base()
	s.Inputs = []Manifest{{Source: "s3://spawn-results-1-us-east-1/tasks/a/out", Destination: "/x"}}
	if err := s.Validate(); err != nil {
		t.Errorf("an ordinary spawn bucket was refused: %v", err)
	}
}
