package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/spore-host/spawn/pkg/observability/metrics"
)

// GetStartTime returns when the agent started
func (a *Agent) GetStartTime() time.Time {
	return a.startTime
}

// GetCPUUsageContext returns the CPU usage the monitor loop measured on its
// last tick. It takes no sample of its own: the scrape runs on the HTTP
// server's goroutine, and sampling there raced the monitor loop and moved its
// baseline (#952). An unmeasured reading is an error, so no sample is exported
// rather than the assume-active 100%.
func (a *Agent) GetCPUUsageContext(ctx context.Context) (float64, error) {
	cpu, measured := a.GetCPUUsageMeasured()
	if !measured {
		return 0, errCPUUnmeasured
	}
	return cpu, nil
}

var errCPUUnmeasured = errors.New("CPU usage not measured yet")

// GetNetworkStats returns network statistics summed over the physical
// interfaces. It returns an error, and so no sample, when none is found: a zero
// counter labelled "eth0" on a host whose NIC is enp39s0 is what this reported
// before #919.
func (a *Agent) GetNetworkStats(ctx context.Context) (metrics.NetworkStats, error) {
	rx, tx, ifaces, err := readNetDevBytes("/proc/net/dev", "/sys/class/net")
	if err != nil {
		return metrics.NetworkStats{}, err
	}
	return metrics.NetworkStats{
		BytesReceived: uint64(rx),
		BytesSent:     uint64(tx),
		Interface:     strings.Join(ifaces, ","),
	}, nil
}

// GetDiskStats returns disk I/O statistics summed over the whole disks. It
// returns an error, and so no sample, when none is found: before #937 this
// dropped every NVMe disk and reported a zero counter labelled "unknown" on
// Nitro, and counted sda and sda1 both elsewhere.
func (a *Agent) GetDiskStats(ctx context.Context) (metrics.DiskStats, error) {
	read, written, devices, err := readDiskstatsBytes("/proc/diskstats", "/sys/block")
	if err != nil {
		return metrics.DiskStats{}, err
	}
	return metrics.DiskStats{
		BytesRead:  uint64(read),
		BytesWrite: uint64(written),
		Device:     strings.Join(devices, ","),
	}, nil
}

// GetGPUStats returns GPU statistics for all GPUs
func (a *Agent) GetGPUStats(ctx context.Context) ([]metrics.GPUStats, error) {
	_, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return nil, nil // No GPU, not an error
	}

	// Query multiple GPU stats at once
	cmd := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=index,utilization.gpu,memory.used,memory.total,temperature.gpu,power.draw",
		"--format=csv,noheader,nounits")
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	gpus := make([]metrics.GPUStats, 0, len(lines))

	for _, line := range lines {
		fields := strings.Split(line, ",")
		if len(fields) < 6 {
			continue
		}

		index, _ := strconv.Atoi(strings.TrimSpace(fields[0]))
		utilization, _ := strconv.ParseFloat(strings.TrimSpace(fields[1]), 64)
		memUsed, _ := strconv.ParseUint(strings.TrimSpace(fields[2]), 10, 64)
		memTotal, _ := strconv.ParseUint(strings.TrimSpace(fields[3]), 10, 64)
		temp, _ := strconv.ParseFloat(strings.TrimSpace(fields[4]), 64)
		power, _ := strconv.ParseFloat(strings.TrimSpace(fields[5]), 64)

		gpus = append(gpus, metrics.GPUStats{
			Index:       index,
			Utilization: utilization,
			MemoryUsed:  memUsed * 1024 * 1024, // Convert MiB to bytes
			MemoryTotal: memTotal * 1024 * 1024,
			Temperature: temp,
			PowerUsage:  power,
		})
	}

	return gpus, nil
}

// GetMemoryStats returns memory statistics
func (a *Agent) GetMemoryStats(ctx context.Context) (metrics.MemoryStats, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return metrics.MemoryStats{}, err
	}

	lines := strings.Split(string(data), "\n")
	var total, available uint64

	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		switch fields[0] {
		case "MemTotal:":
			total, _ = strconv.ParseUint(fields[1], 10, 64)
			total *= 1024 // Convert KiB to bytes
		case "MemAvailable:":
			available, _ = strconv.ParseUint(fields[1], 10, 64)
			available *= 1024
		}
	}

	used := total - available

	return metrics.MemoryStats{
		Used:  used,
		Total: total,
	}, nil
}

// GetTerminalCount returns the number of active terminal sessions
func (a *Agent) GetTerminalCount() int {
	// The metrics collector's interface carries no context; the probe timeout
	// still bounds the scrape.
	output, err := probeOutput(context.Background(), "ps", "aux")
	if err != nil {
		return 0
	}

	count := 0
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if strings.Contains(line, "pts/") || strings.Contains(line, "tty") {
			if strings.Contains(line, "bash") || strings.Contains(line, "zsh") ||
				strings.Contains(line, "sh") || strings.Contains(line, "ssh") {
				count++
			}
		}
	}

	return count
}

// GetLoggedInUserCount returns the number of logged-in users
func (a *Agent) GetLoggedInUserCount() int {
	output, err := probeOutput(context.Background(), "who")
	if err != nil {
		return 0
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	count := 0
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}

	return count
}

// IsSpotInstance returns whether this is a spot instance
func (a *Agent) IsSpotInstance() bool {
	return a.provider.IsSpotInstance(context.Background())
}
