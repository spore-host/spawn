package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spore-host/spawn/pkg/queue"
	"github.com/spore-host/spawn/pkg/security"
)

// QueueRunner executes a batch job queue sequentially
type QueueRunner struct {
	config     *queue.QueueConfig
	state      *QueueState
	stateFile  string
	s3Client   *s3.Client
	s3Uploader *manager.Uploader
	ctx        context.Context
	cancelFunc context.CancelFunc
}

// QueueState tracks the execution state of the queue
type QueueState struct {
	QueueID   string     `json:"queue_id"`
	StartedAt time.Time  `json:"started_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	Status    string     `json:"status"` // "running" | "completed" | "failed"
	Jobs      []JobState `json:"jobs"`
}

// JobState tracks the state of an individual job
type JobState struct {
	JobID           string    `json:"job_id"`
	Status          string    `json:"status"` // "pending" | "running" | "completed" | "failed" | "skipped"
	StartedAt       time.Time `json:"started_at,omitempty"`
	CompletedAt     time.Time `json:"completed_at,omitempty"`
	ExitCode        int       `json:"exit_code,omitempty"`
	Attempt         int       `json:"attempt"`
	PID             int       `json:"pid,omitempty"`
	ErrorMessage    string    `json:"error_message,omitempty"`
	ResultsUploaded bool      `json:"results_uploaded"`
}

// JobResult represents the outcome of a job execution
type JobResult struct {
	ExitCode    int
	StdoutFile  string
	StderrFile  string
	CompletedAt time.Time
}

// NewQueueRunner creates a new queue runner using the default AWS credential chain.
func NewQueueRunner(ctx context.Context, queueFile string) (*QueueRunner, error) {
	awsCfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	return NewQueueRunnerWithAWSConfig(ctx, queueFile, "", awsCfg)
}

// NewQueueRunnerWithAWSConfig creates a queue runner with an injected AWS config.
// stateFile overrides the default state file path; pass "" to use /var/lib/spored/queue-state.json.
func NewQueueRunnerWithAWSConfig(ctx context.Context, queueFile, stateFile string, awsCfg aws.Config) (*QueueRunner, error) {
	cfg, err := queue.LoadConfig(queueFile)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	if stateFile == "" {
		stateFile = "/var/lib/spored/queue-state.json"
	}

	state, err := loadOrInitState(stateFile, cfg)
	if err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}

	s3Client := s3.NewFromConfig(awsCfg)
	runnerCtx, cancel := context.WithCancel(ctx)

	return &QueueRunner{
		config:     cfg,
		state:      state,
		stateFile:  stateFile,
		s3Client:   s3Client,
		s3Uploader: manager.NewUploader(s3Client),
		ctx:        runnerCtx,
		cancelFunc: cancel,
	}, nil
}

// Run executes the queue
func (r *QueueRunner) Run() error {
	// Setup signal handlers
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigChan
		fmt.Printf("Received signal %v, gracefully shutting down...\n", sig)
		r.cancelFunc()
	}()

	// Setup global timeout if specified
	if r.config.GlobalTimeout != "" {
		timeout, err := time.ParseDuration(r.config.GlobalTimeout)
		if err == nil {
			r.ctx, r.cancelFunc = context.WithTimeout(r.ctx, timeout)
		}
	}

	// Resolve execution order
	executionOrder, err := queue.TopologicalSort(r.config.Jobs)
	if err != nil {
		return fmt.Errorf("resolve dependencies: %w", err)
	}

	fmt.Printf("Execution order resolved: %v\n", executionOrder)

	// Execute jobs in order
	completedJobs := make(map[string]bool)
	for _, jobID := range executionOrder {
		// Check if context was cancelled
		select {
		case <-r.ctx.Done():
			r.state.Status = "failed"
			r.state.UpdatedAt = time.Now()
			_ = r.saveState()
			return fmt.Errorf("queue execution cancelled")
		default:
		}

		jobConfig := queue.GetJobConfig(r.config.Jobs, jobID)
		if jobConfig == nil {
			return fmt.Errorf("job config not found: %s", jobID)
		}

		// Check if job was already completed (resume scenario)
		if r.getJobState(jobID).Status == "completed" {
			fmt.Printf("Job %s already completed, skipping\n", jobID)
			completedJobs[jobID] = true
			continue
		}

		// Execute with retry
		result, err := r.executeJobWithRetry(jobConfig)
		if err != nil || result.ExitCode != 0 {
			// Update job state as failed
			r.updateJobState(jobID, &JobState{
				Status:       "failed",
				CompletedAt:  time.Now(),
				ExitCode:     result.ExitCode,
				ErrorMessage: fmt.Sprintf("%v", err),
			})
			_ = r.saveState()

			// Handle failure
			if r.config.OnFailure == "stop" {
				r.state.Status = "failed"
				r.state.UpdatedAt = time.Now()
				_ = r.saveState()
				return fmt.Errorf("job %s failed (exit code %d), stopping queue", jobID, result.ExitCode)
			}
			// Continue to next job if OnFailure == "continue"
			fmt.Printf("Job %s failed, continuing to next job\n", jobID)
			continue
		}

		// Update job state as completed
		r.updateJobState(jobID, &JobState{
			Status:      "completed",
			CompletedAt: result.CompletedAt,
			ExitCode:    result.ExitCode,
		})
		_ = r.saveState()

		// Upload results
		if len(jobConfig.ResultPaths) > 0 {
			fmt.Printf("Uploading results for job %s...\n", jobID)
			err = r.uploadJobResults(jobID, jobConfig.ResultPaths)
			if err != nil {
				fmt.Printf("Warning: failed to upload results for %s: %v\n", jobID, err)
			} else {
				r.markResultsUploaded(jobID)
				_ = r.saveState()
			}
		}

		completedJobs[jobID] = true
	}

	// Final state update
	r.state.Status = "completed"
	r.state.UpdatedAt = time.Now()
	_ = r.saveState()

	// Upload final state to S3
	_ = r.uploadFinalState()

	fmt.Println("Queue execution completed successfully")
	return nil
}

// executeJobWithRetry executes a job with retry logic
func (r *QueueRunner) executeJobWithRetry(job *queue.JobConfig) (*JobResult, error) {
	maxAttempts := 1

	if job.Retry != nil {
		maxAttempts = job.Retry.MaxAttempts
	}

	var lastErr error
	var lastResult *JobResult

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		fmt.Printf("Executing job %s (attempt %d/%d)...\n", job.JobID, attempt, maxAttempts)

		result, err := r.executeJob(job, attempt)
		if err == nil && result.ExitCode == 0 {
			return result, nil
		}

		lastErr = err
		lastResult = result

		// Check if we should retry based on exit code
		if attempt < maxAttempts {
			shouldRetry := queue.ShouldRetry(job.Retry, result.ExitCode)
			if !shouldRetry {
				fmt.Printf("Job %s failed with exit code %d (configured as non-retryable)\n", job.JobID, result.ExitCode)
				return lastResult, fmt.Errorf("job failed with non-retryable exit code %d: %w", result.ExitCode, lastErr)
			}

			backoffDuration, err := queue.CalculateBackoff(job.Retry, attempt)
			if err != nil {
				fmt.Printf("Warning: failed to calculate backoff: %v, using 5s default\n", err)
				backoffDuration = 5 * time.Second
			}
			fmt.Printf("Job %s failed (exit code %d), retrying after %v...\n", job.JobID, result.ExitCode, backoffDuration)
			time.Sleep(backoffDuration)
		}
	}

	return lastResult, fmt.Errorf("job failed after %d attempts: %w", maxAttempts, lastErr)
}

// executeJob executes a single job
func (r *QueueRunner) executeJob(job *queue.JobConfig, attempt int) (*JobResult, error) {
	// Parse timeout
	timeout, err := time.ParseDuration(job.Timeout)
	if err != nil {
		return nil, fmt.Errorf("parse timeout: %w", err)
	}

	ctx, cancel := context.WithTimeout(r.ctx, timeout)
	defer cancel()

	// nosemgrep: dangerous-exec-command -- user-defined job command, intentional
	cmd := exec.CommandContext(ctx, "/bin/bash", "-c", job.Command)

	// Set environment variables
	cmd.Env = os.Environ()
	for k, v := range job.Env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}
	cmd.Env = append(cmd.Env, fmt.Sprintf("JOB_ID=%s", job.JobID))
	cmd.Env = append(cmd.Env, fmt.Sprintf("JOB_ATTEMPT=%d", attempt))
	cmd.Env = append(cmd.Env, fmt.Sprintf("QUEUE_ID=%s", r.config.QueueID))

	// Capture output
	stdoutFile := fmt.Sprintf("/var/log/spored/jobs/%s-stdout.log", job.JobID)
	stderrFile := fmt.Sprintf("/var/log/spored/jobs/%s-stderr.log", job.JobID)

	stdout, err := os.Create(stdoutFile)
	if err != nil {
		return nil, fmt.Errorf("create stdout file: %w", err)
	}
	defer func() { _ = stdout.Close() }()

	stderr, err := os.Create(stderrFile)
	if err != nil {
		return nil, fmt.Errorf("create stderr file: %w", err)
	}
	defer func() { _ = stderr.Close() }()

	cmd.Stdout = stdout
	cmd.Stderr = stderr

	// Start job
	startTime := time.Now()
	err = cmd.Start()
	if err != nil {
		return nil, fmt.Errorf("start command: %w", err)
	}

	// Update job state
	r.updateJobState(job.JobID, &JobState{
		Status:    "running",
		StartedAt: startTime,
		Attempt:   attempt,
		PID:       cmd.Process.Pid,
	})
	_ = r.saveState()

	// Wait for command to complete
	err = cmd.Wait()

	exitCode := 0
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			exitCode = exitError.ExitCode()
		} else {
			exitCode = 1
		}
	}

	result := &JobResult{
		ExitCode:    exitCode,
		StdoutFile:  stdoutFile,
		StderrFile:  stderrFile,
		CompletedAt: time.Now(),
	}

	return result, nil
}

// uploadJobResults uploads job result files to S3
func (r *QueueRunner) uploadJobResults(jobID string, resultPaths []string) error {
	for _, pattern := range resultPaths {
		// Validate pattern for security (prevent path traversal)
		if err := security.ValidatePathForReading(pattern); err != nil {
			return fmt.Errorf("invalid result path pattern %s: %w", pattern, err)
		}

		// Expand glob pattern
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return fmt.Errorf("glob %s: %w", pattern, err)
		}

		for _, filePath := range matches {
			// Upload to S3
			s3Key := fmt.Sprintf("%s/jobs/%s/%s",
				r.config.ResultS3Prefix, jobID, filepath.Base(filePath))

			err = r.uploadFile(filePath, r.config.ResultS3Bucket, s3Key)
			if err != nil {
				return fmt.Errorf("upload %s: %w", filePath, err)
			}
		}
	}

	// Also upload stdout/stderr logs
	_ = r.uploadFile(
		fmt.Sprintf("/var/log/spored/jobs/%s-stdout.log", jobID),
		r.config.ResultS3Bucket,
		fmt.Sprintf("%s/jobs/%s/stdout.log", r.config.ResultS3Prefix, jobID),
	)
	_ = r.uploadFile(
		fmt.Sprintf("/var/log/spored/jobs/%s-stderr.log", jobID),
		r.config.ResultS3Bucket,
		fmt.Sprintf("%s/jobs/%s/stderr.log", r.config.ResultS3Prefix, jobID),
	)

	return nil
}

// uploadFile uploads a file to S3
func (r *QueueRunner) uploadFile(localPath, bucket, key string) error {
	file, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open file: %w", err)
	}
	defer func() { _ = file.Close() }()

	_, err = r.s3Uploader.Upload(r.ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   file,
	})
	if err != nil {
		return fmt.Errorf("s3 upload: %w", err)
	}

	return nil
}

// saveState saves the queue state to disk atomically
func (r *QueueRunner) saveState() error {
	r.state.UpdatedAt = time.Now()

	// Marshal state
	data, err := json.MarshalIndent(r.state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}

	// Atomic write with temp file + rename
	tempFile := r.stateFile + ".tmp"
	err = os.WriteFile(tempFile, data, 0644)
	if err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}

	err = os.Rename(tempFile, r.stateFile)
	if err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}

	return nil
}

// uploadFinalState uploads the final queue state to S3
func (r *QueueRunner) uploadFinalState() error {
	s3Key := fmt.Sprintf("%s/queue-state.json", r.config.ResultS3Prefix)
	return r.uploadFile(r.stateFile, r.config.ResultS3Bucket, s3Key)
}

// getJobState returns the current state of a job
func (r *QueueRunner) getJobState(jobID string) *JobState {
	for i := range r.state.Jobs {
		if r.state.Jobs[i].JobID == jobID {
			return &r.state.Jobs[i]
		}
	}
	return nil
}

// updateJobState updates the state of a job
func (r *QueueRunner) updateJobState(jobID string, update *JobState) {
	for i := range r.state.Jobs {
		if r.state.Jobs[i].JobID == jobID {
			// Merge update into existing state
			if update.Status != "" {
				r.state.Jobs[i].Status = update.Status
			}
			if !update.StartedAt.IsZero() {
				r.state.Jobs[i].StartedAt = update.StartedAt
			}
			if !update.CompletedAt.IsZero() {
				r.state.Jobs[i].CompletedAt = update.CompletedAt
			}
			if update.ExitCode != 0 || update.Status == "completed" {
				r.state.Jobs[i].ExitCode = update.ExitCode
			}
			if update.Attempt > 0 {
				r.state.Jobs[i].Attempt = update.Attempt
			}
			if update.PID > 0 {
				r.state.Jobs[i].PID = update.PID
			}
			if update.ErrorMessage != "" {
				r.state.Jobs[i].ErrorMessage = update.ErrorMessage
			}
			return
		}
	}
}

// markResultsUploaded marks a job's results as uploaded
func (r *QueueRunner) markResultsUploaded(jobID string) {
	for i := range r.state.Jobs {
		if r.state.Jobs[i].JobID == jobID {
			r.state.Jobs[i].ResultsUploaded = true
			return
		}
	}
}

// loadOrInitState loads existing state or initializes new state
func loadOrInitState(stateFile string, cfg *queue.QueueConfig) (*QueueState, error) {
	// Try to load existing state (resume scenario)
	if _, err := os.Stat(stateFile); err == nil {
		data, err := os.ReadFile(stateFile)
		if err != nil {
			return nil, fmt.Errorf("read state file: %w", err)
		}

		var state QueueState
		if err := json.Unmarshal(data, &state); err != nil {
			return nil, fmt.Errorf("unmarshal state: %w", err)
		}

		fmt.Println("Resuming from existing state")
		return &state, nil
	}

	// Initialize new state
	state := &QueueState{
		QueueID:   cfg.QueueID,
		StartedAt: time.Now(),
		UpdatedAt: time.Now(),
		Status:    "running",
		Jobs:      make([]JobState, len(cfg.Jobs)),
	}

	for i, job := range cfg.Jobs {
		state.Jobs[i] = JobState{
			JobID:  job.JobID,
			Status: "pending",
		}
	}

	return state, nil
}
