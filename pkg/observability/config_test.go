package observability

import "testing"

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Metrics.Enabled {
		t.Error("metrics should be disabled by default")
	}

	if cfg.Metrics.Port != 9090 {
		t.Errorf("expected port 9090, got %d", cfg.Metrics.Port)
	}

	if cfg.Metrics.Path != "/metrics" {
		t.Errorf("expected path /metrics, got %s", cfg.Metrics.Path)
	}

	if cfg.Metrics.Bind != "localhost" {
		t.Errorf("expected bind localhost, got %s", cfg.Metrics.Bind)
	}

	if cfg.Tracing.Enabled {
		t.Error("tracing should be disabled by default")
	}

	if cfg.Tracing.Exporter != "xray" {
		t.Errorf("expected exporter xray, got %s", cfg.Tracing.Exporter)
	}

	if cfg.Tracing.SamplingRate != 0.1 {
		t.Errorf("expected sampling rate 0.1, got %f", cfg.Tracing.SamplingRate)
	}
}
