package main

import (
	"time"
)

// API Response structures

// APIResponse is the standard API response format
type APIResponse struct {
	Success        bool           `json:"success"`
	Message        string         `json:"message,omitempty"`
	Error          string         `json:"error,omitempty"`
	AccountBase36  string         `json:"account_base36,omitempty"`
	RegionsQueried []string       `json:"regions_queried,omitempty"`
	TotalInstances int            `json:"total_instances,omitempty"`
	Instances      []InstanceInfo `json:"instances,omitempty"`
	Instance       *InstanceInfo  `json:"instance,omitempty"`
	User           *UserProfile   `json:"user,omitempty"`
}

// InstanceInfo represents EC2 instance information
type InstanceInfo struct {
	InstanceID          string            `json:"instance_id"`
	Name                string            `json:"name"`
	InstanceType        string            `json:"instance_type"`
	State               string            `json:"state"`
	Region              string            `json:"region"`
	AvailabilityZone    string            `json:"availability_zone"`
	PublicIP            string            `json:"public_ip,omitempty"`
	PrivateIP           string            `json:"private_ip,omitempty"`
	LaunchTime          time.Time         `json:"launch_time"`
	TTL                 string            `json:"ttl,omitempty"`
	TTLRemainingSeconds int               `json:"ttl_remaining_seconds,omitempty"`
	IdleTimeout         string            `json:"idle_timeout,omitempty"`
	DNSName             string            `json:"dns_name,omitempty"`
	SpotInstance        bool              `json:"spot_instance"`
	KeyName             string            `json:"key_name,omitempty"`
	Tags                map[string]string `json:"tags"`
}

// UserProfile represents user account information
type UserProfile struct {
	UserID        string    `json:"user_id"`
	AWSAccountID  string    `json:"aws_account_id"`
	AccountBase36 string    `json:"account_base36"`
	Email         string    `json:"email,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	LastAccess    time.Time `json:"last_access"`
}

// Sweep API Response structures

// SweepAPIResponse is the response for /api/sweeps
type SweepAPIResponse struct {
	Success     bool        `json:"success"`
	Message     string      `json:"message,omitempty"`
	Error       string      `json:"error,omitempty"`
	TotalSweeps int         `json:"total_sweeps"`
	Sweeps      []SweepInfo `json:"sweeps"`
}

// SweepDetailAPIResponse is the response for /api/sweeps/{id}
type SweepDetailAPIResponse struct {
	Success bool            `json:"success"`
	Message string          `json:"message,omitempty"`
	Error   string          `json:"error,omitempty"`
	Sweep   SweepDetailInfo `json:"sweep"`
}

// CancelSweepResponse is the response for /api/sweeps/{id}/cancel
type CancelSweepResponse struct {
	Success             bool   `json:"success"`
	Message             string `json:"message"`
	InstancesTerminated int    `json:"instances_terminated"`
}

// SweepInfo represents sweep summary information
type SweepInfo struct {
	SweepID         string     `json:"sweep_id"`
	SweepName       string     `json:"sweep_name"`
	Status          string     `json:"status"`
	TotalParams     int        `json:"total_params"`
	Launched        int        `json:"launched"`
	Failed          int        `json:"failed"`
	Region          string     `json:"region"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
	EstimatedCost   float64    `json:"estimated_cost,omitempty"`
	DurationSeconds int        `json:"duration_seconds,omitempty"`
}

// SweepDetailInfo represents detailed sweep information
type SweepDetailInfo struct {
	SweepID         string              `json:"sweep_id"`
	SweepName       string              `json:"sweep_name"`
	Status          string              `json:"status"`
	TotalParams     int                 `json:"total_params"`
	Launched        int                 `json:"launched"`
	Failed          int                 `json:"failed"`
	Region          string              `json:"region"`
	CreatedAt       time.Time           `json:"created_at"`
	UpdatedAt       time.Time           `json:"updated_at"`
	CompletedAt     *time.Time          `json:"completed_at,omitempty"`
	EstimatedCost   float64             `json:"estimated_cost,omitempty"`
	DurationSeconds int                 `json:"duration_seconds,omitempty"`
	MaxConcurrent   int                 `json:"max_concurrent"`
	LaunchDelay     string              `json:"launch_delay"`
	NextToLaunch    int                 `json:"next_to_launch"`
	CancelRequested bool                `json:"cancel_requested"`
	Instances       []SweepInstanceInfo `json:"instances"`
}

// SweepInstanceInfo represents instance info within a sweep
type SweepInstanceInfo struct {
	Index         int        `json:"index"`
	Region        string     `json:"region"`
	InstanceID    string     `json:"instance_id"`
	RequestedType string     `json:"requested_type,omitempty"` // Pattern specified
	ActualType    string     `json:"actual_type,omitempty"`    // Type actually launched
	State         string     `json:"state"`
	LaunchedAt    time.Time  `json:"launched_at"`
	TerminatedAt  *time.Time `json:"terminated_at,omitempty"`
	ErrorMessage  string     `json:"error_message,omitempty"`
}

// DynamoDB structures

// UserAccountRecord represents a record in the spawn-user-accounts DynamoDB table
type UserAccountRecord struct {
	UserID        string `dynamodbav:"user_id"`
	CliIamArn     string `dynamodbav:"cli_iam_arn,omitempty"`
	IdentityType  string `dynamodbav:"identity_type,omitempty"`
	AWSAccountID  string `dynamodbav:"aws_account_id"`
	AccountBase36 string `dynamodbav:"account_base36"`
	Email         string `dynamodbav:"email,omitempty"`
	LinkedAt      string `dynamodbav:"linked_at,omitempty"`
	CreatedAt     string `dynamodbav:"created_at"`
	LastAccess    string `dynamodbav:"last_access"`
}

// TeamRecord represents a record in the spawn-teams DynamoDB table
type TeamRecord struct {
	TeamID      string `dynamodbav:"team_id" json:"team_id"`
	TeamName    string `dynamodbav:"team_name" json:"team_name"`
	OwnerARN    string `dynamodbav:"owner_arn" json:"owner_arn"`
	Description string `dynamodbav:"description" json:"description,omitempty"`
	CreatedAt   string `dynamodbav:"created_at" json:"created_at"`
	MemberCount int    `dynamodbav:"member_count" json:"member_count"`
}

// TeamMemberRecord represents a record in the spawn-team-memberships DynamoDB table
type TeamMemberRecord struct {
	TeamID    string `dynamodbav:"team_id" json:"team_id"`
	MemberARN string `dynamodbav:"member_arn" json:"member_arn"`
	Role      string `dynamodbav:"role" json:"role"`
	JoinedAt  string `dynamodbav:"joined_at" json:"joined_at"`
	InvitedBy string `dynamodbav:"invited_by" json:"invited_by"`
}

// SweepRecord represents a record in the spawn-sweep-orchestration DynamoDB table
type SweepRecord struct {
	SweepID                string          `dynamodbav:"sweep_id"`
	SweepName              string          `dynamodbav:"sweep_name"`
	UserID                 string          `dynamodbav:"user_id"`
	CreatedAt              string          `dynamodbav:"created_at"`
	UpdatedAt              string          `dynamodbav:"updated_at"`
	CompletedAt            string          `dynamodbav:"completed_at,omitempty"`
	S3ParamsKey            string          `dynamodbav:"s3_params_key"`
	MaxConcurrent          int             `dynamodbav:"max_concurrent"`
	MaxConcurrentPerRegion int             `dynamodbav:"max_concurrent_per_region,omitempty"`
	LaunchDelay            string          `dynamodbav:"launch_delay"`
	TotalParams            int             `dynamodbav:"total_params"`
	Region                 string          `dynamodbav:"region"`
	AWSAccountID           string          `dynamodbav:"aws_account_id"`
	Status                 string          `dynamodbav:"status"`
	CancelRequested        bool            `dynamodbav:"cancel_requested"`
	EstimatedCost          float64         `dynamodbav:"estimated_cost,omitempty"`
	Budget                 float64         `dynamodbav:"budget,omitempty"`
	NextToLaunch           int             `dynamodbav:"next_to_launch"`
	Launched               int             `dynamodbav:"launched"`
	Failed                 int             `dynamodbav:"failed"`
	ErrorMessage           string          `dynamodbav:"error_message,omitempty"`
	Instances              []SweepInstance `dynamodbav:"instances"`

	// Team sharing
	TeamID string `dynamodbav:"team_id" json:"team_id,omitempty"`

	// Multi-region support
	MultiRegion      bool                       `dynamodbav:"multi_region"`
	RegionStatus     map[string]*RegionProgress `dynamodbav:"region_status,omitempty"`
	DistributionMode string                     `dynamodbav:"distribution_mode,omitempty"` // "balanced" or "opportunistic"
}

// RegionProgress tracks per-region sweep progress
type RegionProgress struct {
	Launched     int   `dynamodbav:"launched"`
	Failed       int   `dynamodbav:"failed"`
	ActiveCount  int   `dynamodbav:"active_count"`
	NextToLaunch []int `dynamodbav:"next_to_launch"`
}

// SweepInstance tracks individual instance state in DynamoDB
type SweepInstance struct {
	Index        int    `dynamodbav:"index"`
	Region       string `dynamodbav:"region"`
	InstanceID   string `dynamodbav:"instance_id"`
	State        string `dynamodbav:"state"`
	LaunchedAt   string `dynamodbav:"launched_at"`
	TerminatedAt string `dynamodbav:"terminated_at,omitempty"`
	ErrorMessage string `dynamodbav:"error_message,omitempty"`
}

// Autoscale API Response structures

// AutoScaleGroupsAPIResponse is the response for /api/autoscale-groups
type AutoScaleGroupsAPIResponse struct {
	Success         bool                 `json:"success"`
	Message         string               `json:"message,omitempty"`
	Error           string               `json:"error,omitempty"`
	TotalGroups     int                  `json:"total_groups"`
	AutoScaleGroups []AutoScaleGroupInfo `json:"autoscale_groups"`
}

// AutoScaleGroupDetailAPIResponse is the response for /api/autoscale-groups/{id}
type AutoScaleGroupDetailAPIResponse struct {
	Success bool            `json:"success"`
	Message string          `json:"message,omitempty"`
	Error   string          `json:"error,omitempty"`
	Group   GroupDetailInfo `json:"group"`
}

// CostSummaryAPIResponse is the response for /api/cost-summary
type CostSummaryAPIResponse struct {
	Success bool        `json:"success"`
	Message string      `json:"message,omitempty"`
	Error   string      `json:"error,omitempty"`
	Cost    CostSummary `json:"cost"`
}

// AutoScaleGroupInfo represents autoscale group summary information
type AutoScaleGroupInfo struct {
	AutoScaleGroupID string    `json:"autoscale_group_id"`
	GroupName        string    `json:"group_name"`
	Status           string    `json:"status"`
	DesiredCapacity  int       `json:"desired_capacity"`
	CurrentCapacity  int       `json:"current_capacity"`
	MinCapacity      int       `json:"min_capacity"`
	MaxCapacity      int       `json:"max_capacity"`
	PolicyType       string    `json:"policy_type"`
	LastScaleEvent   time.Time `json:"last_scale_event"`
	CreatedAt        time.Time `json:"created_at"`
	TeamID           string    `dynamodbav:"team_id" json:"team_id,omitempty"`
}

// GroupDetailInfo represents detailed autoscale group information
type GroupDetailInfo struct {
	AutoScaleGroupInfo
	HealthyCount        int                 `json:"healthy_count"`
	UnhealthyCount      int                 `json:"unhealthy_count"`
	PendingCount        int                 `json:"pending_count"`
	QueueDepth          *int                `json:"queue_depth,omitempty"`
	MetricValue         *float64            `json:"metric_value,omitempty"`
	NextScheduledAction *string             `json:"next_scheduled_action,omitempty"`
	Instances           []GroupInstanceInfo `json:"instances"`
}

// GroupInstanceInfo represents instance info within an autoscale group
type GroupInstanceInfo struct {
	InstanceID       string    `json:"instance_id"`
	State            string    `json:"state"`
	HealthStatus     string    `json:"health_status"`
	HeartbeatAge     float64   `json:"heartbeat_age_seconds"`
	SpotInterruption bool      `json:"spot_interruption"`
	LaunchedAt       time.Time `json:"launched_at"`
}

// CostSummary represents current cost information
type CostSummary struct {
	TotalHourlyCost      float64             `json:"total_hourly_cost"`
	EstimatedMonthlyCost float64             `json:"estimated_monthly_cost"`
	InstanceCount        int                 `json:"instance_count"`
	BreakdownByType      map[string]TypeCost `json:"breakdown_by_type"`
}

// TypeCost represents cost breakdown by instance type
type TypeCost struct {
	Count      int     `json:"count"`
	HourlyCost float64 `json:"hourly_cost"`
}
