package scheduler

import (
	"fmt"
	"strings"
	"time"
)

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
	S3ParamsKey string `dynamodbav:"s3_params_key"`
	// S3ParamsBucket is the bucket the params were ACTUALLY written to (#804).
	//
	// resolveSchedulesBucket prefers the shared `spawn-schedules-<region>` and
	// falls back to an account-scoped `spawn-schedules-<account>-<region>` when
	// the shared name is taken by another account. It returns which one it used,
	// and the caller used to discard that — so the record carried only a key and
	// the scheduler-handler rebuilt the bucket from a region template. In any
	// account that took the fallback, every scheduled execution therefore read a
	// bucket it had never written to.
	//
	// omitempty so records written before this field existed unmarshal cleanly;
	// the reader falls back to the template for those.
	S3ParamsBucket string `dynamodbav:"s3_params_bucket,omitempty"`
	SweepName      string `dynamodbav:"sweep_name"`
	MaxConcurrent  int    `dynamodbav:"max_concurrent"`
	LaunchDelay    string `dynamodbav:"launch_delay"`
	Region         string `dynamodbav:"region"`
	// AWSAccountID is the compute account the sweep's instances launch in
	// (#1026). UserID is the account that CREATED the schedule — the infra
	// account, since `spawn schedule create` runs against it — so the
	// orchestrator, which assumes SpawnSweepCrossAccountRole in this account,
	// had nothing to go on. omitempty so older records unmarshal; the
	// scheduler-handler refuses to start a sweep from one.
	AWSAccountID string `dynamodbav:"aws_account_id,omitempty"`

	// Execution limits
	Status         string    `dynamodbav:"status"` // "active" | "paused" | "cancelled"
	ExecutionCount int       `dynamodbav:"execution_count"`
	MaxExecutions  int       `dynamodbav:"max_executions,omitempty"` // 0 = unlimited
	EndAfter       time.Time `dynamodbav:"end_after,omitempty"`      // Zero value = unlimited
	LastSweepID    string    `dynamodbav:"last_sweep_id,omitempty"`  // Most recent sweep ID
	ScheduleARN    string    `dynamodbav:"schedule_arn,omitempty"`   // EventBridge schedule ARN
	// TTL is the record's DynamoDB expiry (RecordTTL). omitempty: zero means
	// no TTL attribute at all — a recurring schedule with no end must not
	// age out while its EventBridge schedule fires (#1382).
	TTL int64 `dynamodbav:"ttl,omitempty"`
}

// RecordTTLMargin is how long a schedule's record outlives its last possible
// firing, so `spawn schedule show` can still explain a finished schedule. It
// matches the execution history's 30-day TTL.
const RecordTTLMargin = 30 * 24 * time.Hour

// RecordTTL is the record's DynamoDB TTL (Unix seconds), or 0 for none.
//
// A schedule with a last possible firing — a one-time at() schedule, or one
// with EndAfter — ages out RecordTTLMargin after it (the later of the two, if
// both), with no cancel needed. It is never earlier: until then FiringAt can
// still say FiringLaunches, and the record is what scheduler-handler and the
// reaper's role sweep (#1378) read. CreateSchedule gives an EndAfter
// schedule's EventBridge schedule the same end (EndDate), so the two go
// together.
//
// A recurring schedule with no EndAfter has no last firing in time (a
// MaxExecutions limit counts successful runs, which may never come), so it
// gets no TTL: the record lives as long as the schedule. Its deleters are
// `spawn schedule cancel` (DeleteSchedule) and, once it has run out,
// scheduler-handler (CompleteSchedule, which then sets a TTL). This replaces
// a flat now+90 days, which deleted the record of a schedule that kept
// firing. Refreshing the TTL on each firing was the alternative, and is
// worse: a paused schedule does not fire, nor does one whose cadence is
// longer than the TTL, so both would lose their record while their
// EventBridge schedule and params stayed.
func (r *ScheduleRecord) RecordTTL() int64 {
	var last time.Time
	if at, ok := r.oneTimeAt(); ok {
		last = at
	}
	if r.EndAfter.After(last) {
		last = r.EndAfter
	}
	if last.IsZero() {
		return 0
	}
	return last.Add(RecordTTLMargin).Unix()
}

// oneTimeAt is the firing time of an at(yyyy-mm-ddThh:mm:ss) expression, in
// the schedule's Timezone (UTC if unset), as EventBridge reads it.
func (r *ScheduleRecord) oneTimeAt() (time.Time, bool) {
	expr := r.ScheduleExpression
	if !strings.HasPrefix(expr, "at(") || !strings.HasSuffix(expr, ")") {
		return time.Time{}, false
	}
	loc := time.UTC
	if r.Timezone != "" {
		l, err := time.LoadLocation(r.Timezone)
		if err != nil {
			return time.Time{}, false
		}
		loc = l
	}
	t, err := time.ParseInLocation("2006-01-02T15:04:05", expr[len("at("):len(expr)-1], loc)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
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
	// ScheduleStatusCompleted: the schedule ran out and scheduler-handler
	// deleted its EventBridge schedule and params (CompleteSchedule).
	ScheduleStatusCompleted ScheduleStatus = "completed"
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

// Where `spawn schedule create` keeps a schedule. The CLI writes the record
// with NewClient's table, in the region it loads its infra config in
// (spawnconfig.LoadInfraAWSConfig(ctx, TableRegion)), and the params to
// ParamsKeyPrefix in the bucket the record names. Anything that has to know
// which schedules exist (pkg/instancerole's schedule references, #1378) reads
// exactly these, so it reads what the CLI wrote.
const (
	TableName            = "spawn-schedules"
	TableRegion          = "us-east-1"
	ParamsBucketTemplate = "spawn-schedules-%s" // %s = the record's Region
	ParamsKeyPrefix      = "schedules/"

	// HandlerFunctionName is the function every schedule targets.
	// scripts/deploy-scheduler-handler.sh deploys it once, in TableRegion,
	// next to the table it reads. A schedule for another region keeps its
	// control plane here and launches its members in its Region (#1383).
	HandlerFunctionName = "scheduler-handler"
	// ExecutionRoleName is the role EventBridge Scheduler assumes to invoke
	// the handler.
	ExecutionRoleName = "SpawnSchedulerExecutionRole"
)

// HandlerARN is the scheduler-handler function a schedule in accountID
// targets: always in TableRegion, never the schedule's Region (#1383).
func HandlerARN(accountID string) string {
	return fmt.Sprintf("arn:aws:lambda:%s:%s:function:%s", TableRegion, accountID, HandlerFunctionName)
}

// ExecutionRoleARN is the role EventBridge Scheduler invokes the handler as.
func ExecutionRoleARN(accountID string) string {
	return fmt.Sprintf("arn:aws:iam::%s:role/%s", accountID, ExecutionRoleName)
}

// Firing is what scheduler-handler does with a schedule's record when the
// schedule fires.
type Firing int

const (
	// FiringLaunches: it reads the params and starts a sweep.
	FiringLaunches Firing = iota
	// FiringNotActive: the status is not "active" (paused or cancelled), so
	// it skips the firing.
	FiringNotActive
	// FiringMaxExecutions: MaxExecutions runs have happened; it refuses.
	FiringMaxExecutions
	// FiringExpired: EndAfter has passed; it refuses.
	FiringExpired
)

// FiringAt is scheduler-handler's decision for a firing at now, before it
// reads the params. It is the handler's own check, not a copy of it, so
// anything that asks "will this schedule launch again?" gets the handler's
// answer. No command raises MaxExecutions or EndAfter on an existing record,
// so a schedule that has run out stays that way; `spawn schedule resume` only
// moves a paused one back to active.
func (r *ScheduleRecord) FiringAt(now time.Time) Firing {
	switch {
	case r.Status != string(ScheduleStatusActive):
		return FiringNotActive
	case r.MaxExecutions > 0 && r.ExecutionCount >= r.MaxExecutions:
		return FiringMaxExecutions
	case !r.EndAfter.IsZero() && now.After(r.EndAfter):
		return FiringExpired
	}
	return FiringLaunches
}

// ParamsBucketName is the bucket holding the record's params: S3ParamsBucket,
// or, for a record written before #804, tmpl (ParamsBucketTemplate unless
// overridden) filled with the record's Region.
func (r *ScheduleRecord) ParamsBucketName(tmpl string) string {
	if r.S3ParamsBucket != "" {
		return r.S3ParamsBucket
	}
	return fmt.Sprintf(tmpl, r.Region)
}
