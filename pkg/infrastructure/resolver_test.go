package infrastructure

import (
	"testing"

	"github.com/spore-host/spawn/pkg/config"
)

func TestResolver_DynamoDBTables(t *testing.T) {
	tests := []struct {
		name       string
		cfg        *config.InfrastructureConfig
		wantTables map[string]string
	}{
		{
			name: "default shared infrastructure",
			cfg: &config.InfrastructureConfig{
				Mode:     config.InfrastructureModeShared,
				DynamoDB: config.DynamoDBConfig{},
			},
			wantTables: map[string]string{
				"schedules":           "spawn-schedules",
				"sweep_orchestration": "spawn-sweep-orchestration",
				"alerts":              "spawn-alerts",
				"alert_history":       "spawn-alert-history",
			},
		},
		{
			name: "custom self-hosted table names",
			cfg: &config.InfrastructureConfig{
				Mode: config.InfrastructureModeSelfHosted,
				DynamoDB: config.DynamoDBConfig{
					SchedulesTable:          "my-spawn-schedules",
					SweepOrchestrationTable: "my-spawn-sweeps",
					AlertsTable:             "my-spawn-alerts",
					AlertHistoryTable:       "my-spawn-alert-history",
				},
			},
			wantTables: map[string]string{
				"schedules":           "my-spawn-schedules",
				"sweep_orchestration": "my-spawn-sweeps",
				"alerts":              "my-spawn-alerts",
				"alert_history":       "my-spawn-alert-history",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolver := NewResolver(tt.cfg, "us-east-1", "123456789012")

			if got := resolver.GetSchedulesTable(); got != tt.wantTables["schedules"] {
				t.Errorf("GetSchedulesTable() = %v, want %v", got, tt.wantTables["schedules"])
			}
			if got := resolver.GetSweepOrchestrationTable(); got != tt.wantTables["sweep_orchestration"] {
				t.Errorf("GetSweepOrchestrationTable() = %v, want %v", got, tt.wantTables["sweep_orchestration"])
			}
			if got := resolver.GetAlertsTable(); got != tt.wantTables["alerts"] {
				t.Errorf("GetAlertsTable() = %v, want %v", got, tt.wantTables["alerts"])
			}
			if got := resolver.GetAlertHistoryTable(); got != tt.wantTables["alert_history"] {
				t.Errorf("GetAlertHistoryTable() = %v, want %v", got, tt.wantTables["alert_history"])
			}
		})
	}
}

func TestResolver_S3Buckets(t *testing.T) {
	tests := []struct {
		name        string
		cfg         *config.InfrastructureConfig
		region      string
		wantBuckets map[string]string
	}{
		{
			name: "default shared infrastructure",
			cfg: &config.InfrastructureConfig{
				Mode: config.InfrastructureModeShared,
				S3:   config.S3Config{},
			},
			region: "us-east-1",
			wantBuckets: map[string]string{
				"binaries":  "spawn-binaries-us-east-1",
				"schedules": "spawn-schedules-us-east-1",
			},
		},
		{
			name: "custom self-hosted bucket prefixes",
			cfg: &config.InfrastructureConfig{
				Mode: config.InfrastructureModeSelfHosted,
				S3: config.S3Config{
					BinariesBucketPrefix:  "my-spawn-binaries",
					SchedulesBucketPrefix: "my-spawn-schedules",
				},
			},
			region: "us-west-2",
			wantBuckets: map[string]string{
				"binaries":  "my-spawn-binaries-us-west-2",
				"schedules": "my-spawn-schedules-us-west-2",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolver := NewResolver(tt.cfg, tt.region, "123456789012")

			if got := resolver.GetBinariesBucket(); got != tt.wantBuckets["binaries"] {
				t.Errorf("GetBinariesBucket() = %v, want %v", got, tt.wantBuckets["binaries"])
			}
			if got := resolver.GetSchedulesBucket(); got != tt.wantBuckets["schedules"] {
				t.Errorf("GetSchedulesBucket() = %v, want %v", got, tt.wantBuckets["schedules"])
			}
		})
	}
}

func TestResolver_LambdaFunctions(t *testing.T) {
	tests := []struct {
		name          string
		cfg           *config.InfrastructureConfig
		region        string
		accountID     string
		wantFunctions map[string]string
	}{
		{
			name: "default shared infrastructure (spore-host-infra account)",
			cfg: &config.InfrastructureConfig{
				Mode:   config.InfrastructureModeShared,
				Lambda: config.LambdaConfig{},
			},
			region:    "us-east-1",
			accountID: "123456789012",
			wantFunctions: map[string]string{
				"scheduler_handler":  "arn:aws:lambda:us-east-1:966362334030:function:spawn-scheduler-handler",
				"sweep_orchestrator": "arn:aws:lambda:us-east-1:966362334030:function:spawn-sweep-orchestrator",
				"alert_handler":      "arn:aws:lambda:us-east-1:966362334030:function:spawn-alert-handler",
				"dashboard_api":      "arn:aws:lambda:us-east-1:966362334030:function:spawn-dashboard-api",
			},
		},
		{
			name: "custom self-hosted function ARNs",
			cfg: &config.InfrastructureConfig{
				Mode: config.InfrastructureModeSelfHosted,
				Lambda: config.LambdaConfig{
					SchedulerHandlerARN:  "arn:aws:lambda:us-west-2:123456789012:function:my-scheduler",
					SweepOrchestratorARN: "arn:aws:lambda:us-west-2:123456789012:function:my-orchestrator",
					AlertHandlerARN:      "arn:aws:lambda:us-west-2:123456789012:function:my-alerts",
					DashboardAPIARN:      "arn:aws:lambda:us-west-2:123456789012:function:my-dashboard",
				},
			},
			region:    "us-west-2",
			accountID: "123456789012",
			wantFunctions: map[string]string{
				"scheduler_handler":  "arn:aws:lambda:us-west-2:123456789012:function:my-scheduler",
				"sweep_orchestrator": "arn:aws:lambda:us-west-2:123456789012:function:my-orchestrator",
				"alert_handler":      "arn:aws:lambda:us-west-2:123456789012:function:my-alerts",
				"dashboard_api":      "arn:aws:lambda:us-west-2:123456789012:function:my-dashboard",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolver := NewResolver(tt.cfg, tt.region, tt.accountID)

			if got := resolver.GetSchedulerHandlerARN(); got != tt.wantFunctions["scheduler_handler"] {
				t.Errorf("GetSchedulerHandlerARN() = %v, want %v", got, tt.wantFunctions["scheduler_handler"])
			}
			if got := resolver.GetSweepOrchestratorARN(); got != tt.wantFunctions["sweep_orchestrator"] {
				t.Errorf("GetSweepOrchestratorARN() = %v, want %v", got, tt.wantFunctions["sweep_orchestrator"])
			}
			if got := resolver.GetAlertHandlerARN(); got != tt.wantFunctions["alert_handler"] {
				t.Errorf("GetAlertHandlerARN() = %v, want %v", got, tt.wantFunctions["alert_handler"])
			}
			if got := resolver.GetDashboardAPIARN(); got != tt.wantFunctions["dashboard_api"] {
				t.Errorf("GetDashboardAPIARN() = %v, want %v", got, tt.wantFunctions["dashboard_api"])
			}
		})
	}
}

func TestResolver_CloudWatch(t *testing.T) {
	tests := []struct {
		name         string
		cfg          *config.InfrastructureConfig
		serviceName  string
		wantLogGroup string
	}{
		{
			name: "default log group prefix",
			cfg: &config.InfrastructureConfig{
				CloudWatch: config.CloudWatchConfig{},
			},
			serviceName:  "scheduler",
			wantLogGroup: "/spawn/scheduler",
		},
		{
			name: "custom log group prefix",
			cfg: &config.InfrastructureConfig{
				CloudWatch: config.CloudWatchConfig{
					LogGroupPrefix: "/my-spawn",
				},
			},
			serviceName:  "orchestrator",
			wantLogGroup: "/my-spawn/orchestrator",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolver := NewResolver(tt.cfg, "us-east-1", "123456789012")

			if got := resolver.GetLogGroup(tt.serviceName); got != tt.wantLogGroup {
				t.Errorf("GetLogGroup(%s) = %v, want %v", tt.serviceName, got, tt.wantLogGroup)
			}
		})
	}
}

func TestResolver_Helpers(t *testing.T) {
	tests := []struct {
		name             string
		cfg              *config.InfrastructureConfig
		region           string
		accountID        string
		wantSelfHosted   bool
		wantInfraAccount string
	}{
		{
			name: "shared infrastructure",
			cfg: &config.InfrastructureConfig{
				Mode: config.InfrastructureModeShared,
			},
			region:           "us-east-1",
			accountID:        "123456789012",
			wantSelfHosted:   false,
			wantInfraAccount: "966362334030", // spore-host-infra
		},
		{
			name: "self-hosted infrastructure",
			cfg: &config.InfrastructureConfig{
				Mode: config.InfrastructureModeSelfHosted,
			},
			region:           "us-west-2",
			accountID:        "987654321098",
			wantSelfHosted:   true,
			wantInfraAccount: "987654321098", // customer account
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolver := NewResolver(tt.cfg, tt.region, tt.accountID)

			if got := resolver.IsSelfHosted(); got != tt.wantSelfHosted {
				t.Errorf("IsSelfHosted() = %v, want %v", got, tt.wantSelfHosted)
			}
			if got := resolver.GetInfrastructureAccount(); got != tt.wantInfraAccount {
				t.Errorf("GetInfrastructureAccount() = %v, want %v", got, tt.wantInfraAccount)
			}
			if got := resolver.GetAccountID(); got != tt.accountID {
				t.Errorf("GetAccountID() = %v, want %v", got, tt.accountID)
			}
			if got := resolver.GetRegion(); got != tt.region {
				t.Errorf("GetRegion() = %v, want %v", got, tt.region)
			}
		})
	}
}

func TestResolver_GetResourceSummary(t *testing.T) {
	cfg := &config.InfrastructureConfig{
		Mode: config.InfrastructureModeShared,
	}
	resolver := NewResolver(cfg, "us-east-1", "123456789012")

	summary := resolver.GetResourceSummary()

	// Check that all expected keys are present
	expectedKeys := []string{
		"mode",
		"schedules_table",
		"sweep_orchestration_table",
		"alerts_table",
		"alert_history_table",
		"binaries_bucket",
		"schedules_bucket",
		"scheduler_handler_arn",
		"sweep_orchestrator_arn",
		"alert_handler_arn",
		"dashboard_api_arn",
	}

	for _, key := range expectedKeys {
		if _, exists := summary[key]; !exists {
			t.Errorf("GetResourceSummary() missing key: %s", key)
		}
	}

	// Check specific values
	if summary["mode"] != "shared" {
		t.Errorf("GetResourceSummary()[mode] = %v, want %v", summary["mode"], "shared")
	}
	if summary["schedules_table"] != "spawn-schedules" {
		t.Errorf("GetResourceSummary()[schedules_table] = %v, want %v", summary["schedules_table"], "spawn-schedules")
	}
}
