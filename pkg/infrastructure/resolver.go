package infrastructure

import (
	"fmt"

	"github.com/spore-host/spawn/pkg/config"
)

// Resolver resolves infrastructure resource names and ARNs
// It handles both shared (spore-host-infra account) and self-hosted modes
type Resolver struct {
	config    *config.InfrastructureConfig
	region    string
	accountID string
}

// NewResolver creates a new infrastructure resolver
func NewResolver(cfg *config.InfrastructureConfig, region, accountID string) *Resolver {
	return &Resolver{
		config:    cfg,
		region:    region,
		accountID: accountID,
	}
}

// DynamoDB resource resolution

// GetSchedulesTable returns the DynamoDB table name for schedules
func (r *Resolver) GetSchedulesTable() string {
	if r.config.DynamoDB.SchedulesTable != "" {
		return r.config.DynamoDB.SchedulesTable
	}
	return "spawn-schedules" // Default for shared infrastructure
}

// GetSweepOrchestrationTable returns the DynamoDB table name for sweep orchestration
func (r *Resolver) GetSweepOrchestrationTable() string {
	if r.config.DynamoDB.SweepOrchestrationTable != "" {
		return r.config.DynamoDB.SweepOrchestrationTable
	}
	return "spawn-sweep-orchestration" // Default for shared infrastructure
}

// GetAlertsTable returns the DynamoDB table name for alerts
func (r *Resolver) GetAlertsTable() string {
	if r.config.DynamoDB.AlertsTable != "" {
		return r.config.DynamoDB.AlertsTable
	}
	return "spawn-alerts" // Default for shared infrastructure
}

// GetAlertHistoryTable returns the DynamoDB table name for alert history
func (r *Resolver) GetAlertHistoryTable() string {
	if r.config.DynamoDB.AlertHistoryTable != "" {
		return r.config.DynamoDB.AlertHistoryTable
	}
	return "spawn-alert-history" // Default for shared infrastructure
}

// S3 resource resolution

// GetBinariesBucket returns the S3 bucket name for spawn binaries
func (r *Resolver) GetBinariesBucket() string {
	prefix := r.config.S3.BinariesBucketPrefix
	if prefix == "" {
		prefix = "spawn-binaries"
	}
	return fmt.Sprintf("%s-%s", prefix, r.region)
}

// GetSchedulesBucket returns the S3 bucket name for schedules
func (r *Resolver) GetSchedulesBucket() string {
	prefix := r.config.S3.SchedulesBucketPrefix
	if prefix == "" {
		prefix = "spawn-schedules"
	}
	return fmt.Sprintf("%s-%s", prefix, r.region)
}

// Lambda resource resolution

// GetSchedulerHandlerARN returns the Lambda function ARN for scheduler handler
func (r *Resolver) GetSchedulerHandlerARN() string {
	if r.config.Lambda.SchedulerHandlerARN != "" {
		return r.config.Lambda.SchedulerHandlerARN
	}
	// Default to spore-host-infra account (966362334030)
	return fmt.Sprintf("arn:aws:lambda:%s:966362334030:function:spawn-scheduler-handler", r.region)
}

// GetSweepOrchestratorARN returns the Lambda function ARN for sweep orchestrator
func (r *Resolver) GetSweepOrchestratorARN() string {
	if r.config.Lambda.SweepOrchestratorARN != "" {
		return r.config.Lambda.SweepOrchestratorARN
	}
	// Default to spore-host-infra account
	return fmt.Sprintf("arn:aws:lambda:%s:966362334030:function:spawn-sweep-orchestrator", r.region)
}

// GetAlertHandlerARN returns the Lambda function ARN for alert handler
func (r *Resolver) GetAlertHandlerARN() string {
	if r.config.Lambda.AlertHandlerARN != "" {
		return r.config.Lambda.AlertHandlerARN
	}
	// Default to spore-host-infra account
	return fmt.Sprintf("arn:aws:lambda:%s:966362334030:function:spawn-alert-handler", r.region)
}

// GetDashboardAPIARN returns the Lambda function ARN for dashboard API
func (r *Resolver) GetDashboardAPIARN() string {
	if r.config.Lambda.DashboardAPIARN != "" {
		return r.config.Lambda.DashboardAPIARN
	}
	// Default to spore-host-infra account
	return fmt.Sprintf("arn:aws:lambda:%s:966362334030:function:spawn-dashboard-api", r.region)
}

// CloudWatch resource resolution

// GetLogGroup returns the CloudWatch log group name for a service
func (r *Resolver) GetLogGroup(serviceName string) string {
	prefix := r.config.CloudWatch.LogGroupPrefix
	if prefix == "" {
		prefix = "/spawn"
	}
	return fmt.Sprintf("%s/%s", prefix, serviceName)
}

// Utility methods

// IsSelfHosted returns true if using self-hosted infrastructure
func (r *Resolver) IsSelfHosted() bool {
	return r.config.IsSelfHosted()
}

// GetAccountID returns the AWS account ID
func (r *Resolver) GetAccountID() string {
	return r.accountID
}

// GetRegion returns the AWS region
func (r *Resolver) GetRegion() string {
	return r.region
}

// GetInfrastructureAccount returns the infrastructure account ID
// For shared mode, returns spore-host-infra (966362334030)
// For self-hosted mode, returns the current account ID
func (r *Resolver) GetInfrastructureAccount() string {
	if r.IsSelfHosted() {
		return r.accountID
	}
	return "966362334030" // spore-host-infra account
}

// GetResourceSummary returns a summary of resolved resource names for display
func (r *Resolver) GetResourceSummary() map[string]string {
	return map[string]string{
		"mode":                      string(r.config.Mode),
		"schedules_table":           r.GetSchedulesTable(),
		"sweep_orchestration_table": r.GetSweepOrchestrationTable(),
		"alerts_table":              r.GetAlertsTable(),
		"alert_history_table":       r.GetAlertHistoryTable(),
		"binaries_bucket":           r.GetBinariesBucket(),
		"schedules_bucket":          r.GetSchedulesBucket(),
		"scheduler_handler_arn":     r.GetSchedulerHandlerARN(),
		"sweep_orchestrator_arn":    r.GetSweepOrchestratorARN(),
		"alert_handler_arn":         r.GetAlertHandlerARN(),
		"dashboard_api_arn":         r.GetDashboardAPIARN(),
	}
}
