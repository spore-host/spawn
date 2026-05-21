package agent

import (
	"context"
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

// GetCPUUsageContext returns CPU usage percentage (metrics interface with context)
func (a *Agent) GetCPUUsageContext(ctx context.Context) (float64, error) {
	return a.getCPUUsage(), nil
}

// GetNetworkStats returns network statistics
func (a *Agent) GetNetworkStats(ctx context.Context) (metrics.NetworkStats, error) {
	data, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return metrics.NetworkStats{}, err
	}

	lines := strings.Split(string(data), "\n")
	var rxBytes, txBytes uint64
	var iface string

	for _, line := range lines {
		if strings.Contains(line, "eth0") || strings.Contains(line, "ens") {
			fields := strings.Fields(line)
			if len(fields) >= 10 {
				iface = strings.TrimSuffix(fields[0], ":")
				rx, _ := strconv.ParseUint(fields[1], 10, 64)
				tx, _ := strconv.ParseUint(fields[9], 10, 64)
				rxBytes += rx
				txBytes += tx
			}
		}
	}

	if iface == "" {
		iface = "eth0"
	}

	return metrics.NetworkStats{
		BytesReceived: rxBytes,
		BytesSent:     txBytes,
		Interface:     iface,
	}, nil
}

// GetDiskStats returns disk I/O statistics
func (a *Agent) GetDiskStats(ctx context.Context) (metrics.DiskStats, error) {
	data, err := os.ReadFile("/proc/diskstats")
	if err != nil {
		return metrics.DiskStats{}, err
	}

	lines := strings.Split(string(data), "\n")
	var sectorsRead, sectorsWritten uint64
	var device string

	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 14 {
			continue
		}

		deviceName := fields[2]
		if strings.HasPrefix(deviceName, "xvd") || strings.HasPrefix(deviceName, "nvme") ||
			strings.HasPrefix(deviceName, "sd") || strings.HasPrefix(deviceName, "vd") {
			// Skip partition numbers
			if len(deviceName) > 4 && deviceName[len(deviceName)-1] >= '0' && deviceName[len(deviceName)-1] <= '9' {
				continue
			}

			if device == "" {
				device = deviceName
			}

			sr, _ := strconv.ParseUint(fields[5], 10, 64)
			sw, _ := strconv.ParseUint(fields[9], 10, 64)
			sectorsRead += sr
			sectorsWritten += sw
		}
	}

	if device == "" {
		device = "unknown"
	}

	return metrics.DiskStats{
		BytesRead:  sectorsRead * 512,
		BytesWrite: sectorsWritten * 512,
		Device:     device,
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
	cmd := exec.Command("ps", "aux")
	output, err := cmd.Output()
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
	cmd := exec.Command("who")
	output, err := cmd.Output()
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
