package queue

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// QueueConfig represents a batch job queue configuration
type QueueConfig struct {
	QueueID        string      `json:"queue_id"`
	QueueName      string      `json:"queue_name"`
	Jobs           []JobConfig `json:"jobs"`
	GlobalTimeout  string      `json:"global_timeout"`
	OnFailure      string      `json:"on_failure"` // "stop" | "continue"
	ResultS3Bucket string      `json:"result_s3_bucket"`
	ResultS3Prefix string      `json:"result_s3_prefix"`
}

// JobConfig represents a single job in the queue
type JobConfig struct {
	JobID       string            `json:"job_id"`
	Command     string            `json:"command"`
	Timeout     string            `json:"timeout"`
	Env         map[string]string `json:"env,omitempty"`
	DependsOn   []string          `json:"depends_on"`
	Retry       *RetryConfig      `json:"retry,omitempty"`
	ResultPaths []string          `json:"result_paths,omitempty"`
}

// RetryConfig defines retry behavior for a job
type RetryConfig struct {
	MaxAttempts      int     `json:"max_attempts"`
	Backoff          string  `json:"backoff"`                       // "fixed" | "exponential" | "exponential-jitter"
	BaseDelay        string  `json:"base_delay,omitempty"`          // e.g., "2s", "1m"
	MaxDelay         string  `json:"max_delay,omitempty"`           // e.g., "5m", "1h"
	Jitter           float64 `json:"jitter,omitempty"`              // 0.0 - 1.0, adds randomization
	RetryOnCodes     []int   `json:"retry_on_codes,omitempty"`      // Only retry these exit codes
	DontRetryOnCodes []int   `json:"dont_retry_on_codes,omitempty"` // Never retry these exit codes
}

// LoadConfig loads a queue configuration from a JSON file
func LoadConfig(filename string) (*QueueConfig, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}

	var config QueueConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parse json: %w", err)
	}

	// Validate configuration
	if err := ValidateQueue(&config); err != nil {
		return nil, fmt.Errorf("validation failed: %w", err)
	}

	return &config, nil
}

// ValidateQueue performs comprehensive validation of a queue configuration
func ValidateQueue(cfg *QueueConfig) error {
	if cfg.QueueID == "" {
		return fmt.Errorf("queue_id is required")
	}

	if len(cfg.Jobs) == 0 {
		return fmt.Errorf("at least one job is required")
	}

	// Check for duplicate job IDs
	jobIDs := make(map[string]bool)
	for _, job := range cfg.Jobs {
		if job.JobID == "" {
			return fmt.Errorf("job_id is required for all jobs")
		}
		if jobIDs[job.JobID] {
			return fmt.Errorf("duplicate job_id: %s", job.JobID)
		}
		jobIDs[job.JobID] = true

		if job.Command == "" {
			return fmt.Errorf("command is required for job %s", job.JobID)
		}

		// Validate timeout format
		if job.Timeout == "" {
			return fmt.Errorf("timeout is required for job %s", job.JobID)
		}
		if _, err := time.ParseDuration(job.Timeout); err != nil {
			return fmt.Errorf("invalid timeout format for job %s: %w", job.JobID, err)
		}

		// Validate dependencies exist
		for _, dep := range job.DependsOn {
			if !jobIDs[dep] && dep != job.JobID {
				// Check if dependency will be defined later
				found := false
				for _, j := range cfg.Jobs {
					if j.JobID == dep {
						found = true
						break
					}
				}
				if !found {
					return fmt.Errorf("job %s depends on non-existent job: %s", job.JobID, dep)
				}
			}
			if dep == job.JobID {
				return fmt.Errorf("job %s cannot depend on itself", job.JobID)
			}
		}

		// Validate retry configuration
		if job.Retry != nil {
			if job.Retry.MaxAttempts < 1 {
				return fmt.Errorf("retry max_attempts must be >= 1 for job %s", job.JobID)
			}
			if job.Retry.Backoff != "fixed" && job.Retry.Backoff != "exponential" {
				return fmt.Errorf("retry backoff must be 'fixed' or 'exponential' for job %s", job.JobID)
			}
		}

		// Validate result paths are not empty strings
		for _, path := range job.ResultPaths {
			if path == "" {
				return fmt.Errorf("result_paths cannot contain empty strings for job %s", job.JobID)
			}
		}
	}

	// Validate global timeout
	if cfg.GlobalTimeout != "" {
		if _, err := time.ParseDuration(cfg.GlobalTimeout); err != nil {
			return fmt.Errorf("invalid global_timeout format: %w", err)
		}
	}

	// Validate on_failure value
	if cfg.OnFailure != "" && cfg.OnFailure != "stop" && cfg.OnFailure != "continue" {
		return fmt.Errorf("on_failure must be 'stop' or 'continue'")
	}

	// Validate dependency graph for cycles
	if err := validateDAG(cfg.Jobs); err != nil {
		return fmt.Errorf("dependency validation failed: %w", err)
	}

	return nil
}

// validateDAG checks for circular dependencies using DFS
func validateDAG(jobs []JobConfig) error {
	graph := make(map[string][]string)
	for _, job := range jobs {
		graph[job.JobID] = job.DependsOn
	}

	visited := make(map[string]bool)
	recStack := make(map[string]bool)

	var hasCycle func(string) bool
	hasCycle = func(jobID string) bool {
		visited[jobID] = true
		recStack[jobID] = true

		for _, dep := range graph[jobID] {
			if !visited[dep] {
				if hasCycle(dep) {
					return true
				}
			} else if recStack[dep] {
				return true
			}
		}

		recStack[jobID] = false
		return false
	}

	for _, job := range jobs {
		if !visited[job.JobID] {
			if hasCycle(job.JobID) {
				return fmt.Errorf("circular dependency detected involving job: %s", job.JobID)
			}
		}
	}

	return nil
}

// GenerateQueueID creates a unique queue ID
func GenerateQueueID() string {
	return fmt.Sprintf("queue-%s", time.Now().Format("20060102-150405"))
}
