package autoscaler

import (
	"time"

	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// Config holds dependencies for the autoscaler
type Config struct {
	EC2Client        *ec2.Client
	DynamoClient     *dynamodb.Client
	SQSClient        *sqs.Client
	CloudWatchClient *cloudwatch.Client
	TableName        string
	RegistryTable    string
	EC2RoleARN       string // Cross-account role ARN for EC2 operations (optional)
	ExternalID       string // External ID for assuming cross-account role (optional)
}

// AutoScaleGroup represents an auto-scaling job array configuration
type AutoScaleGroup struct {
	AutoScaleGroupID    string               `dynamodbav:"autoscale_group_id"`
	GroupName           string               `dynamodbav:"group_name"`
	JobArrayID          string               `dynamodbav:"job_array_id"`
	DesiredCapacity     int                  `dynamodbav:"desired_capacity"`
	MinCapacity         int                  `dynamodbav:"min_capacity"`
	MaxCapacity         int                  `dynamodbav:"max_capacity"`
	LaunchTemplate      LaunchTemplate       `dynamodbav:"launch_template"`
	Status              string               `dynamodbav:"status"` // "active", "paused", "terminated"
	CreatedAt           time.Time            `dynamodbav:"created_at"`
	UpdatedAt           time.Time            `dynamodbav:"updated_at"`
	LastScaleEvent      time.Time            `dynamodbav:"last_scale_event"`
	HealthCheckInterval time.Duration        `dynamodbav:"health_check_interval"`
	ReplacementStrategy string               `dynamodbav:"replacement_strategy"` // "immediate", "rolling"
	ScalingPolicy       *ScalingPolicy       `dynamodbav:"scaling_policy,omitempty"`
	MetricPolicy        *MetricScalingPolicy `dynamodbav:"metric_policy,omitempty"`
	ScalingState        *ScalingState        `dynamodbav:"scaling_state,omitempty"`
	DrainConfig         *DrainConfig         `dynamodbav:"drain_config,omitempty"`
	ScheduleConfig      *ScheduleConfig      `dynamodbav:"schedule_config,omitempty"`
}

// LaunchTemplate defines how to launch new instances
type LaunchTemplate struct {
	InstanceType       string
	AMI                string
	Spot               bool
	KeyName            string
	SubnetID           string
	SecurityGroups     []string
	IAMInstanceProfile string
	UserData           string
	Tags               map[string]string
}

// HealthStatus represents the health status of an instance
type HealthStatus struct {
	InstanceID       string
	EC2State         string
	HeartbeatAge     time.Duration
	SpotInterruption bool
	Healthy          bool
	Reason           string
}

// CapacityPlan describes capacity changes to execute
type CapacityPlan struct {
	CurrentCapacity int
	DesiredCapacity int
	HealthyCount    int
	UnhealthyCount  int
	PendingCount    int
	ToLaunch        int
	ToTerminate     []string
}
