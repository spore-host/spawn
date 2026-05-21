package metrics

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spore-host/spawn/pkg/provider"
)

// MetricsSource provides data for metrics collection
type MetricsSource interface {
	GetIdentity() *provider.Identity
	GetConfig() *provider.Config
	GetStartTime() time.Time
	GetLastActivityTime() time.Time
	IsIdle() bool
	GetCPUUsageContext(ctx context.Context) (float64, error)
	GetNetworkStats(ctx context.Context) (NetworkStats, error)
	GetDiskStats(ctx context.Context) (DiskStats, error)
	GetGPUStats(ctx context.Context) ([]GPUStats, error)
	GetMemoryStats(ctx context.Context) (MemoryStats, error)
	GetTerminalCount() int
	GetLoggedInUserCount() int
	IsSpotInstance() bool
}

// NetworkStats holds network statistics
type NetworkStats struct {
	BytesReceived uint64
	BytesSent     uint64
	Interface     string
}

// DiskStats holds disk I/O statistics
type DiskStats struct {
	BytesRead  uint64
	BytesWrite uint64
	Device     string
}

// GPUStats holds GPU statistics
type GPUStats struct {
	Index       int
	Utilization float64
	MemoryUsed  uint64
	MemoryTotal uint64
	Temperature float64
	PowerUsage  float64
}

// MemoryStats holds memory statistics
type MemoryStats struct {
	Used  uint64
	Total uint64
}

// Collector collects metrics from an agent
type Collector struct {
	source MetricsSource

	// Instance lifecycle
	uptimeSeconds      *prometheus.Desc
	startTimeSeconds   *prometheus.Desc
	terminationPending *prometheus.Desc
	spotInstance       *prometheus.Desc

	// Resource usage
	cpuUsagePercent       *prometheus.Desc
	networkBytesTotal     *prometheus.Desc
	diskIOBytesTotal      *prometheus.Desc
	gpuUtilizationPercent *prometheus.Desc
	gpuMemoryUsedBytes    *prometheus.Desc
	gpuMemoryTotalBytes   *prometheus.Desc
	gpuTemperatureCelsius *prometheus.Desc
	gpuPowerWatts         *prometheus.Desc
	memoryUsedBytes       *prometheus.Desc
	memoryTotalBytes      *prometheus.Desc

	// Idle detection
	idleState           *prometheus.Desc
	idleDurationSeconds *prometheus.Desc
	activeTerminals     *prometheus.Desc
	loggedInUsers       *prometheus.Desc

	// Cost tracking
	costPerHourDollars   *prometheus.Desc
	estimatedCostDollars *prometheus.Desc

	// Timeouts
	ttlRemainingSeconds         *prometheus.Desc
	idleTimeoutRemainingSeconds *prometheus.Desc

	// Job arrays
	jobArraySize            *prometheus.Desc
	jobArrayIndex           *prometheus.Desc
	jobArrayPeersDiscovered *prometheus.Desc
}

// NewCollector creates a new Collector
func NewCollector(source MetricsSource) *Collector {
	const namespace = "spawn"

	return &Collector{
		source: source,

		uptimeSeconds: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "instance", "uptime_seconds"),
			"Time since instance started",
			[]string{"instance_id", "region", "provider"},
			nil,
		),
		startTimeSeconds: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "instance", "start_time_seconds"),
			"Unix timestamp when instance started",
			[]string{"instance_id"},
			nil,
		),
		terminationPending: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "instance", "termination_scheduled"),
			"Whether instance has termination scheduled (1=yes, 0=no)",
			[]string{"reason"},
			nil,
		),
		spotInstance: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "instance", "spot"),
			"Whether instance is a spot instance (1=yes, 0=no)",
			[]string{"instance_id"},
			nil,
		),

		cpuUsagePercent: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "cpu", "usage_percent"),
			"CPU usage percentage",
			[]string{"instance_id"},
			nil,
		),
		networkBytesTotal: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "network", "bytes_total"),
			"Network bytes transferred",
			[]string{"instance_id", "interface", "direction"},
			nil,
		),
		diskIOBytesTotal: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "disk", "io_bytes_total"),
			"Disk I/O bytes",
			[]string{"instance_id", "device", "operation"},
			nil,
		),
		gpuUtilizationPercent: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "gpu", "utilization_percent"),
			"GPU utilization percentage",
			[]string{"instance_id", "gpu_index"},
			nil,
		),
		gpuMemoryUsedBytes: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "gpu", "memory_used_bytes"),
			"GPU memory used in bytes",
			[]string{"instance_id", "gpu_index"},
			nil,
		),
		gpuMemoryTotalBytes: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "gpu", "memory_total_bytes"),
			"GPU memory total in bytes",
			[]string{"instance_id", "gpu_index"},
			nil,
		),
		gpuTemperatureCelsius: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "gpu", "temperature_celsius"),
			"GPU temperature in Celsius",
			[]string{"instance_id", "gpu_index"},
			nil,
		),
		gpuPowerWatts: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "gpu", "power_watts"),
			"GPU power usage in watts",
			[]string{"instance_id", "gpu_index"},
			nil,
		),
		memoryUsedBytes: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "memory", "used_bytes"),
			"Memory used in bytes",
			[]string{"instance_id"},
			nil,
		),
		memoryTotalBytes: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "memory", "total_bytes"),
			"Memory total in bytes",
			[]string{"instance_id"},
			nil,
		),

		idleState: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "idle", "state"),
			"Whether instance is idle (1=idle, 0=active)",
			[]string{"instance_id"},
			nil,
		),
		idleDurationSeconds: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "idle", "duration_seconds"),
			"Time since last activity",
			[]string{"instance_id"},
			nil,
		),
		activeTerminals: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "active", "terminals"),
			"Number of active terminal sessions",
			[]string{"instance_id"},
			nil,
		),
		loggedInUsers: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "logged_in", "users"),
			"Number of logged in users",
			[]string{"instance_id"},
			nil,
		),

		costPerHourDollars: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "cost", "per_hour_dollars"),
			"Cost per hour in dollars",
			[]string{"instance_type", "region", "spot"},
			nil,
		),
		estimatedCostDollars: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "estimated", "cost_dollars"),
			"Estimated cost since start in dollars",
			[]string{"instance_id"},
			nil,
		),

		ttlRemainingSeconds: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "ttl", "remaining_seconds"),
			"Time remaining before TTL expiration",
			[]string{"instance_id"},
			nil,
		),
		idleTimeoutRemainingSeconds: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "idle_timeout", "remaining_seconds"),
			"Time remaining before idle timeout",
			[]string{"instance_id"},
			nil,
		),

		jobArraySize: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "job_array", "size"),
			"Size of job array",
			[]string{"job_array_id", "job_array_name"},
			nil,
		),
		jobArrayIndex: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "job_array", "index"),
			"Index of this instance in job array",
			[]string{"job_array_id"},
			nil,
		),
		jobArrayPeersDiscovered: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "job_array", "peers_discovered"),
			"Number of peers discovered in job array",
			[]string{"job_array_id"},
			nil,
		),
	}
}

// Describe implements prometheus.Collector
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.uptimeSeconds
	ch <- c.startTimeSeconds
	ch <- c.terminationPending
	ch <- c.spotInstance
	ch <- c.cpuUsagePercent
	ch <- c.networkBytesTotal
	ch <- c.diskIOBytesTotal
	ch <- c.gpuUtilizationPercent
	ch <- c.gpuMemoryUsedBytes
	ch <- c.gpuMemoryTotalBytes
	ch <- c.gpuTemperatureCelsius
	ch <- c.gpuPowerWatts
	ch <- c.memoryUsedBytes
	ch <- c.memoryTotalBytes
	ch <- c.idleState
	ch <- c.idleDurationSeconds
	ch <- c.activeTerminals
	ch <- c.loggedInUsers
	ch <- c.costPerHourDollars
	ch <- c.estimatedCostDollars
	ch <- c.ttlRemainingSeconds
	ch <- c.idleTimeoutRemainingSeconds
	ch <- c.jobArraySize
	ch <- c.jobArrayIndex
	ch <- c.jobArrayPeersDiscovered
}

// Collect implements prometheus.Collector
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	ctx := context.Background()

	identity := c.source.GetIdentity()
	config := c.source.GetConfig()
	startTime := c.source.GetStartTime()

	// Instance lifecycle metrics
	uptime := time.Since(startTime)
	ch <- prometheus.MustNewConstMetric(
		c.uptimeSeconds,
		prometheus.GaugeValue,
		uptime.Seconds(),
		identity.InstanceID, identity.Region, identity.Provider,
	)

	ch <- prometheus.MustNewConstMetric(
		c.startTimeSeconds,
		prometheus.GaugeValue,
		float64(startTime.Unix()),
		identity.InstanceID,
	)

	// Spot instance
	spotValue := 0.0
	if c.source.IsSpotInstance() {
		spotValue = 1.0
	}
	ch <- prometheus.MustNewConstMetric(
		c.spotInstance,
		prometheus.GaugeValue,
		spotValue,
		identity.InstanceID,
	)

	// CPU usage
	if cpu, err := c.source.GetCPUUsageContext(ctx); err == nil {
		ch <- prometheus.MustNewConstMetric(
			c.cpuUsagePercent,
			prometheus.GaugeValue,
			cpu,
			identity.InstanceID,
		)
	}

	// Network stats
	if net, err := c.source.GetNetworkStats(ctx); err == nil {
		ch <- prometheus.MustNewConstMetric(
			c.networkBytesTotal,
			prometheus.CounterValue,
			float64(net.BytesReceived),
			identity.InstanceID, net.Interface, "rx",
		)
		ch <- prometheus.MustNewConstMetric(
			c.networkBytesTotal,
			prometheus.CounterValue,
			float64(net.BytesSent),
			identity.InstanceID, net.Interface, "tx",
		)
	}

	// Disk stats
	if disk, err := c.source.GetDiskStats(ctx); err == nil {
		ch <- prometheus.MustNewConstMetric(
			c.diskIOBytesTotal,
			prometheus.CounterValue,
			float64(disk.BytesRead),
			identity.InstanceID, disk.Device, "read",
		)
		ch <- prometheus.MustNewConstMetric(
			c.diskIOBytesTotal,
			prometheus.CounterValue,
			float64(disk.BytesWrite),
			identity.InstanceID, disk.Device, "write",
		)
	}

	// GPU stats
	if gpus, err := c.source.GetGPUStats(ctx); err == nil {
		for _, gpu := range gpus {
			indexStr := string(rune('0' + gpu.Index))
			ch <- prometheus.MustNewConstMetric(
				c.gpuUtilizationPercent,
				prometheus.GaugeValue,
				gpu.Utilization,
				identity.InstanceID, indexStr,
			)
			ch <- prometheus.MustNewConstMetric(
				c.gpuMemoryUsedBytes,
				prometheus.GaugeValue,
				float64(gpu.MemoryUsed),
				identity.InstanceID, indexStr,
			)
			ch <- prometheus.MustNewConstMetric(
				c.gpuMemoryTotalBytes,
				prometheus.GaugeValue,
				float64(gpu.MemoryTotal),
				identity.InstanceID, indexStr,
			)
			ch <- prometheus.MustNewConstMetric(
				c.gpuTemperatureCelsius,
				prometheus.GaugeValue,
				gpu.Temperature,
				identity.InstanceID, indexStr,
			)
			ch <- prometheus.MustNewConstMetric(
				c.gpuPowerWatts,
				prometheus.GaugeValue,
				gpu.PowerUsage,
				identity.InstanceID, indexStr,
			)
		}
	}

	// Memory stats
	if mem, err := c.source.GetMemoryStats(ctx); err == nil {
		ch <- prometheus.MustNewConstMetric(
			c.memoryUsedBytes,
			prometheus.GaugeValue,
			float64(mem.Used),
			identity.InstanceID,
		)
		ch <- prometheus.MustNewConstMetric(
			c.memoryTotalBytes,
			prometheus.GaugeValue,
			float64(mem.Total),
			identity.InstanceID,
		)
	}

	// Idle state
	idleValue := 0.0
	if c.source.IsIdle() {
		idleValue = 1.0
	}
	ch <- prometheus.MustNewConstMetric(
		c.idleState,
		prometheus.GaugeValue,
		idleValue,
		identity.InstanceID,
	)

	idleDuration := time.Since(c.source.GetLastActivityTime())
	ch <- prometheus.MustNewConstMetric(
		c.idleDurationSeconds,
		prometheus.GaugeValue,
		idleDuration.Seconds(),
		identity.InstanceID,
	)

	ch <- prometheus.MustNewConstMetric(
		c.activeTerminals,
		prometheus.GaugeValue,
		float64(c.source.GetTerminalCount()),
		identity.InstanceID,
	)

	ch <- prometheus.MustNewConstMetric(
		c.loggedInUsers,
		prometheus.GaugeValue,
		float64(c.source.GetLoggedInUserCount()),
		identity.InstanceID,
	)

	// TTL remaining
	if config.TTL > 0 {
		remaining := config.TTL - uptime
		if remaining < 0 {
			remaining = 0
		}
		ch <- prometheus.MustNewConstMetric(
			c.ttlRemainingSeconds,
			prometheus.GaugeValue,
			remaining.Seconds(),
			identity.InstanceID,
		)
	}

	// Idle timeout remaining
	if config.IdleTimeout > 0 {
		remaining := config.IdleTimeout - idleDuration
		if remaining < 0 {
			remaining = 0
		}
		ch <- prometheus.MustNewConstMetric(
			c.idleTimeoutRemainingSeconds,
			prometheus.GaugeValue,
			remaining.Seconds(),
			identity.InstanceID,
		)
	}

	// Job array metrics
	if config.JobArrayID != "" {
		if config.JobArraySize > 0 {
			ch <- prometheus.MustNewConstMetric(
				c.jobArraySize,
				prometheus.GaugeValue,
				float64(config.JobArraySize),
				config.JobArrayID, config.JobArrayName,
			)
		}

		ch <- prometheus.MustNewConstMetric(
			c.jobArrayIndex,
			prometheus.GaugeValue,
			float64(config.JobArrayIndex),
			config.JobArrayID,
		)
	}
}
