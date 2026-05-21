package provider

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLocalProvider_GetIdentity(t *testing.T) {
	tests := []struct {
		name         string
		configYAML   string
		wantID       string
		wantRegion   string
		wantProvider string
		wantErr      bool
	}{
		{
			name: "valid config",
			configYAML: `
instance_id: test-local-01
region: us-west-2
account_id: "123456789012"
`,
			wantID:       "test-local-01",
			wantRegion:   "us-west-2",
			wantProvider: "local",
			wantErr:      false,
		},
		{
			name: "auto instance_id uses hostname",
			configYAML: `
instance_id: auto
region: local
`,
			wantID:       "", // Will be hostname, check non-empty
			wantRegion:   "local",
			wantProvider: "local",
			wantErr:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create temp config file
			tmpDir := t.TempDir()
			configPath := filepath.Join(tmpDir, "config.yaml")
			if err := os.WriteFile(configPath, []byte(tt.configYAML), 0644); err != nil {
				t.Fatalf("Failed to write config: %v", err)
			}

			// Set environment variable
			oldEnv := os.Getenv("SPAWN_CONFIG")
			if err := os.Setenv("SPAWN_CONFIG", configPath); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = os.Setenv("SPAWN_CONFIG", oldEnv) }()

			provider, err := NewLocalProvider(context.Background())
			if (err != nil) != tt.wantErr {
				t.Errorf("NewLocalProvider() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}

			identity, err := provider.GetIdentity(context.Background())
			if err != nil {
				t.Fatalf("GetIdentity() error = %v", err)
			}

			if tt.wantID != "" && identity.InstanceID != tt.wantID {
				t.Errorf("InstanceID = %v, want %v", identity.InstanceID, tt.wantID)
			}
			if tt.wantID == "" && identity.InstanceID == "" {
				t.Errorf("InstanceID should not be empty for auto mode")
			}
			if identity.Region != tt.wantRegion {
				t.Errorf("Region = %v, want %v", identity.Region, tt.wantRegion)
			}
			if identity.Provider != tt.wantProvider {
				t.Errorf("Provider = %v, want %v", identity.Provider, tt.wantProvider)
			}
		})
	}
}

func TestLocalProvider_GetConfig(t *testing.T) {
	tests := []struct {
		name       string
		configYAML string
		wantTTL    string
		wantJobID  string
		wantErr    bool
	}{
		{
			name: "complete config",
			configYAML: `
instance_id: test-01
region: local
ttl: 8h
idle_timeout: 1h
on_complete: exit
job_array:
  id: array-123
  name: test-array
  index: 0
`,
			wantTTL:   "8h",
			wantJobID: "array-123",
			wantErr:   false,
		},
		{
			name: "minimal config with defaults",
			configYAML: `
instance_id: test-01
region: local
`,
			wantTTL:   "",
			wantJobID: "",
			wantErr:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			configPath := filepath.Join(tmpDir, "config.yaml")
			if err := os.WriteFile(configPath, []byte(tt.configYAML), 0644); err != nil {
				t.Fatalf("Failed to write config: %v", err)
			}

			oldEnv := os.Getenv("SPAWN_CONFIG")
			if err := os.Setenv("SPAWN_CONFIG", configPath); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = os.Setenv("SPAWN_CONFIG", oldEnv) }()

			provider, err := NewLocalProvider(context.Background())
			if err != nil {
				t.Fatalf("NewLocalProvider() error = %v", err)
			}

			config, err := provider.GetConfig(context.Background())
			if (err != nil) != tt.wantErr {
				t.Errorf("GetConfig() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}

			if tt.wantTTL != "" {
				wantDuration, _ := time.ParseDuration(tt.wantTTL)
				if config.TTL != wantDuration {
					t.Errorf("TTL = %v, want %v", config.TTL, wantDuration)
				}
			}
			if config.JobArrayID != tt.wantJobID {
				t.Errorf("JobArrayID = %v, want %v", config.JobArrayID, tt.wantJobID)
			}
		})
	}
}

func TestLocalProvider_IsSpotInstance(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	configYAML := `
instance_id: test-01
region: local
`
	if err := os.WriteFile(configPath, []byte(configYAML), 0644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	oldEnv := os.Getenv("SPAWN_CONFIG")
	if err := os.Setenv("SPAWN_CONFIG", configPath); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Setenv("SPAWN_CONFIG", oldEnv) }()

	provider, err := NewLocalProvider(context.Background())
	if err != nil {
		t.Fatalf("NewLocalProvider() error = %v", err)
	}

	if provider.IsSpotInstance(context.Background()) {
		t.Errorf("Local provider should never be Spot instance")
	}
}

func TestLocalProvider_CheckSpotInterruption(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	configYAML := `
instance_id: test-01
region: local
`
	if err := os.WriteFile(configPath, []byte(configYAML), 0644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	oldEnv := os.Getenv("SPAWN_CONFIG")
	if err := os.Setenv("SPAWN_CONFIG", configPath); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Setenv("SPAWN_CONFIG", oldEnv) }()

	provider, err := NewLocalProvider(context.Background())
	if err != nil {
		t.Fatalf("NewLocalProvider() error = %v", err)
	}

	info, err := provider.CheckSpotInterruption(context.Background())
	if err != nil {
		t.Errorf("CheckSpotInterruption() error = %v", err)
	}
	if info != nil {
		t.Errorf("Local provider should return nil for Spot interruption check")
	}
}

func TestLocalProvider_Terminate(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	configYAML := `
instance_id: test-01
region: local
`
	if err := os.WriteFile(configPath, []byte(configYAML), 0644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	oldEnv := os.Getenv("SPAWN_CONFIG")
	if err := os.Setenv("SPAWN_CONFIG", configPath); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Setenv("SPAWN_CONFIG", oldEnv) }()

	provider, err := NewLocalProvider(context.Background())
	if err != nil {
		t.Fatalf("NewLocalProvider() error = %v", err)
	}

	// Terminate should not return error (but will exit in real usage)
	// We can't test the actual exit, but we can verify it doesn't error
	// In a real test, we'd need to mock os.Exit
	_ = provider
}

func TestLocalProvider_GetProviderType(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	configYAML := `
instance_id: test-01
region: local
`
	if err := os.WriteFile(configPath, []byte(configYAML), 0644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	oldEnv := os.Getenv("SPAWN_CONFIG")
	if err := os.Setenv("SPAWN_CONFIG", configPath); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Setenv("SPAWN_CONFIG", oldEnv) }()

	provider, err := NewLocalProvider(context.Background())
	if err != nil {
		t.Fatalf("NewLocalProvider() error = %v", err)
	}

	if got := provider.GetProviderType(); got != "local" {
		t.Errorf("GetProviderType() = %v, want local", got)
	}
}
