package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
	paramparser "github.com/spore-host/spawn/pkg/params"
)

// SweepConfig represents a parameter sweep configuration
type SweepConfig struct {
	SweepID       string                   `json:"sweep_id"`
	SweepName     string                   `json:"sweep_name"`
	ParamFile     string                   `json:"param_file"`
	Defaults      map[string]interface{}   `json:"defaults"`
	Params        []map[string]interface{} `json:"params"`
	MaxConcurrent int                      `json:"max_concurrent"`
	LaunchDelay   time.Duration            `json:"launch_delay"`
	Detached      bool                     `json:"detached"`
}

// SweepState tracks the state of a parameter sweep
type SweepState struct {
	SweepID       string          `json:"sweep_id"`
	SweepName     string          `json:"sweep_name"`
	CreatedAt     time.Time       `json:"created_at"`
	ParamFile     string          `json:"param_file"`
	TotalParams   int             `json:"total_params"`
	MaxConcurrent int             `json:"max_concurrent"`
	LaunchDelay   string          `json:"launch_delay"`
	Completed     int             `json:"completed"`
	Running       int             `json:"running"`
	Pending       int             `json:"pending"`
	Failed        int             `json:"failed"`
	Instances     []InstanceState `json:"instances"`
}

// InstanceState tracks the state of a single instance in a sweep
type InstanceState struct {
	Index        int       `json:"index"`
	InstanceID   string    `json:"instance_id"`
	State        string    `json:"state"`
	LaunchedAt   time.Time `json:"launched_at"`
	TerminatedAt time.Time `json:"terminated_at,omitempty"`
	ErrorMessage string    `json:"error_message,omitempty"`
}

// ParamFileFormat represents the JSON parameter file structure
type ParamFileFormat struct {
	Defaults map[string]interface{}   `json:"defaults"`
	Params   []map[string]interface{} `json:"params"`
}

// parseParamFile reads and parses a parameter file (JSON, YAML, or CSV)
func parseParamFile(path string) (*ParamFileFormat, error) {
	// Use the params package parser which supports JSON, YAML, and CSV
	result, err := paramparser.ParseParamFile(path)
	if err != nil {
		return nil, err
	}

	// Convert to cmd.ParamFileFormat (same structure)
	return &ParamFileFormat{
		Defaults: result.Defaults,
		Params:   result.Params,
	}, nil
}

// buildLaunchConfigFromParams merges defaults with parameter overrides
func buildLaunchConfigFromParams(defaults, params map[string]interface{}, sweepID, sweepName string, index, total int) (aws.LaunchConfig, error) {
	// Start with an empty config
	config := aws.LaunchConfig{
		SweepID:    sweepID,
		SweepName:  sweepName,
		SweepIndex: index,
		SweepSize:  total,
		Parameters: make(map[string]string),
	}

	// Merge defaults and params into a single map
	merged := make(map[string]interface{})
	for k, v := range defaults {
		merged[k] = v
	}
	for k, v := range params {
		merged[k] = v
	}

	// Map known fields to LaunchConfig struct fields
	for key, val := range merged {
		switch key {
		case "instance_type":
			if s, ok := val.(string); ok {
				config.InstanceType = s
			}
		case "region":
			if s, ok := val.(string); ok {
				config.Region = s
			}
		case "az", "availability_zone":
			if s, ok := val.(string); ok {
				config.AvailabilityZone = s
			}
		case "ami":
			if s, ok := val.(string); ok {
				config.AMI = s
			}
		case "key_pair", "key_name":
			if s, ok := val.(string); ok {
				config.KeyName = s
			}
		case "spot":
			if b, ok := val.(bool); ok {
				config.Spot = b
			}
		case "spot_max_price":
			if s, ok := val.(string); ok {
				config.SpotMaxPrice = s
			}
		case "hibernate":
			if b, ok := val.(bool); ok {
				config.Hibernate = b
			}
		case "ttl":
			if s, ok := val.(string); ok {
				config.TTL = s
			}
		case "idle_timeout":
			if s, ok := val.(string); ok {
				config.IdleTimeout = s
			}
		case "hibernate_on_idle":
			if b, ok := val.(bool); ok {
				config.HibernateOnIdle = b
			}
		case "session_timeout":
			if s, ok := val.(string); ok {
				config.SessionTimeout = s
			}
		case "on_complete":
			if s, ok := val.(string); ok {
				config.OnComplete = s
			}
		case "completion_file":
			if s, ok := val.(string); ok {
				config.CompletionFile = s
			}
		case "completion_delay":
			if s, ok := val.(string); ok {
				config.CompletionDelay = s
			}
		case "dns", "dns_name":
			if s, ok := val.(string); ok {
				config.DNSName = s
			}
		case "step":
			// Workflow step name
			if s, ok := val.(string); ok {
				if config.Tags == nil {
					config.Tags = make(map[string]string)
				}
				config.Tags["spawn:step"] = s
			}
		case "command", "user_command":
			// Store command for user-data execution
			if s, ok := val.(string); ok {
				if config.Tags == nil {
					config.Tags = make(map[string]string)
				}
				config.Tags["spawn:command"] = s
			}
		case "user_data":
			if s, ok := val.(string); ok {
				config.UserData = s
			}
		case "iam_role":
			if s, ok := val.(string); ok {
				config.IamInstanceProfile = s
			}
		case "name":
			if s, ok := val.(string); ok {
				config.Name = s
			}
		default:
			// All unknown fields become parameters (PARAM_* env vars)
			config.Parameters[key] = fmt.Sprintf("%v", val)
		}
	}

	return config, nil
}

// generateSweepID creates a unique sweep identifier
func generateSweepID(name string) string {
	timestamp := time.Now().Format("20060102")
	random := fmt.Sprintf("%06d", time.Now().UnixNano()%1000000)
	return fmt.Sprintf("%s-%s-%s", name, timestamp, random)
}

// getSweepStateDir returns the directory for sweep state files
func getSweepStateDir() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	dir := filepath.Join(homeDir, ".spawn", "sweeps")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("failed to create sweeps directory: %w", err)
	}
	return dir, nil
}

// saveSweepState saves the sweep state to ~/.spawn/sweeps/<id>.json
func saveSweepState(state *SweepState) error {
	dir, err := getSweepStateDir()
	if err != nil {
		return err
	}

	path := filepath.Join(dir, fmt.Sprintf("%s.json", state.SweepID))
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal sweep state: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("failed to write sweep state file: %w", err)
	}

	return nil
}

// loadSweepState loads the sweep state from ~/.spawn/sweeps/<id>.json
func loadSweepState(sweepID string) (*SweepState, error) {
	dir, err := getSweepStateDir()
	if err != nil {
		return nil, err
	}

	path := filepath.Join(dir, fmt.Sprintf("%s.json", sweepID))
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read sweep state file: %w", err)
	}

	var state SweepState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("failed to parse sweep state: %w", err)
	}

	return &state, nil
}
