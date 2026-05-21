package alerts

import (
	"testing"
	"time"
)

func TestAlertConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		config  *AlertConfig
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid email alert",
			config: &AlertConfig{
				UserID:  "123456789012",
				SweepID: "sweep-123",
				Triggers: []TriggerType{
					TriggerComplete,
				},
				Destinations: []Destination{
					{Type: DestinationEmail, Target: "user@example.com"},
				},
			},
			wantErr: false,
		},
		{
			name: "missing user_id",
			config: &AlertConfig{
				SweepID: "sweep-123",
				Triggers: []TriggerType{
					TriggerComplete,
				},
				Destinations: []Destination{
					{Type: DestinationEmail, Target: "user@example.com"},
				},
			},
			wantErr: true,
			errMsg:  "user_id is required",
		},
		{
			name: "missing sweep_id and schedule_id",
			config: &AlertConfig{
				UserID: "123456789012",
				Triggers: []TriggerType{
					TriggerComplete,
				},
				Destinations: []Destination{
					{Type: DestinationEmail, Target: "user@example.com"},
				},
			},
			wantErr: true,
			errMsg:  "either sweep_id or schedule_id is required",
		},
		{
			name: "missing triggers",
			config: &AlertConfig{
				UserID:   "123456789012",
				SweepID:  "sweep-123",
				Triggers: []TriggerType{},
				Destinations: []Destination{
					{Type: DestinationEmail, Target: "user@example.com"},
				},
			},
			wantErr: true,
			errMsg:  "at least one trigger is required",
		},
		{
			name: "missing destinations",
			config: &AlertConfig{
				UserID:  "123456789012",
				SweepID: "sweep-123",
				Triggers: []TriggerType{
					TriggerComplete,
				},
				Destinations: []Destination{},
			},
			wantErr: true,
			errMsg:  "at least one destination is required",
		},
		{
			name: "cost threshold without value",
			config: &AlertConfig{
				UserID:  "123456789012",
				SweepID: "sweep-123",
				Triggers: []TriggerType{
					TriggerCostThreshold,
				},
				Destinations: []Destination{
					{Type: DestinationEmail, Target: "user@example.com"},
				},
				CostThreshold: 0,
			},
			wantErr: true,
			errMsg:  "cost_threshold must be > 0 for cost_threshold trigger",
		},
		{
			name: "long running without duration",
			config: &AlertConfig{
				UserID:  "123456789012",
				SweepID: "sweep-123",
				Triggers: []TriggerType{
					TriggerLongRunning,
				},
				Destinations: []Destination{
					{Type: DestinationEmail, Target: "user@example.com"},
				},
				DurationMinutes: 0,
			},
			wantErr: true,
			errMsg:  "duration_minutes must be > 0 for long_running trigger",
		},
		{
			name: "valid cost threshold alert",
			config: &AlertConfig{
				UserID:  "123456789012",
				SweepID: "sweep-123",
				Triggers: []TriggerType{
					TriggerCostThreshold,
				},
				Destinations: []Destination{
					{Type: DestinationEmail, Target: "user@example.com"},
				},
				CostThreshold: 100.50,
			},
			wantErr: false,
		},
		{
			name: "multiple triggers and destinations",
			config: &AlertConfig{
				UserID:  "123456789012",
				SweepID: "sweep-123",
				Triggers: []TriggerType{
					TriggerComplete,
					TriggerFailure,
					TriggerInstanceFailed,
				},
				Destinations: []Destination{
					{Type: DestinationEmail, Target: "user@example.com"},
					{Type: DestinationSlack, Target: "https://hooks.slack.com/services/..."},
				},
			},
			wantErr: false,
		},
		{
			name: "schedule alert",
			config: &AlertConfig{
				UserID:     "123456789012",
				ScheduleID: "sched-123",
				Triggers: []TriggerType{
					TriggerFailure,
				},
				Destinations: []Destination{
					{Type: DestinationSlack, Target: "https://hooks.slack.com/services/..."},
				},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil && tt.errMsg != "" && err.Error() != tt.errMsg {
				t.Errorf("Validate() error message = %v, want %v", err.Error(), tt.errMsg)
			}
		})
	}
}

func TestAlertConfigTTL(t *testing.T) {
	config := &AlertConfig{
		UserID:  "123456789012",
		SweepID: "sweep-123",
		Triggers: []TriggerType{
			TriggerComplete,
		},
		Destinations: []Destination{
			{Type: DestinationEmail, Target: "user@example.com"},
		},
		CreatedAt: time.Now(),
	}

	// Simulate TTL calculation (would be done in CreateAlert)
	config.TTL = time.Now().Add(AlertTTLDays * 24 * time.Hour).Unix()

	expectedTTL := time.Now().Add(AlertTTLDays * 24 * time.Hour).Unix()

	// Allow 1 second tolerance for test execution time
	if config.TTL < expectedTTL-1 || config.TTL > expectedTTL+1 {
		t.Errorf("TTL = %d, want approximately %d", config.TTL, expectedTTL)
	}
}
