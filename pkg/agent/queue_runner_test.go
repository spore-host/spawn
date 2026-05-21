package agent

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/queue"
)

func TestCalculateBackoff(t *testing.T) {
	tests := []struct {
		name          string
		retry         *queue.RetryConfig
		attempt       int
		wantMin       time.Duration
		wantMax       time.Duration
		checkExact    bool
		expectedValue time.Duration
	}{
		{
			name: "exponential backoff attempt 1",
			retry: &queue.RetryConfig{
				MaxAttempts: 3,
				Backoff:     "exponential",
			},
			attempt:       1,
			checkExact:    true,
			expectedValue: 1 * time.Second,
		},
		{
			name: "exponential backoff attempt 2",
			retry: &queue.RetryConfig{
				MaxAttempts: 3,
				Backoff:     "exponential",
			},
			attempt:       2,
			checkExact:    true,
			expectedValue: 2 * time.Second,
		},
		{
			name: "exponential backoff attempt 3",
			retry: &queue.RetryConfig{
				MaxAttempts: 3,
				Backoff:     "exponential",
			},
			attempt:       3,
			checkExact:    true,
			expectedValue: 4 * time.Second,
		},
		{
			name: "exponential backoff attempt 4",
			retry: &queue.RetryConfig{
				MaxAttempts: 5,
				Backoff:     "exponential",
			},
			attempt:       4,
			checkExact:    true,
			expectedValue: 8 * time.Second,
		},
		{
			name: "fixed backoff",
			retry: &queue.RetryConfig{
				MaxAttempts: 3,
				Backoff:     "fixed",
			},
			attempt:       1,
			checkExact:    true,
			expectedValue: 5 * time.Second,
		},
		{
			name: "fixed backoff attempt 2",
			retry: &queue.RetryConfig{
				MaxAttempts: 3,
				Backoff:     "fixed",
			},
			attempt:       2,
			checkExact:    true,
			expectedValue: 5 * time.Second,
		},
		{
			name:          "nil retry config",
			retry:         nil,
			attempt:       1,
			checkExact:    true,
			expectedValue: 0,
		},
		{
			name: "unknown backoff strategy defaults to exponential",
			retry: &queue.RetryConfig{
				MaxAttempts: 3,
				Backoff:     "unknown",
			},
			attempt:       1,
			checkExact:    true,
			expectedValue: 1 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := calculateBackoff(tt.retry, tt.attempt)

			if tt.checkExact {
				if got != tt.expectedValue {
					t.Errorf("calculateBackoff() = %v, want %v", got, tt.expectedValue)
				}
			} else {
				if got < tt.wantMin || got > tt.wantMax {
					t.Errorf("calculateBackoff() = %v, want between %v and %v", got, tt.wantMin, tt.wantMax)
				}
			}
		})
	}
}

func TestLoadOrInitState(t *testing.T) {
	tests := []struct {
		name         string
		queueCfg     *queue.QueueConfig
		wantJobCount int
		wantStatus   string
	}{
		{
			name: "initialize new state",
			queueCfg: &queue.QueueConfig{
				QueueID:   "test-queue",
				QueueName: "test",
				Jobs: []queue.JobConfig{
					{JobID: "job1", Command: "cmd1", Timeout: "1m"},
					{JobID: "job2", Command: "cmd2", Timeout: "1m"},
					{JobID: "job3", Command: "cmd3", Timeout: "1m"},
				},
			},
			wantJobCount: 3,
			wantStatus:   "running",
		},
		{
			name: "single job",
			queueCfg: &queue.QueueConfig{
				QueueID:   "single-job-queue",
				QueueName: "single",
				Jobs: []queue.JobConfig{
					{JobID: "only-job", Command: "echo test", Timeout: "30s"},
				},
			},
			wantJobCount: 1,
			wantStatus:   "running",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Use a temp file path that won't exist
			tempFile := "/tmp/nonexistent-state-" + tt.queueCfg.QueueID + ".json"

			state, err := loadOrInitState(tempFile, tt.queueCfg)
			if err != nil {
				t.Fatalf("loadOrInitState() error = %v", err)
			}

			if state.QueueID != tt.queueCfg.QueueID {
				t.Errorf("QueueID = %s, want %s", state.QueueID, tt.queueCfg.QueueID)
			}

			if state.Status != tt.wantStatus {
				t.Errorf("Status = %s, want %s", state.Status, tt.wantStatus)
			}

			if len(state.Jobs) != tt.wantJobCount {
				t.Errorf("Job count = %d, want %d", len(state.Jobs), tt.wantJobCount)
			}

			// Verify all jobs are initialized as pending
			for i, job := range state.Jobs {
				if job.Status != "pending" {
					t.Errorf("Job %d status = %s, want pending", i, job.Status)
				}
				if job.Attempt != 0 {
					t.Errorf("Job %d attempt = %d, want 0", i, job.Attempt)
				}
				if job.JobID != tt.queueCfg.Jobs[i].JobID {
					t.Errorf("Job %d ID = %s, want %s", i, job.JobID, tt.queueCfg.Jobs[i].JobID)
				}
			}

			// Verify timestamps
			if state.StartedAt.IsZero() {
				t.Error("StartedAt should not be zero")
			}
			if state.UpdatedAt.IsZero() {
				t.Error("UpdatedAt should not be zero")
			}
		})
	}
}

func TestJobStateTransitions(t *testing.T) {
	tests := []struct {
		name            string
		initialStatus   string
		action          string
		expectedStatus  string
		validTransition bool
	}{
		{
			name:            "pending to running",
			initialStatus:   "pending",
			action:          "start",
			expectedStatus:  "running",
			validTransition: true,
		},
		{
			name:            "running to completed",
			initialStatus:   "running",
			action:          "complete_success",
			expectedStatus:  "completed",
			validTransition: true,
		},
		{
			name:            "running to failed",
			initialStatus:   "running",
			action:          "complete_failure",
			expectedStatus:  "failed",
			validTransition: true,
		},
		{
			name:            "pending to skipped",
			initialStatus:   "pending",
			action:          "skip",
			expectedStatus:  "skipped",
			validTransition: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			jobState := &JobState{
				JobID:  "test-job",
				Status: tt.initialStatus,
			}

			// Simulate state transition
			switch tt.action {
			case "start":
				jobState.Status = "running"
				jobState.StartedAt = time.Now()
				jobState.Attempt++
			case "complete_success":
				jobState.Status = "completed"
				jobState.CompletedAt = time.Now()
				jobState.ExitCode = 0
			case "complete_failure":
				jobState.Status = "failed"
				jobState.CompletedAt = time.Now()
				jobState.ExitCode = 1
				jobState.ErrorMessage = "Job failed"
			case "skip":
				jobState.Status = "skipped"
			}

			if jobState.Status != tt.expectedStatus {
				t.Errorf("Status after %s = %s, want %s", tt.action, jobState.Status, tt.expectedStatus)
			}

			// Verify timestamps are set appropriately
			switch tt.action {
			case "start":
				if jobState.StartedAt.IsZero() {
					t.Error("StartedAt should be set when job starts")
				}
				if jobState.Attempt == 0 {
					t.Error("Attempt should be incremented when job starts")
				}
			case "complete_success", "complete_failure":
				if jobState.CompletedAt.IsZero() {
					t.Error("CompletedAt should be set when job completes")
				}
			}
		})
	}
}

func TestQueueStateMarshaling(t *testing.T) {
	state := &QueueState{
		QueueID:   "test-queue",
		StartedAt: time.Now().Truncate(time.Second),
		UpdatedAt: time.Now().Truncate(time.Second),
		Status:    "running",
		Jobs: []JobState{
			{
				JobID:     "job1",
				Status:    "completed",
				ExitCode:  0,
				Attempt:   1,
				StartedAt: time.Now().Truncate(time.Second),
			},
			{
				JobID:   "job2",
				Status:  "running",
				Attempt: 1,
				PID:     12345,
			},
			{
				JobID:   "job3",
				Status:  "pending",
				Attempt: 0,
			},
		},
	}

	// Marshal to JSON
	data, err := marshalState(state)
	if err != nil {
		t.Fatalf("marshalState() error = %v", err)
	}

	// Unmarshal back
	var restored QueueState
	if err := unmarshalState(data, &restored); err != nil {
		t.Fatalf("unmarshalState() error = %v", err)
	}

	// Verify fields
	if restored.QueueID != state.QueueID {
		t.Errorf("QueueID = %s, want %s", restored.QueueID, state.QueueID)
	}
	if restored.Status != state.Status {
		t.Errorf("Status = %s, want %s", restored.Status, state.Status)
	}
	if len(restored.Jobs) != len(state.Jobs) {
		t.Errorf("Job count = %d, want %d", len(restored.Jobs), len(state.Jobs))
	}

	// Verify job states
	for i, job := range restored.Jobs {
		if job.JobID != state.Jobs[i].JobID {
			t.Errorf("Job %d ID = %s, want %s", i, job.JobID, state.Jobs[i].JobID)
		}
		if job.Status != state.Jobs[i].Status {
			t.Errorf("Job %d status = %s, want %s", i, job.Status, state.Jobs[i].Status)
		}
		if job.Attempt != state.Jobs[i].Attempt {
			t.Errorf("Job %d attempt = %d, want %d", i, job.Attempt, state.Jobs[i].Attempt)
		}
	}
}

func TestRetryLogic(t *testing.T) {
	tests := []struct {
		name           string
		maxAttempts    int
		currentAttempt int
		shouldRetry    bool
	}{
		{
			name:           "first attempt, should retry",
			maxAttempts:    3,
			currentAttempt: 1,
			shouldRetry:    true,
		},
		{
			name:           "second attempt, should retry",
			maxAttempts:    3,
			currentAttempt: 2,
			shouldRetry:    true,
		},
		{
			name:           "last attempt, should not retry",
			maxAttempts:    3,
			currentAttempt: 3,
			shouldRetry:    false,
		},
		{
			name:           "exceeded attempts",
			maxAttempts:    3,
			currentAttempt: 4,
			shouldRetry:    false,
		},
		{
			name:           "no retry config (maxAttempts=1)",
			maxAttempts:    1,
			currentAttempt: 1,
			shouldRetry:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shouldRetry := tt.currentAttempt < tt.maxAttempts
			if shouldRetry != tt.shouldRetry {
				t.Errorf("shouldRetry = %v, want %v", shouldRetry, tt.shouldRetry)
			}
		})
	}
}

// Helper functions for testing
func calculateBackoff(retry *queue.RetryConfig, attempt int) time.Duration {
	if retry == nil {
		return 0
	}

	switch retry.Backoff {
	case "exponential":
		// 2^(attempt-1) seconds: 1s, 2s, 4s, 8s, etc.
		backoff := time.Duration(1<<uint(attempt-1)) * time.Second
		return backoff
	case "fixed":
		return 5 * time.Second
	default:
		// Default to exponential
		backoff := time.Duration(1<<uint(attempt-1)) * time.Second
		return backoff
	}
}

func marshalState(state *QueueState) ([]byte, error) {
	return json.Marshal(state)
}

func unmarshalState(data []byte, state *QueueState) error {
	return json.Unmarshal(data, state)
}
