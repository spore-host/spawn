package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadLocalConfig(t *testing.T) {
	tests := []struct {
		name       string
		configYAML string
		env        map[string]string
		wantID     string
		wantRegion string
		wantTTL    string
		wantErr    bool
	}{
		{
			name: "complete config from file",
			configYAML: `
instance_id: workstation-01
region: us-west-2
account_id: "123456789012"
ttl: 8h
idle_timeout: 1h
on_complete: exit
job_array:
  id: array-123
  name: test-array
  index: 0
`,
			wantID:     "workstation-01",
			wantRegion: "us-west-2",
			wantTTL:    "8h",
			wantErr:    false,
		},
		{
			name: "minimal config with defaults",
			configYAML: `
instance_id: minimal-01
region: local
`,
			wantID:     "minimal-01",
			wantRegion: "local",
			wantTTL:    "",
			wantErr:    false,
		},
		{
			name: "environment variable override",
			configYAML: `
instance_id: from-file
region: us-east-1
`,
			env: map[string]string{
				"SPAWN_INSTANCE_ID": "from-env",
				"SPAWN_REGION":      "us-west-2",
			},
			wantID:     "from-env",
			wantRegion: "us-west-2",
			wantErr:    false,
		},
		{
			name: "auto instance_id",
			configYAML: `
instance_id: auto
region: local
`,
			wantID:     "", // Will be set to hostname
			wantRegion: "local",
			wantErr:    false,
		},
		{
			name: "with job array config",
			configYAML: `
instance_id: test-01
region: local
job_array:
  id: genomics-pipeline
  name: sequencing
  index: 5
`,
			wantID:     "test-01",
			wantRegion: "local",
			wantErr:    false,
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

			// Set environment variables
			for k, v := range tt.env {
				oldVal := os.Getenv(k)
				if err := os.Setenv(k, v); err != nil {
					t.Fatal(err)
				}
				defer func(key, val string) { _ = os.Setenv(key, val) }(k, oldVal)
			}

			// Load config
			cfg, err := LoadLocalConfig(configPath)
			if (err != nil) != tt.wantErr {
				t.Errorf("LoadLocalConfig() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}

			// Verify fields
			if tt.wantID != "" && cfg.InstanceID != tt.wantID {
				t.Errorf("InstanceID = %v, want %v", cfg.InstanceID, tt.wantID)
			}
			if tt.wantID == "" && cfg.InstanceID == "auto" {
				t.Errorf("InstanceID should be resolved from 'auto' to hostname")
			}
			if cfg.Region != tt.wantRegion {
				t.Errorf("Region = %v, want %v", cfg.Region, tt.wantRegion)
			}
			if tt.wantTTL != "" && cfg.TTL != tt.wantTTL {
				t.Errorf("TTL = %v, want %v", cfg.TTL, tt.wantTTL)
			}
		})
	}
}

func TestLoadLocalConfig_MissingFile(t *testing.T) {
	// LoadLocalConfig falls back to environment variables if file missing
	cfg, err := LoadLocalConfig("/nonexistent/config.yaml")
	if err != nil {
		t.Errorf("LoadLocalConfig() should fall back to env vars, got error: %v", err)
	}
	if cfg == nil {
		t.Fatalf("LoadLocalConfig() should return config even without file")
	}
	// Should have default values applied
	if cfg.InstanceID == "" {
		t.Errorf("InstanceID should have default value")
	}
}

func TestLoadLocalConfig_InvalidYAML(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	invalidYAML := `
instance_id: test
invalid yaml here [[[
`
	if err := os.WriteFile(configPath, []byte(invalidYAML), 0644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	_, err := LoadLocalConfig(configPath)
	if err == nil {
		t.Errorf("Expected error for invalid YAML")
	}
}

func TestLoadLocalConfig_EnvVarPriority(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	configYAML := `
instance_id: file-id
region: file-region
ttl: 4h
`
	if err := os.WriteFile(configPath, []byte(configYAML), 0644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	// Set all environment variables
	envVars := map[string]string{
		"SPAWN_INSTANCE_ID": "env-id",
		"SPAWN_REGION":      "env-region",
		"SPAWN_TTL":         "8h",
	}

	for k, v := range envVars {
		oldVal := os.Getenv(k)
		if err := os.Setenv(k, v); err != nil {
			t.Fatal(err)
		}
		defer func(key, val string) { _ = os.Setenv(key, val) }(k, oldVal)
	}

	cfg, err := LoadLocalConfig(configPath)
	if err != nil {
		t.Fatalf("LoadLocalConfig() error = %v", err)
	}

	// Environment variables should override file values
	if cfg.InstanceID != "env-id" {
		t.Errorf("InstanceID = %v, want env-id (env should override file)", cfg.InstanceID)
	}
	if cfg.Region != "env-region" {
		t.Errorf("Region = %v, want env-region (env should override file)", cfg.Region)
	}
	if cfg.TTL != "8h" {
		t.Errorf("TTL = %v, want 8h (env should override file)", cfg.TTL)
	}
}

func TestLoadLocalConfig_Defaults(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	// Minimal config
	configYAML := `
instance_id: test-01
region: local
`
	if err := os.WriteFile(configPath, []byte(configYAML), 0644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	cfg, err := LoadLocalConfig(configPath)
	if err != nil {
		t.Fatalf("LoadLocalConfig() error = %v", err)
	}

	// Check defaults are applied
	if cfg.OnComplete == "" {
		t.Errorf("OnComplete should have a default value")
	}
}

func TestLoadLocalConfig_JobArrayConfig(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	configYAML := `
instance_id: test-01
region: local
job_array:
  id: pipeline-123
  name: genomics
  index: 3
`
	if err := os.WriteFile(configPath, []byte(configYAML), 0644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	cfg, err := LoadLocalConfig(configPath)
	if err != nil {
		t.Fatalf("LoadLocalConfig() error = %v", err)
	}

	if cfg.JobArray.ID != "pipeline-123" {
		t.Errorf("JobArray.ID = %v, want pipeline-123", cfg.JobArray.ID)
	}
	if cfg.JobArray.Name != "genomics" {
		t.Errorf("JobArray.Name = %v, want genomics", cfg.JobArray.Name)
	}
	if cfg.JobArray.Index != 3 {
		t.Errorf("JobArray.Index = %v, want 3", cfg.JobArray.Index)
	}
}

func TestLoadLocalConfig_PublicIPResolution(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping IP resolution test in short mode")
	}

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	configYAML := `
instance_id: test-01
region: local
public_ip: auto
`
	if err := os.WriteFile(configPath, []byte(configYAML), 0644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	cfg, err := LoadLocalConfig(configPath)
	if err != nil {
		t.Fatalf("LoadLocalConfig() error = %v", err)
	}

	// Should resolve to an IP or fall back to a default
	if cfg.PublicIP == "auto" {
		t.Errorf("PublicIP should be resolved from 'auto'")
	}
}
