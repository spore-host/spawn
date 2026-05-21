package autoscaler

import (
	"context"
	"testing"
	"time"
)

func TestGetDefaultDrainConfig(t *testing.T) {
	config := GetDefaultDrainConfig()

	if config.Enabled {
		t.Error("default drain should be disabled")
	}

	if config.TimeoutSeconds != 300 {
		t.Errorf("default timeout: got %d, want 300", config.TimeoutSeconds)
	}

	if config.CheckInterval != 30*time.Second {
		t.Errorf("default check interval: got %v, want 30s", config.CheckInterval)
	}
}

func TestDrainManager_hasActiveWork(t *testing.T) {
	tests := []struct {
		name           string
		hasDynamoDB    bool
		hasRegistry    bool
		wantActiveWork bool
		wantErr        bool
	}{
		{
			name:           "no registry configured",
			hasDynamoDB:    false,
			hasRegistry:    false,
			wantActiveWork: false,
			wantErr:        false,
		},
		{
			name:           "registry table name empty",
			hasDynamoDB:    true,
			hasRegistry:    false,
			wantActiveWork: false,
			wantErr:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dm := &DrainManager{}
			if tt.hasRegistry {
				dm.registryTable = "test-registry"
			}

			hasWork, err := dm.hasActiveWork(context.Background(), "i-test")
			if (err != nil) != tt.wantErr {
				t.Errorf("got error %v, wantErr %v", err, tt.wantErr)
			}

			if hasWork != tt.wantActiveWork {
				t.Errorf("got active work %v, want %v", hasWork, tt.wantActiveWork)
			}
		})
	}
}

func TestGetDefaultDrainConfig_AllFields(t *testing.T) {
	config := GetDefaultDrainConfig()

	if config.Enabled {
		t.Error("default drain should be disabled")
	}

	if config.TimeoutSeconds != 300 {
		t.Errorf("default timeout: got %d, want 300", config.TimeoutSeconds)
	}

	if config.CheckInterval != 30*time.Second {
		t.Errorf("default check interval: got %v, want 30s", config.CheckInterval)
	}

	if config.HeartbeatStaleAfter != 300 {
		t.Errorf("default heartbeat stale: got %d, want 300", config.HeartbeatStaleAfter)
	}

	if config.GracePeriodSeconds != 30 {
		t.Errorf("default grace period: got %d, want 30", config.GracePeriodSeconds)
	}
}
