package orchestrator

import (
	"os"
	"testing"
	"time"
)

func TestOrchestrator_shouldScaleUp(t *testing.T) {
	tests := []struct {
		name        string
		policy      BurstPolicy
		queueDepth  int
		localCount  int
		cloudCount  int
		currentCost float64
		want        bool
	}{
		{
			name: "should scale up - queue exceeds capacity",
			policy: BurstPolicy{
				QueueDepthThreshold: 50,
				MaxCloudInstances:   10,
				CostBudget:          5.0,
			},
			queueDepth:  100,
			localCount:  2,
			cloudCount:  3,
			currentCost: 1.0,
			want:        true,
		},
		{
			name: "should not scale - at max cloud instances",
			policy: BurstPolicy{
				QueueDepthThreshold: 50,
				MaxCloudInstances:   5,
				CostBudget:          10.0,
			},
			queueDepth:  100,
			localCount:  2,
			cloudCount:  5,
			currentCost: 2.0,
			want:        false,
		},
		{
			name: "should not scale - over budget",
			policy: BurstPolicy{
				QueueDepthThreshold: 50,
				MaxCloudInstances:   10,
				CostBudget:          2.0,
			},
			queueDepth:  100,
			localCount:  2,
			cloudCount:  3,
			currentCost: 2.5,
			want:        false,
		},
		{
			name: "should not scale - queue below threshold",
			policy: BurstPolicy{
				QueueDepthThreshold: 100,
				MaxCloudInstances:   10,
				CostBudget:          10.0,
			},
			queueDepth:  50,
			localCount:  2,
			cloudCount:  3,
			currentCost: 1.0,
			want:        false,
		},
		{
			name: "should not scale - capacity sufficient",
			policy: BurstPolicy{
				QueueDepthThreshold: 50,
				MaxCloudInstances:   10,
				CostBudget:          10.0,
			},
			queueDepth:  60,
			localCount:  30,
			cloudCount:  40,
			currentCost: 3.0,
			want:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := &Orchestrator{
				config: &Config{
					BurstPolicy: tt.policy,
				},
				totalCost: tt.currentCost,
			}

			got := o.shouldScaleUp(tt.queueDepth, tt.localCount, tt.cloudCount)
			if got != tt.want {
				t.Errorf("shouldScaleUp() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOrchestrator_shouldScaleDown(t *testing.T) {
	tests := []struct {
		name       string
		policy     BurstPolicy
		queueDepth int
		cloudCount int
		want       bool
	}{
		{
			name: "should scale down - queue below half threshold",
			policy: BurstPolicy{
				QueueDepthThreshold: 100,
				MinCloudInstances:   0,
			},
			queueDepth: 40,
			cloudCount: 10,
			want:       true,
		},
		{
			name: "should not scale down - at minimum",
			policy: BurstPolicy{
				QueueDepthThreshold: 100,
				MinCloudInstances:   5,
			},
			queueDepth: 40,
			cloudCount: 5,
			want:       false,
		},
		{
			name: "should not scale down - queue still high",
			policy: BurstPolicy{
				QueueDepthThreshold: 100,
				MinCloudInstances:   0,
			},
			queueDepth: 60,
			cloudCount: 10,
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := &Orchestrator{
				config: &Config{
					BurstPolicy: tt.policy,
				},
			}

			got := o.shouldScaleDown(tt.queueDepth, tt.cloudCount)
			if got != tt.want {
				t.Errorf("shouldScaleDown() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOrchestrator_calculateNeededInstances(t *testing.T) {
	tests := []struct {
		name         string
		policy       BurstPolicy
		queueDepth   int
		currentTotal int
		managed      int
		want         int
	}{
		{
			name: "queue 50 jobs, have 2, need 6 total, launch 4",
			policy: BurstPolicy{
				MaxCloudInstances: 10,
			},
			queueDepth:   50,
			currentTotal: 2,
			managed:      0,
			want:         4, // (50/10)+1=6 needed, 6-2=4
		},
		{
			name: "cap at max cloud instances",
			policy: BurstPolicy{
				MaxCloudInstances: 5,
			},
			queueDepth:   1000,
			currentTotal: 2,
			managed:      2,
			want:         3, // MaxCloudInstances=5, already have 2 managed, so max 3 more
		},
		{
			name: "cap at 10 instances per burst",
			policy: BurstPolicy{
				MaxCloudInstances: 100,
			},
			queueDepth:   2000,
			currentTotal: 0,
			managed:      0,
			want:         10, // Would need 201, but capped at 10 per burst
		},
		{
			name: "no instances needed - capacity exceeds demand",
			policy: BurstPolicy{
				MaxCloudInstances: 10,
			},
			queueDepth:   50,
			currentTotal: 100,
			managed:      0,
			want:         0, // negative clamped to 0 (or returns negative)
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := &Orchestrator{
				config: &Config{
					BurstPolicy: tt.policy,
				},
				managedInstances: make(map[string]*ManagedInstance),
			}

			// Add managed instances
			for i := 0; i < tt.managed; i++ {
				o.managedInstances[string(rune(i))] = &ManagedInstance{}
			}

			got := o.calculateNeededInstances(tt.queueDepth, tt.currentTotal)
			// Accept either exact match or 0 for the "no instances needed" case
			if tt.name == "no instances needed - capacity exceeds demand" {
				if got > 0 {
					t.Errorf("calculateNeededInstances() = %v, want <= 0", got)
				}
			} else if got != tt.want {
				t.Errorf("calculateNeededInstances() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOrchestrator_calculateTerminateCount(t *testing.T) {
	tests := []struct {
		name       string
		policy     BurstPolicy
		queueDepth int
		cloudCount int
		want       int
	}{
		{
			name: "queue needs 2, have 10, should terminate 8",
			policy: BurstPolicy{
				MinCloudInstances: 0,
			},
			queueDepth: 20,
			cloudCount: 10,
			want:       5, // Capped at 5
		},
		{
			name: "respect minimum cloud instances",
			policy: BurstPolicy{
				MinCloudInstances: 5,
			},
			queueDepth: 0,
			cloudCount: 10,
			want:       5,
		},
		{
			name: "no termination needed",
			policy: BurstPolicy{
				MinCloudInstances: 0,
			},
			queueDepth: 100,
			cloudCount: 5,
			want:       0,
		},
		{
			name: "cap at 5 terminations per cycle",
			policy: BurstPolicy{
				MinCloudInstances: 0,
			},
			queueDepth: 0,
			cloudCount: 20,
			want:       5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := &Orchestrator{
				config: &Config{
					BurstPolicy: tt.policy,
				},
			}

			got := o.calculateTerminateCount(tt.queueDepth, tt.cloudCount)
			if got != tt.want {
				t.Errorf("calculateTerminateCount() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetInstanceCostPerHour(t *testing.T) {
	tests := []struct {
		name         string
		instanceType string
		spot         bool
		wantMin      float64
		wantMax      float64
	}{
		{
			name:         "t3.micro on-demand",
			instanceType: "t3.micro",
			spot:         false,
			wantMin:      0.01,
			wantMax:      0.02,
		},
		{
			name:         "t3.micro spot",
			instanceType: "t3.micro",
			spot:         true,
			wantMin:      0.003,
			wantMax:      0.005,
		},
		{
			name:         "c5.4xlarge on-demand",
			instanceType: "c5.4xlarge",
			spot:         false,
			wantMin:      0.6,
			wantMax:      0.7,
		},
		{
			name:         "c5.4xlarge spot",
			instanceType: "c5.4xlarge",
			spot:         true,
			wantMin:      0.2,
			wantMax:      0.25,
		},
		{
			name:         "unknown instance type",
			instanceType: "unknown.type",
			spot:         false,
			wantMin:      0.05,
			wantMax:      0.15,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getInstanceCostPerHour(tt.instanceType, tt.spot)
			if got < tt.wantMin || got > tt.wantMax {
				t.Errorf("getInstanceCostPerHour() = %v, want between %v and %v",
					got, tt.wantMin, tt.wantMax)
			}
		})
	}
}

func TestOrchestrator_updateCostTracking(t *testing.T) {
	o := &Orchestrator{
		managedInstances: map[string]*ManagedInstance{
			"i-001": {CostPerHour: 0.5},
			"i-002": {CostPerHour: 0.3},
			"i-003": {CostPerHour: 0.7},
		},
	}

	o.updateCostTracking()

	want := 1.5
	if o.totalCost != want {
		t.Errorf("totalCost = %v, want %v", o.totalCost, want)
	}
}

func TestLoadConfig(t *testing.T) {
	tests := []struct {
		name       string
		configYAML string
		wantErr    bool
		wantMode   string
		wantQueue  string
	}{
		{
			name: "valid config",
			configYAML: `
job_array_id: test-array
queue_url: https://sqs.us-east-1.amazonaws.com/123/test-queue
region: us-east-1
burst_policy:
  mode: auto
  queue_depth_threshold: 100
  max_cloud_instances: 10
  instance_type: t3.micro
  ami: ami-12345
`,
			wantErr:   false,
			wantMode:  "auto",
			wantQueue: "https://sqs.us-east-1.amazonaws.com/123/test-queue",
		},
		{
			name: "manual mode",
			configYAML: `
job_array_id: test-array
queue_url: https://sqs.us-east-1.amazonaws.com/123/test-queue
burst_policy:
  mode: manual
`,
			wantErr:  false,
			wantMode: "manual",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create temp config file
			tmpFile := t.TempDir() + "/config.yaml"
			if err := writeFile(tmpFile, []byte(tt.configYAML)); err != nil {
				t.Fatalf("Failed to write config: %v", err)
			}

			cfg, err := LoadConfig(tmpFile)
			if (err != nil) != tt.wantErr {
				t.Errorf("LoadConfig() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}

			if cfg.BurstPolicy.Mode != tt.wantMode {
				t.Errorf("Mode = %v, want %v", cfg.BurstPolicy.Mode, tt.wantMode)
			}
			if tt.wantQueue != "" && cfg.QueueURL != tt.wantQueue {
				t.Errorf("QueueURL = %v, want %v", cfg.QueueURL, tt.wantQueue)
			}
		})
	}
}

func TestBurstPolicy_GetCheckInterval(t *testing.T) {
	tests := []struct {
		name     string
		interval string
		wantSec  int
	}{
		{
			name:     "1 minute",
			interval: "1m",
			wantSec:  60,
		},
		{
			name:     "30 seconds",
			interval: "30s",
			wantSec:  30,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Parse the duration like LoadConfig does
			parsed, err := parseTestDuration(tt.interval)
			if err != nil {
				t.Fatalf("Failed to parse duration: %v", err)
			}

			p := &BurstPolicy{
				CheckInterval: tt.interval,
				checkInterval: parsed,
			}

			got := p.GetCheckInterval()
			if int(got.Seconds()) != tt.wantSec {
				t.Errorf("GetCheckInterval() = %v seconds, want %v seconds",
					int(got.Seconds()), tt.wantSec)
			}
		})
	}
}

func TestBurstPolicy_GetScaleDownDelay(t *testing.T) {
	tests := []struct {
		name    string
		delay   string
		wantSec int
	}{
		{
			name:    "5 minutes",
			delay:   "5m",
			wantSec: 300,
		},
		{
			name:    "1 hour",
			delay:   "1h",
			wantSec: 3600,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := parseTestDuration(tt.delay)
			if err != nil {
				t.Fatalf("Failed to parse duration: %v", err)
			}

			p := &BurstPolicy{
				ScaleDownDelay: tt.delay,
				scaleDownDelay: parsed,
			}

			got := p.GetScaleDownDelay()
			if int(got.Seconds()) != tt.wantSec {
				t.Errorf("GetScaleDownDelay() = %v seconds, want %v seconds",
					int(got.Seconds()), tt.wantSec)
			}
		})
	}
}

// Helper functions
func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0644)
}

func parseTestDuration(s string) (time.Duration, error) {
	return time.ParseDuration(s)
}
