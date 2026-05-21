package scheduler

import "time"

// ScheduleRecord represents a scheduled parameter sweep execution stored in DynamoDB.
type ScheduleRecord struct {
	ScheduleID   string    `dynamodbav:"schedule_id"`
	UserID       string    `dynamodbav:"user_id"`
	ScheduleName string    `dynamodbav:"schedule_name"`
	CreatedAt    time.Time `dynamodbav:"created_at"`
	UpdatedAt    time.Time `dynamodbav:"updated_at"`

	// Schedule configuration
	ScheduleExpression string    `dynamodbav:"schedule_expression"` // Cron or at()
	ScheduleType       string    `dynamodbav:"schedule_type"`       // "one-time" | "recurring"
	Timezone           string    `dynamodbav:"timezone"`            // IANA timezone
	NextExecutionTime  time.Time `dynamodbav:"next_execution_time"`

	// Sweep configuration (passed to orchestrator)
	S3ParamsKey   string `dynamodbav:"s3_params_key"`
	SweepName     string `dynamodbav:"sweep_name"`
	MaxConcurrent int    `dynamodbav:"max_concurrent"`
	LaunchDelay   string `dynamodbav:"launch_delay"`
	Region        string `dynamodbav:"region"`

	// Execution limits
	Status         string    `dynamodbav:"status"` // "active" | "paused" | "cancelled"
	ExecutionCount int       `dynamodbav:"execution_count"`
	MaxExecutions  int       `dynamodbav:"max_executions,omitempty"` // 0 = unlimited
	EndAfter       time.Time `dynamodbav:"end_after,omitempty"`      // Zero value = unlimited
	LastSweepID    string    `dynamodbav:"last_sweep_id,omitempty"`  // Most recent sweep ID
	ScheduleARN    string    `dynamodbav:"schedule_arn,omitempty"`   // EventBridge schedule ARN
	TTL            int64     `dynamodbav:"ttl"`                      // Auto-delete after 90 days
}

// ExecutionHistory represents a record of a schedule execution attempt.
type ExecutionHistory struct {
	ScheduleID    string    `dynamodbav:"schedule_id"`
	ExecutionTime time.Time `dynamodbav:"execution_time"`
	SweepID       string    `dynamodbav:"sweep_id"`
	Status        string    `dynamodbav:"status"` // "success" | "failed"
	ErrorMessage  string    `dynamodbav:"error_message,omitempty"`
	UserID        string    `dynamodbav:"user_id"`
	TTL           int64     `dynamodbav:"ttl"` // Auto-delete after 30 days
}

// ScheduleStatus represents the possible states of a schedule.
type ScheduleStatus string

const (
	ScheduleStatusActive    ScheduleStatus = "active"
	ScheduleStatusPaused    ScheduleStatus = "paused"
	ScheduleStatusCancelled ScheduleStatus = "cancelled"
)

// ScheduleType represents whether a schedule runs once or repeatedly.
type ScheduleType string

const (
	ScheduleTypeOneTime   ScheduleType = "one-time"
	ScheduleTypeRecurring ScheduleType = "recurring"
)

// ExecutionStatus represents the outcome of a schedule execution.
type ExecutionStatus string

const (
	ExecutionStatusSuccess ExecutionStatus = "success"
	ExecutionStatusFailed  ExecutionStatus = "failed"
)
