package queue

import (
	"strings"
	"testing"
	"time"
)

func TestValidateQueue(t *testing.T) {
	tests := []struct {
		name    string
		config  *QueueConfig
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid simple queue",
			config: &QueueConfig{
				QueueID:   "test-queue",
				QueueName: "test",
				Jobs: []JobConfig{
					{
						JobID:   "job1",
						Command: "echo 'hello'",
						Timeout: "5m",
					},
					{
						JobID:   "job2",
						Command: "echo 'world'",
						Timeout: "5m",
					},
				},
				GlobalTimeout:  "1h",
				OnFailure:      "stop",
				ResultS3Bucket: "test-bucket",
				ResultS3Prefix: "test-prefix",
			},
			wantErr: false,
		},
		{
			name: "valid queue with dependencies",
			config: &QueueConfig{
				QueueID:   "test-queue",
				QueueName: "test",
				Jobs: []JobConfig{
					{
						JobID:   "job1",
						Command: "echo 'hello'",
						Timeout: "5m",
					},
					{
						JobID:     "job2",
						Command:   "echo 'world'",
						Timeout:   "5m",
						DependsOn: []string{"job1"},
					},
				},
				GlobalTimeout:  "1h",
				OnFailure:      "stop",
				ResultS3Bucket: "test-bucket",
			},
			wantErr: false,
		},
		{
			name: "duplicate job IDs",
			config: &QueueConfig{
				QueueID:   "test-queue",
				QueueName: "test",
				Jobs: []JobConfig{
					{
						JobID:   "job1",
						Command: "echo 'hello'",
						Timeout: "5m",
					},
					{
						JobID:   "job1",
						Command: "echo 'world'",
						Timeout: "5m",
					},
				},
				GlobalTimeout:  "1h",
				OnFailure:      "stop",
				ResultS3Bucket: "test-bucket",
			},
			wantErr: true,
			errMsg:  "duplicate job_id",
		},
		{
			name: "circular dependency",
			config: &QueueConfig{
				QueueID:   "test-queue",
				QueueName: "test",
				Jobs: []JobConfig{
					{
						JobID:     "job1",
						Command:   "echo 'hello'",
						Timeout:   "5m",
						DependsOn: []string{"job2"},
					},
					{
						JobID:     "job2",
						Command:   "echo 'world'",
						Timeout:   "5m",
						DependsOn: []string{"job1"},
					},
				},
				GlobalTimeout:  "1h",
				OnFailure:      "stop",
				ResultS3Bucket: "test-bucket",
			},
			wantErr: true,
			errMsg:  "circular dependency",
		},
		{
			name: "missing dependency",
			config: &QueueConfig{
				QueueID:   "test-queue",
				QueueName: "test",
				Jobs: []JobConfig{
					{
						JobID:     "job1",
						Command:   "echo 'hello'",
						Timeout:   "5m",
						DependsOn: []string{"nonexistent"},
					},
				},
				GlobalTimeout:  "1h",
				OnFailure:      "stop",
				ResultS3Bucket: "test-bucket",
			},
			wantErr: true,
			errMsg:  "depends on non-existent job",
		},
		{
			name: "invalid timeout",
			config: &QueueConfig{
				QueueID:   "test-queue",
				QueueName: "test",
				Jobs: []JobConfig{
					{
						JobID:   "job1",
						Command: "echo 'hello'",
						Timeout: "invalid",
					},
				},
				GlobalTimeout:  "1h",
				OnFailure:      "stop",
				ResultS3Bucket: "test-bucket",
			},
			wantErr: true,
			errMsg:  "invalid timeout",
		},
		{
			name: "empty job ID",
			config: &QueueConfig{
				QueueID:   "test-queue",
				QueueName: "test",
				Jobs: []JobConfig{
					{
						JobID:   "",
						Command: "echo 'hello'",
						Timeout: "5m",
					},
				},
				GlobalTimeout:  "1h",
				OnFailure:      "stop",
				ResultS3Bucket: "test-bucket",
			},
			wantErr: true,
			errMsg:  "job_id is required",
		},
		{
			name: "empty command",
			config: &QueueConfig{
				QueueID:   "test-queue",
				QueueName: "test",
				Jobs: []JobConfig{
					{
						JobID:   "job1",
						Command: "",
						Timeout: "5m",
					},
				},
				GlobalTimeout:  "1h",
				OnFailure:      "stop",
				ResultS3Bucket: "test-bucket",
			},
			wantErr: true,
			errMsg:  "command is required",
		},
		{
			name: "invalid on_failure value",
			config: &QueueConfig{
				QueueID:   "test-queue",
				QueueName: "test",
				Jobs: []JobConfig{
					{
						JobID:   "job1",
						Command: "echo 'hello'",
						Timeout: "5m",
					},
				},
				GlobalTimeout:  "1h",
				OnFailure:      "invalid",
				ResultS3Bucket: "test-bucket",
			},
			wantErr: true,
			errMsg:  "on_failure must be",
		},
		{
			name: "valid continue on failure",
			config: &QueueConfig{
				QueueID:   "test-queue",
				QueueName: "test",
				Jobs: []JobConfig{
					{
						JobID:   "job1",
						Command: "echo 'hello'",
						Timeout: "5m",
					},
				},
				GlobalTimeout:  "1h",
				OnFailure:      "continue",
				ResultS3Bucket: "test-bucket",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateQueue(tt.config)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateQueue() error = %v, wantErr %v", err, tt.wantErr)
			}

			if tt.wantErr && err != nil && tt.errMsg != "" {
				if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tt.errMsg)) {
					t.Errorf("ValidateQueue() error = %v, should contain %q", err, tt.errMsg)
				}
			}
		})
	}
}

func TestParseTimeout(t *testing.T) {
	tests := []struct {
		name    string
		timeout string
		want    time.Duration
		wantErr bool
	}{
		{
			name:    "seconds",
			timeout: "30s",
			want:    30 * time.Second,
			wantErr: false,
		},
		{
			name:    "minutes",
			timeout: "5m",
			want:    5 * time.Minute,
			wantErr: false,
		},
		{
			name:    "hours",
			timeout: "2h",
			want:    2 * time.Hour,
			wantErr: false,
		},
		{
			name:    "invalid format",
			timeout: "invalid",
			wantErr: true,
		},
		{
			name:    "empty string",
			timeout: "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := time.ParseDuration(tt.timeout)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseDuration() error = %v, wantErr %v", err, tt.wantErr)
			}

			if !tt.wantErr && got != tt.want {
				t.Errorf("ParseDuration() = %v, want %v", got, tt.want)
			}
		})
	}
}
