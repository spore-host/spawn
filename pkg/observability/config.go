package observability

// Config holds observability configuration
type Config struct {
	Metrics  MetricsConfig
	Tracing  TracingConfig
	Alerting AlertingConfig
}

// MetricsConfig holds metrics server configuration
type MetricsConfig struct {
	Enabled bool
	Port    int
	Path    string
	Bind    string
}

// TracingConfig holds distributed tracing configuration
type TracingConfig struct {
	Enabled      bool
	Exporter     string
	SamplingRate float64
	Endpoint     string
}

// AlertingConfig holds Prometheus/Alertmanager configuration
type AlertingConfig struct {
	PrometheusURL   string
	AlertmanagerURL string
}

// DefaultConfig returns default observability configuration
func DefaultConfig() Config {
	return Config{
		Metrics: MetricsConfig{
			Enabled: false,
			Port:    9090,
			Path:    "/metrics",
			Bind:    "localhost",
		},
		Tracing: TracingConfig{
			Enabled:      false,
			Exporter:     "xray",
			SamplingRate: 0.1,
		},
		Alerting: AlertingConfig{},
	}
}
