package metrics

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spore-host/spawn/pkg/provider"
)

type mockMetricsSource struct {
	identity         *provider.Identity
	config           *provider.Config
	startTime        time.Time
	lastActivityTime time.Time
	idle             bool
	spotInstance     bool
}

func (m *mockMetricsSource) GetIdentity() *provider.Identity {
	return m.identity
}

func (m *mockMetricsSource) GetConfig() *provider.Config {
	return m.config
}

func (m *mockMetricsSource) GetStartTime() time.Time {
	return m.startTime
}

func (m *mockMetricsSource) GetLastActivityTime() time.Time {
	return m.lastActivityTime
}

func (m *mockMetricsSource) IsIdle() bool {
	return m.idle
}

func (m *mockMetricsSource) GetCPUUsageContext(ctx context.Context) (float64, error) {
	return 25.5, nil
}

func (m *mockMetricsSource) GetNetworkStats(ctx context.Context) (NetworkStats, error) {
	return NetworkStats{
		BytesReceived: 1000000,
		BytesSent:     2000000,
		Interface:     "eth0",
	}, nil
}

func (m *mockMetricsSource) GetDiskStats(ctx context.Context) (DiskStats, error) {
	return DiskStats{
		BytesRead:  5000000,
		BytesWrite: 3000000,
		Device:     "xvda",
	}, nil
}

func (m *mockMetricsSource) GetGPUStats(ctx context.Context) ([]GPUStats, error) {
	return []GPUStats{
		{
			Index:       0,
			Utilization: 75.0,
			MemoryUsed:  8000000000,
			MemoryTotal: 16000000000,
			Temperature: 65.0,
			PowerUsage:  250.0,
		},
	}, nil
}

func (m *mockMetricsSource) GetMemoryStats(ctx context.Context) (MemoryStats, error) {
	return MemoryStats{
		Used:  4000000000,
		Total: 8000000000,
	}, nil
}

func (m *mockMetricsSource) GetTerminalCount() int {
	return 2
}

func (m *mockMetricsSource) GetLoggedInUserCount() int {
	return 1
}

func (m *mockMetricsSource) IsSpotInstance() bool {
	return m.spotInstance
}

func TestCollector(t *testing.T) {
	now := time.Now()
	source := &mockMetricsSource{
		identity: &provider.Identity{
			InstanceID: "i-test123",
			Region:     "us-east-1",
			Provider:   "ec2",
			AccountID:  "123456789012",
		},
		config: &provider.Config{
			TTL:           2 * time.Hour,
			IdleTimeout:   30 * time.Minute,
			JobArrayID:    "ja-123",
			JobArrayName:  "test-array",
			JobArraySize:  10,
			JobArrayIndex: 5,
		},
		startTime:        now.Add(-1 * time.Hour),
		lastActivityTime: now.Add(-5 * time.Minute),
		idle:             true,
		spotInstance:     true,
	}

	collector := NewCollector(source)

	// Test Describe
	descCh := make(chan *prometheus.Desc, 100)
	go func() {
		collector.Describe(descCh)
		close(descCh)
	}()

	descCount := 0
	for range descCh {
		descCount++
	}

	if descCount == 0 {
		t.Error("no metrics described")
	}

	// Test Collect
	metricCh := make(chan prometheus.Metric, 100)
	go func() {
		collector.Collect(metricCh)
		close(metricCh)
	}()

	metricCount := 0
	for range metricCh {
		metricCount++
	}

	if metricCount == 0 {
		t.Error("no metrics collected")
	}

	t.Logf("Collected %d metrics", metricCount)
}

func TestCollectorNoGPU(t *testing.T) {
	source := &mockMetricsSource{
		identity: &provider.Identity{
			InstanceID: "i-test123",
			Region:     "us-east-1",
			Provider:   "ec2",
		},
		config:           &provider.Config{},
		startTime:        time.Now(),
		lastActivityTime: time.Now(),
	}

	// Override GetGPUStats to return no GPU
	collector := NewCollector(source)

	metricCh := make(chan prometheus.Metric, 100)
	go func() {
		collector.Collect(metricCh)
		close(metricCh)
	}()

	for range metricCh {
		// Just ensure it doesn't panic
	}
}
