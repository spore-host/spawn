package taskproto

import (
	"encoding/json"
	"strings"
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
  "run_id": "11111111-2222-3333-4444-555555555555",
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
	if rec.RunID != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("run_id = %q, want the wrapper's stamped attempt id (spawn#608)", rec.RunID)
	}
}

// TestCompletionRecord_RunIDAdditive guards the additive-field contract for
// run_id (spawn#608): a record written by a PRE-fix wrapper has no run_id at all
// and must still parse (with RunID ""), and a record that has one must round-trip
// it. The empty case is what the --wait path treats as "unattributable", so it has
// to parse rather than error.
func TestCompletionRecord_RunIDAdditive(t *testing.T) {
	// An old record: exactly the pre-#608 wrapper's shape, no run_id key.
	old := `{"task_id":"align-42","exit_code":0,"state":"completed","started_at":"2026-07-19T12:00:00Z","ended_at":"2026-07-19T12:05:00Z"}`
	rec, err := ParseCompletionRecord([]byte(old))
	if err != nil {
		t.Fatalf("a pre-run_id record must still parse: %v", err)
	}
	if rec.RunID != "" {
		t.Errorf("RunID = %q, want empty for a record that predates run-id stamping", rec.RunID)
	}
	if rec.TaskID != "align-42" || rec.State != StateCompleted {
		t.Errorf("old record parsed wrong: %+v", rec)
	}

	// A current record round-trips run_id...
	data, err := json.Marshal(CompletionRecord{TaskID: "align-42", RunID: "run-7", State: StateFailed, ExitCode: 141})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"run_id":"run-7"`) {
		t.Errorf("marshalled record missing run_id: %s", data)
	}
	back, err := ParseCompletionRecord(data)
	if err != nil {
		t.Fatal(err)
	}
	if back.RunID != "run-7" {
		t.Errorf("RunID = %q, want run-7", back.RunID)
	}

	// ...and stays omitted (not `"run_id":""`) when unset, so nothing downstream
	// sees a new always-present key.
	data, err = json.Marshal(CompletionRecord{TaskID: "align-42", State: StateCompleted})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "run_id") {
		t.Errorf("run_id must be omitempty when unset: %s", data)
	}
}

func TestRetryClass_Retryable(t *testing.T) {
	cases := map[RetryClass]bool{
		RetryNone:                false,
		RetryAppError:            false,
		RetryStagingError:        false,
		RetryOutputDeliveryError: true, // spawn#561: compute succeeded, only delivery failed
		RetryCapacity:            true,
		RetrySpotInterruption:    true,
		RetryInstanceHealth:      false,
		RetryTTLExpired:          false,
		RetryControllerLost:      false,
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
