package config

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// InfrastructureMode represents where spawn infrastructure resources are located
type InfrastructureMode string

const (
	InfrastructureModeShared     InfrastructureMode = "shared"      // Use spore-host-infra account resources
	InfrastructureModeSelfHosted InfrastructureMode = "self-hosted" // Use customer's own AWS resources
)

// InfrastructureConfig holds configuration for AWS infrastructure resources
type InfrastructureConfig struct {
	// Mode specifies where infrastructure resources are located
	Mode InfrastructureMode `yaml:"mode"`

	// DynamoDB table names
	DynamoDB DynamoDBConfig `yaml:"dynamodb"`

	// S3 bucket configuration
	S3 S3Config `yaml:"s3"`

	// Lambda function ARNs
	Lambda LambdaConfig `yaml:"lambda"`

	// CloudWatch Logs configuration
	CloudWatch CloudWatchConfig `yaml:"cloudwatch"`
}

// DynamoDBConfig holds DynamoDB table names
type DynamoDBConfig struct {
	SchedulesTable          string `yaml:"schedules_table"`
	SweepOrchestrationTable string `yaml:"sweep_orchestration_table"`
	AlertsTable             string `yaml:"alerts_table"`
	AlertHistoryTable       string `yaml:"alert_history_table"`
	// Additional tables can be added here
}

// S3Config holds S3 bucket configuration
type S3Config struct {
	// Bucket name prefixes (region will be appended: {prefix}-{region})
	BinariesBucketPrefix  string `yaml:"binaries_bucket_prefix"`
	SchedulesBucketPrefix string `yaml:"schedules_bucket_prefix"`
	// Additional buckets can be added here
}

// LambdaConfig holds Lambda function ARNs
type LambdaConfig struct {
	SchedulerHandlerARN  string `yaml:"scheduler_handler_arn"`
	SweepOrchestratorARN string `yaml:"sweep_orchestrator_arn"`
	AlertHandlerARN      string `yaml:"alert_handler_arn"`
	DashboardAPIARN      string `yaml:"dashboard_api_arn"`
	// Additional functions can be added here
}

// CloudWatchConfig holds CloudWatch Logs configuration
type CloudWatchConfig struct {
	LogGroupPrefix string `yaml:"log_group_prefix"` // Prefix for log groups
}

// LoadInfrastructureConfig loads infrastructure configuration with precedence:
// 1. CLI flags (passed as parameters)
// 2. Environment variables
// 3. Config file
// 4. Defaults (shared infrastructure in spore-host-infra account)
func LoadInfrastructureConfig(ctx context.Context, flagMode string) (*InfrastructureConfig, error) {
	cfg := &InfrastructureConfig{
		Mode: InfrastructureModeShared,
		DynamoDB: DynamoDBConfig{
			SchedulesTable:          "spawn-schedules",
			SweepOrchestrationTable: "spawn-sweep-orchestration",
			AlertsTable:             "spawn-alerts",
			AlertHistoryTable:       "spawn-alert-history",
		},
		S3: S3Config{
			BinariesBucketPrefix:  "spawn-binaries",
			SchedulesBucketPrefix: "spawn-schedules",
		},
		Lambda: LambdaConfig{
			// Default ARNs point to spore-host-infra account (966362334030)
			// These will be constructed at runtime based on region
			SchedulerHandlerARN:  "", // Constructed: arn:aws:lambda:{region}:966362334030:function:spawn-scheduler-handler
			SweepOrchestratorARN: "", // Constructed: arn:aws:lambda:{region}:966362334030:function:spawn-sweep-orchestrator
			AlertHandlerARN:      "", // Constructed: arn:aws:lambda:{region}:966362334030:function:spawn-alert-handler
			DashboardAPIARN:      "", // Constructed: arn:aws:lambda:{region}:966362334030:function:spawn-dashboard-api
		},
		CloudWatch: CloudWatchConfig{
			LogGroupPrefix: "/spawn",
		},
	}

	// 3. Try config file
	fileConfig, err := loadFromFile()
	if err == nil && fileConfig != nil {
		if fileConfig.Infrastructure.Mode != "" {
			cfg.Mode = InfrastructureMode(fileConfig.Infrastructure.Mode)
		}

		// DynamoDB
		if fileConfig.Infrastructure.DynamoDB.SchedulesTable != "" {
			cfg.DynamoDB.SchedulesTable = fileConfig.Infrastructure.DynamoDB.SchedulesTable
		}
		if fileConfig.Infrastructure.DynamoDB.SweepOrchestrationTable != "" {
			cfg.DynamoDB.SweepOrchestrationTable = fileConfig.Infrastructure.DynamoDB.SweepOrchestrationTable
		}
		if fileConfig.Infrastructure.DynamoDB.AlertsTable != "" {
			cfg.DynamoDB.AlertsTable = fileConfig.Infrastructure.DynamoDB.AlertsTable
		}
		if fileConfig.Infrastructure.DynamoDB.AlertHistoryTable != "" {
			cfg.DynamoDB.AlertHistoryTable = fileConfig.Infrastructure.DynamoDB.AlertHistoryTable
		}

		// S3
		if fileConfig.Infrastructure.S3.BinariesBucketPrefix != "" {
			cfg.S3.BinariesBucketPrefix = fileConfig.Infrastructure.S3.BinariesBucketPrefix
		}
		if fileConfig.Infrastructure.S3.SchedulesBucketPrefix != "" {
			cfg.S3.SchedulesBucketPrefix = fileConfig.Infrastructure.S3.SchedulesBucketPrefix
		}

		// Lambda
		if fileConfig.Infrastructure.Lambda.SchedulerHandlerARN != "" {
			cfg.Lambda.SchedulerHandlerARN = fileConfig.Infrastructure.Lambda.SchedulerHandlerARN
		}
		if fileConfig.Infrastructure.Lambda.SweepOrchestratorARN != "" {
			cfg.Lambda.SweepOrchestratorARN = fileConfig.Infrastructure.Lambda.SweepOrchestratorARN
		}
		if fileConfig.Infrastructure.Lambda.AlertHandlerARN != "" {
			cfg.Lambda.AlertHandlerARN = fileConfig.Infrastructure.Lambda.AlertHandlerARN
		}
		if fileConfig.Infrastructure.Lambda.DashboardAPIARN != "" {
			cfg.Lambda.DashboardAPIARN = fileConfig.Infrastructure.Lambda.DashboardAPIARN
		}

		// CloudWatch
		if fileConfig.Infrastructure.CloudWatch.LogGroupPrefix != "" {
			cfg.CloudWatch.LogGroupPrefix = fileConfig.Infrastructure.CloudWatch.LogGroupPrefix
		}
	}

	// 2. Environment variables
	if envMode := os.Getenv("SPAWN_INFRASTRUCTURE_MODE"); envMode != "" {
		cfg.Mode = InfrastructureMode(strings.ToLower(envMode))
	}

	// DynamoDB env vars
	if envTable := os.Getenv("SPAWN_DYNAMODB_SCHEDULES_TABLE"); envTable != "" {
		cfg.DynamoDB.SchedulesTable = envTable
	}
	if envTable := os.Getenv("SPAWN_DYNAMODB_SWEEP_ORCHESTRATION_TABLE"); envTable != "" {
		cfg.DynamoDB.SweepOrchestrationTable = envTable
	}
	if envTable := os.Getenv("SPAWN_DYNAMODB_ALERTS_TABLE"); envTable != "" {
		cfg.DynamoDB.AlertsTable = envTable
	}
	if envTable := os.Getenv("SPAWN_DYNAMODB_ALERT_HISTORY_TABLE"); envTable != "" {
		cfg.DynamoDB.AlertHistoryTable = envTable
	}

	// S3 env vars
	if envBucket := os.Getenv("SPAWN_S3_BINARIES_BUCKET_PREFIX"); envBucket != "" {
		cfg.S3.BinariesBucketPrefix = envBucket
	}
	if envBucket := os.Getenv("SPAWN_S3_SCHEDULES_BUCKET_PREFIX"); envBucket != "" {
		cfg.S3.SchedulesBucketPrefix = envBucket
	}

	// Lambda env vars
	if envARN := os.Getenv("SPAWN_LAMBDA_SCHEDULER_HANDLER_ARN"); envARN != "" {
		cfg.Lambda.SchedulerHandlerARN = envARN
	}
	if envARN := os.Getenv("SPAWN_LAMBDA_SWEEP_ORCHESTRATOR_ARN"); envARN != "" {
		cfg.Lambda.SweepOrchestratorARN = envARN
	}
	if envARN := os.Getenv("SPAWN_LAMBDA_ALERT_HANDLER_ARN"); envARN != "" {
		cfg.Lambda.AlertHandlerARN = envARN
	}
	if envARN := os.Getenv("SPAWN_LAMBDA_DASHBOARD_API_ARN"); envARN != "" {
		cfg.Lambda.DashboardAPIARN = envARN
	}

	// CloudWatch env vars
	if envPrefix := os.Getenv("SPAWN_CLOUDWATCH_LOG_GROUP_PREFIX"); envPrefix != "" {
		cfg.CloudWatch.LogGroupPrefix = envPrefix
	}

	// 1. CLI flags (highest priority)
	if flagMode != "" {
		cfg.Mode = InfrastructureMode(strings.ToLower(flagMode))
	}

	return cfg, nil
}

// IsSelfHosted returns true if using self-hosted infrastructure
func (c *InfrastructureConfig) IsSelfHosted() bool {
	return c.Mode == InfrastructureModeSelfHosted
}

// GetModeDisplayName returns a human-readable name for the infrastructure mode
func (c *InfrastructureConfig) GetModeDisplayName() string {
	switch c.Mode {
	case InfrastructureModeShared:
		return "Shared (spore-host-infra account)"
	case InfrastructureModeSelfHosted:
		return "Self-Hosted (customer account)"
	default:
		return string(c.Mode)
	}
}

// GetInfrastructureConfigSource returns a human-readable description of where the infrastructure config came from
func GetInfrastructureConfigSource(ctx context.Context, flagMode string) string {
	if flagMode != "" {
		return "CLI flags"
	}

	if os.Getenv("SPAWN_INFRASTRUCTURE_MODE") != "" {
		return "environment variables"
	}

	fileConfig, err := loadFromFile()
	if err == nil && fileConfig != nil && fileConfig.Infrastructure.Mode != "" {
		return "config file (~/.spawn/config.yaml)"
	}

	return "default (shared infrastructure)"
}

// ValidateInfrastructureMode checks if the specified infrastructure mode is valid
func ValidateInfrastructureMode(mode string) error {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		return nil // Empty defaults to shared
	}

	validModes := []string{
		string(InfrastructureModeShared),
		string(InfrastructureModeSelfHosted),
	}

	for _, valid := range validModes {
		if mode == valid {
			return nil
		}
	}

	return fmt.Errorf("invalid infrastructure mode %q, valid modes: %s", mode, strings.Join(validModes, ", "))
}
