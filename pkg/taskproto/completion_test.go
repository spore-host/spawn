package taskproto

import (
	"encoding/json"
	"testing"
)

func TestCompletionRecord_RoundTrip(t *testing.T) {
	rec := CompletionRecord{
		TaskID:     "align-42",
		ExitCode:   0,
		State:      StateCompleted,
		StartedAt:  "2026-07-19T12:00:00Z",
		EndedAt:    "2026-07-19T12:05:00Z",
		Logs:       []string{"s3://b/tasks/align-42/command.log"},
		RetryClass: RetryNone,
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseCompletionRecord(data)
	if err != nil {
		t.Fatalf("ParseCompletionRecord: %v", err)
	}
	if got.TaskID != rec.TaskID || got.State != rec.State || got.ExitCode != rec.ExitCode ||
		got.StartedAt != rec.StartedAt || got.EndedAt != rec.EndedAt || len(got.Logs) != 1 {
		t.Errorf("round trip mismatch: %+v", got)
	}
}

// TestParseCompletionRecord_WrapperShape parses the exact JSON the generated
// bash wrapper emits, locking the wire contract between the two.
func TestParseCompletionRecord_WrapperShape(t *testing.T) {
	// Mirrors the heredoc in GenerateWrapper for a failed task.
	raw := `{
  "task_id": "align-42",
  "exit_code": 1,
  "state": "failed",
  "started_at": "2026-07-19T12:00:00Z",
  "ended_at": "2026-07-19T12:05:00Z",
  "logs": ["s3://spawn-results-1-us-east-1/tasks/align-42/command.log"],
  "retry_class": "app_error"
}`
	rec, err := ParseCompletionRecord([]byte(raw))
	if err != nil {
		t.Fatalf("ParseCompletionRecord: %v", err)
	}
	if rec.ExitCode != 1 || rec.State != StateFailed || rec.RetryClass != RetryAppError {
		t.Errorf("unexpected parse: %+v", rec)
	}
}

func TestRetryClass_Retryable(t *testing.T) {
	cases := map[RetryClass]bool{
		RetryNone:             false,
		RetryAppError:         false,
		RetryStagingError:     false,
		RetryCapacity:         true,
		RetrySpotInterruption: true,
		RetryInstanceHealth:   false,
		RetryTTLExpired:       false,
		RetryControllerLost:   false,
	}
	for rc, want := range cases {
		if got := rc.Retryable(); got != want {
			t.Errorf("%q.Retryable() = %v, want %v", rc, got, want)
		}
	}
}

func TestClassifyLaunchError(t *testing.T) {
	cases := map[string]RetryClass{
		"InsufficientInstanceCapacity": RetryCapacity,
		"MaxSpotInstanceCountExceeded": RetryCapacity,
		"SpotMaxPriceTooLow":           RetryCapacity,
		"RequestLimitExceeded":         RetryCapacity,
		"Unsupported":                  RetryCapacity,
		"InvalidParameterValue":        RetryNone,
		"UnauthorizedOperation":        RetryNone,
		"":                             RetryNone,
	}
	for code, want := range cases {
		if got := ClassifyLaunchError(code); got != want {
			t.Errorf("ClassifyLaunchError(%q) = %q, want %q", code, got, want)
		}
	}
}
