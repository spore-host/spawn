package orchestrator

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// BurstPolicy defines auto-burst behavior
type BurstPolicy struct {
	Mode                string  `yaml:"mode"`                  // "manual", "auto", "scheduled"
	QueueDepthThreshold int     `yaml:"queue_depth_threshold"` // Burst if queue > N
	LocalCapacity       int     `yaml:"local_capacity"`        // Max local instances
	MaxCloudInstances   int     `yaml:"max_cloud_instances"`   // Cloud instance limit
	CostBudget          float64 `yaml:"cost_budget"`           // Max $/hour
	ScaleDownDelay      string  `yaml:"scale_down_delay"`      // Wait before terminating idle
	MinCloudInstances   int     `yaml:"min_cloud_instances"`   // Keep warm pool

	// Instance configuration
	InstanceType   string   `yaml:"instance_type"`
	AMI            string   `yaml:"ami"`
	Spot           bool     `yaml:"spot"`
	KeyName        string   `yaml:"key_name"`
	SubnetID       string   `yaml:"subnet_id"`
	SecurityGroups []string `yaml:"security_groups"`

	// Monitoring
	CheckInterval string `yaml:"check_interval"` // How often to check queue

	// Parsed durations
	scaleDownDelay time.Duration
	checkInterval  time.Duration
}

// Config is the orchestrator configuration
type Config struct {
	JobArrayID  string      `yaml:"job_array_id"`
	QueueURL    string      `yaml:"queue_url"`
	BurstPolicy BurstPolicy `yaml:"burst_policy"`
	Region      string      `yaml:"region"`
}

// LoadConfig loads orchestrator config from file
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	// Apply defaults
	if cfg.BurstPolicy.Mode == "" {
		cfg.BurstPolicy.Mode = "manual"
	}
	if cfg.BurstPolicy.QueueDepthThreshold == 0 {
		cfg.BurstPolicy.QueueDepthThreshold = 100
	}
	if cfg.BurstPolicy.MaxCloudInstances == 0 {
		cfg.BurstPolicy.MaxCloudInstances = 50
	}
	if cfg.BurstPolicy.InstanceType == "" {
		cfg.BurstPolicy.InstanceType = "t3.micro"
	}
	if cfg.BurstPolicy.ScaleDownDelay == "" {
		cfg.BurstPolicy.ScaleDownDelay = "5m"
	}
	if cfg.BurstPolicy.CheckInterval == "" {
		cfg.BurstPolicy.CheckInterval = "1m"
	}

	// Parse durations
	cfg.BurstPolicy.scaleDownDelay, err = time.ParseDuration(cfg.BurstPolicy.ScaleDownDelay)
	if err != nil {
		return nil, fmt.Errorf("invalid scale_down_delay: %w", err)
	}

	cfg.BurstPolicy.checkInterval, err = time.ParseDuration(cfg.BurstPolicy.CheckInterval)
	if err != nil {
		return nil, fmt.Errorf("invalid check_interval: %w", err)
	}

	return &cfg, nil
}

// GetScaleDownDelay returns parsed scale down delay
func (p *BurstPolicy) GetScaleDownDelay() time.Duration {
	return p.scaleDownDelay
}

// GetCheckInterval returns parsed check interval
func (p *BurstPolicy) GetCheckInterval() time.Duration {
	return p.checkInterval
}
