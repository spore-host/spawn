package config

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spore-host/spawn/pkg/plugin"
	"gopkg.in/yaml.v3"
)

// LocalConfig represents the local configuration file format
type LocalConfig struct {
	// Instance identity
	InstanceID string `yaml:"instance_id"`
	Name       string `yaml:"name"`
	Region     string `yaml:"region"`
	AccountID  string `yaml:"account_id"`
	PublicIP   string `yaml:"public_ip"`
	PrivateIP  string `yaml:"private_ip"`

	// Lifecycle configuration
	TTL             string  `yaml:"ttl"`
	IdleTimeout     string  `yaml:"idle_timeout"`
	HibernateOnIdle bool    `yaml:"hibernate_on_idle"`
	IdleCPUPercent  float64 `yaml:"idle_cpu_percent"`
	OnComplete      string  `yaml:"on_complete"`
	CompletionFile  string  `yaml:"completion_file"`
	CompletionDelay string  `yaml:"completion_delay"`

	// DNS configuration (optional)
	DNS struct {
		Enabled bool   `yaml:"enabled"`
		Name    string `yaml:"name"`
		Domain  string `yaml:"domain"`
	} `yaml:"dns"`

	// Job array configuration
	JobArray struct {
		ID        string `yaml:"id"`
		Name      string `yaml:"name"`
		Index     int    `yaml:"index"`
		PeersFile string `yaml:"peers_file"`
	} `yaml:"job_array"`

	// Orchestrator configuration (for burst coordination)
	Orchestrator struct {
		Enabled           bool   `yaml:"enabled"`
		URL               string `yaml:"url"`
		RegisterOnStartup bool   `yaml:"register_on_startup"`
	} `yaml:"orchestrator"`

	// Plugin declarations to install at instance startup.
	Plugins []plugin.Declaration `yaml:"plugins,omitempty"`

	// Observability configuration
	Observability struct {
		Metrics struct {
			Enabled bool   `yaml:"enabled"`
			Port    int    `yaml:"port"`
			Path    string `yaml:"path"`
			Bind    string `yaml:"bind"`
		} `yaml:"metrics"`
		Tracing struct {
			Enabled      bool    `yaml:"enabled"`
			Exporter     string  `yaml:"exporter"`
			SamplingRate float64 `yaml:"sampling_rate"`
			Endpoint     string  `yaml:"endpoint"`
		} `yaml:"tracing"`
		Alerting struct {
			PrometheusURL   string `yaml:"prometheus_url"`
			AlertmanagerURL string `yaml:"alertmanager_url"`
		} `yaml:"alerting"`
	} `yaml:"observability"`
}

// LoadLocalConfig loads configuration from file and environment variables
func LoadLocalConfig(configPath string) (*LocalConfig, error) {
	// Default config path
	if configPath == "" {
		configPath = "/etc/spawn/local.yaml"
	}

	// Check if config file exists
	data, err := os.ReadFile(configPath)
	if err != nil {
		// Config file doesn't exist - use environment variables only
		return loadFromEnv(), nil
	}

	// Parse YAML
	var config LocalConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// Override with environment variables
	mergeEnvVars(&config)

	// Apply defaults
	applyDefaults(&config)

	return &config, nil
}

// loadFromEnv creates config from environment variables only
func loadFromEnv() *LocalConfig {
	config := &LocalConfig{}
	mergeEnvVars(config)
	applyDefaults(config)
	return config
}

// mergeEnvVars overrides config with environment variables
func mergeEnvVars(config *LocalConfig) {
	if val := os.Getenv("SPAWN_INSTANCE_ID"); val != "" {
		config.InstanceID = val
	}
	if val := os.Getenv("SPAWN_INSTANCE_NAME"); val != "" {
		config.Name = val
	}
	if val := os.Getenv("SPAWN_REGION"); val != "" {
		config.Region = val
	}
	if val := os.Getenv("SPAWN_ACCOUNT_ID"); val != "" {
		config.AccountID = val
	}
	if val := os.Getenv("SPAWN_PUBLIC_IP"); val != "" {
		config.PublicIP = val
	}
	if val := os.Getenv("SPAWN_PRIVATE_IP"); val != "" {
		config.PrivateIP = val
	}
	if val := os.Getenv("SPAWN_TTL"); val != "" {
		config.TTL = val
	}
	if val := os.Getenv("SPAWN_IDLE_TIMEOUT"); val != "" {
		config.IdleTimeout = val
	}
	if val := os.Getenv("SPAWN_ON_COMPLETE"); val != "" {
		config.OnComplete = val
	}
	if val := os.Getenv("SPAWN_DNS_NAME"); val != "" {
		config.DNS.Name = val
	}
	if val := os.Getenv("SPAWN_JOB_ARRAY_ID"); val != "" {
		config.JobArray.ID = val
	}
}

// applyDefaults fills in default values
func applyDefaults(config *LocalConfig) {
	if config.InstanceID == "" || config.InstanceID == "auto" {
		// Use hostname as instance ID
		hostname, err := os.Hostname()
		if err == nil {
			config.InstanceID = "local-" + hostname
		} else {
			config.InstanceID = "local-unknown"
		}
	}

	if config.Region == "" {
		config.Region = "local"
	}

	if config.AccountID == "" {
		config.AccountID = "local"
	}

	// Resolve public IP if set to "auto"
	if config.PublicIP == "auto" {
		if ip := detectPublicIP(); ip != "" {
			config.PublicIP = ip
		} else {
			// Fallback to first non-loopback IP
			config.PublicIP = detectLocalIP()
		}
	}

	if config.IdleCPUPercent == 0 {
		config.IdleCPUPercent = 5.0
	}

	if config.OnComplete == "" {
		config.OnComplete = "exit"
	}

	if config.CompletionFile == "" && config.OnComplete != "" {
		config.CompletionFile = "/tmp/SPAWN_COMPLETE"
	}

	if config.CompletionDelay == "" && config.OnComplete != "" {
		config.CompletionDelay = "30s"
	}
}

// ParseDuration parses a duration string, returns zero duration on error.
// It accepts Go's native duration syntax plus a day unit (e.g. "2d", "1d12h"),
// which time.ParseDuration does not support.
func ParseDuration(s string) time.Duration {
	d, err := ParseDurationE(s)
	if err != nil {
		return 0
	}
	return d
}

// ParseDurationE parses a duration string composed of <number><unit> segments
// where unit is one of s, m, h, d (days). It first tries Go's native
// time.ParseDuration (which handles ns..h and fractional values), then falls
// back to the segment parser so day units and combined forms like "1d12h" work.
func ParseDurationE(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}

	// Native syntax first (handles fractional values and sub-second units).
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}

	// Fallback: <number><unit> segments, unit ∈ {s,m,h,d}.
	var total time.Duration
	remaining := s
	for len(remaining) > 0 {
		i := 0
		for i < len(remaining) && remaining[i] >= '0' && remaining[i] <= '9' {
			i++
		}
		if i == 0 {
			return 0, fmt.Errorf("invalid duration format: %s", s)
		}
		numStr := remaining[:i]
		if i >= len(remaining) {
			return 0, fmt.Errorf("invalid duration format: missing unit in %s", s)
		}
		unit := remaining[i]
		remaining = remaining[i+1:]

		num, err := strconv.ParseInt(numStr, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid number in duration: %s", numStr)
		}
		switch unit {
		case 's':
			total += time.Duration(num) * time.Second
		case 'm':
			total += time.Duration(num) * time.Minute
		case 'h':
			total += time.Duration(num) * time.Hour
		case 'd':
			total += time.Duration(num) * 24 * time.Hour
		default:
			return 0, fmt.Errorf("invalid unit in duration: %c", unit)
		}
	}
	return total, nil
}

// detectPublicIP attempts to detect the public IP address
func detectPublicIP() string {
	// Try common IP detection services with short timeout
	services := []string{
		"https://api.ipify.org",
		"https://icanhazip.com",
		"https://ifconfig.me",
	}

	client := &http.Client{
		Timeout: 2 * time.Second,
	}

	for _, service := range services {
		resp, err := client.Get(service)
		if err != nil {
			continue
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode == http.StatusOK {
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				continue
			}
			ip := strings.TrimSpace(string(body))
			if net.ParseIP(ip) != nil {
				return ip
			}
		}
	}

	return ""
}

// detectLocalIP returns the first non-loopback IP address
func detectLocalIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "127.0.0.1"
	}

	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String()
			}
		}
	}

	return "127.0.0.1"
}
