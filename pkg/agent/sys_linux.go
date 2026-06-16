//go:build linux

package agent

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// This file holds the Linux implementations of spored's OS-specific "leaf"
// operations — the only platform-dependent surface. The agent core (agent.go)
// is OS-agnostic and calls these. Windows equivalents live in sys_windows.go;
// sys_other.go has conservative stubs so darwin/dev builds compile (#77).

// sysReadCPUTimes returns cumulative idle and total CPU jiffies from /proc/stat.
func sysReadCPUTimes() (idle, total int64, err error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0, err
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 {
		return 0, 0, errEmptyProcStat
	}
	fields := strings.Fields(lines[0])
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0, errBadProcStat
	}
	idleVal, _ := strconv.ParseInt(fields[4], 10, 64)
	var totalVal int64
	for _, f := range fields[1:] {
		v, _ := strconv.ParseInt(f, 10, 64)
		totalVal += v
	}
	return idleVal, totalVal, nil
}

// sysReadNetworkBytes returns cumulative RX+TX bytes on the primary interfaces.
func sysReadNetworkBytes() (rx, tx int64) {
	data, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return -1, -1 // signal "unknown" → caller assumes active
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, "eth0") || strings.Contains(line, "ens") {
			fields := strings.Fields(line)
			if len(fields) >= 10 {
				r, _ := strconv.ParseInt(fields[1], 10, 64)
				t, _ := strconv.ParseInt(fields[9], 10, 64)
				rx += r
				tx += t
			}
		}
	}
	return rx, tx
}

// sysReadDiskIOBytes returns cumulative bytes read+written across block devices.
func sysReadDiskIOBytes() int64 {
	data, err := os.ReadFile("/proc/diskstats")
	if err != nil {
		return 0
	}
	var totalSectors int64
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 14 {
			continue
		}
		deviceName := fields[2]
		if strings.HasPrefix(deviceName, "xvd") || strings.HasPrefix(deviceName, "nvme") ||
			strings.HasPrefix(deviceName, "sd") || strings.HasPrefix(deviceName, "vd") {
			// Skip partitions (trailing digit).
			if len(deviceName) > 4 && deviceName[len(deviceName)-1] >= '0' && deviceName[len(deviceName)-1] <= '9' {
				continue
			}
			sectorsRead, _ := strconv.ParseInt(fields[5], 10, 64)
			sectorsWritten, _ := strconv.ParseInt(fields[9], 10, 64)
			totalSectors += sectorsRead + sectorsWritten
		}
	}
	return totalSectors * 512
}

// sysIsProcessRunning reports whether a process with the exact name is running.
func sysIsProcessRunning(name string) bool {
	return exec.Command("pgrep", "-x", name).Run() == nil
}

// sysCountActiveSessions returns login sessions with recent keyboard activity,
// via `w -h -s` (idle column), falling back to `who` for raw presence.
func sysCountActiveSessions() int {
	out, err := exec.Command("w", "-h", "-s").Output()
	if err != nil {
		who, werr := exec.Command("who").Output()
		if werr != nil {
			return 0
		}
		count := 0
		for _, line := range strings.Split(strings.TrimSpace(string(who)), "\n") {
			if strings.TrimSpace(line) != "" {
				count++
			}
		}
		return count
	}
	active := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		if isRecentActivity(fields[3], 5*time.Minute) {
			active++
		}
	}
	return active
}

// sysCountActivePortConnections counts ESTABLISHED connections on the given
// ports via /proc/net/tcp — catches browser app users (RStudio/Jupyter) that
// don't show in `who`.
func sysCountActivePortConnections(ports []int) int {
	if len(ports) == 0 {
		return 0
	}
	portSet := make(map[int]bool, len(ports))
	for _, p := range ports {
		portSet[p] = true
	}
	data, err := os.ReadFile("/proc/net/tcp")
	if err != nil {
		return 0
	}
	count := 0
	for _, line := range strings.Split(string(data), "\n")[1:] {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[3] != "01" { // 01 = ESTABLISHED
			continue
		}
		parts := strings.SplitN(fields[1], ":", 2)
		if len(parts) != 2 {
			continue
		}
		port64, err := strconv.ParseInt(parts[1], 16, 32)
		if err != nil {
			continue
		}
		if portSet[int(port64)] {
			count++
		}
	}
	return count
}

// sysHasActiveTerminals reports whether interactive PTYs exist (/dev/pts).
func sysHasActiveTerminals() bool {
	entries, err := os.ReadDir("/dev/pts")
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.Name() == "ptmx" {
			continue
		}
		if _, err := strconv.Atoi(entry.Name()); err == nil {
			return true
		}
	}
	return false
}

// sysHasRecentUserActivity reports recent logins (last 5 min) via `last`,
// falling back to /var/log/wtmp mtime.
func sysHasRecentUserActivity() bool {
	output, err := exec.Command("last", "-s", "-5min", "-w").Output()
	if err != nil {
		fileInfo, serr := os.Stat("/var/log/wtmp")
		if serr != nil {
			return false
		}
		return time.Since(fileInfo.ModTime()) < 5*time.Minute
	}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "wtmp") && !strings.HasPrefix(line, "reboot") {
			if !strings.Contains(line, "system boot") && !strings.Contains(line, "down") {
				return true
			}
		}
	}
	return false
}

// sysWarnUsers broadcasts a message to logged-in terminals (wall).
func sysWarnUsers(message string) {
	_ = exec.Command("wall", message).Run()
}

// sysShellCommand builds the OS shell invocation for a user pre-stop hook. When
// user is non-empty it runs the hook as that user via `su - <user> -c` (a login
// shell, so $HOME/PATH/env match how the workload ran) instead of as root —
// spored is a root service, and running the hook as root made ~/$HOME resolve to
// /root, silently sending e.g. `aws s3 sync ~/out …` to the wrong directory (#63).
// user is validated by the caller; if empty, falls back to the legacy root shell.
func sysShellCommand(ctx context.Context, command, user string) *exec.Cmd {
	if user != "" {
		return exec.CommandContext(ctx, "su", "-", user, "-c", command) // nosemgrep: dangerous-exec-command -- user-configured pre-stop hook runs as the instance's own user on their own instance
	}
	return exec.CommandContext(ctx, "sh", "-c", command) // nosemgrep: dangerous-exec-command -- user-configured pre-stop hook runs on their own instance
}

// sysMountLustre mounts an FSx Lustre filesystem at runtime (#194), mirroring the
// boot-time storage user-data: ensure the lustre client + module, then mount
// <dnsName>@tcp:/<mountName> at mountPoint and add an fstab line. Used by spored
// when a filesystem was created asynchronously and wasn't AVAILABLE at first boot.
func sysMountLustre(ctx context.Context, dnsName, mountName, mountPoint string) error {
	script := strings.Join([]string{
		"set -e",
		"command -v mount.lustre >/dev/null 2>&1 || dnf install -y lustre-client",
		"modprobe lustre 2>/dev/null || true",
		"mkdir -p " + shellQuoteArg(mountPoint),
		// Idempotent: skip if already mounted (spored restart).
		"mountpoint -q " + shellQuoteArg(mountPoint) + " && exit 0",
		"mount -t lustre -o noatime,flock " + shellQuoteArg(dnsName+"@tcp:/"+mountName) + " " + shellQuoteArg(mountPoint),
		"grep -q " + shellQuoteArg(mountPoint) + " /etc/fstab || echo " +
			shellQuoteArg(dnsName+"@tcp:/"+mountName+" "+mountPoint+" lustre noatime,flock,_netdev 0 0") + " >> /etc/fstab",
	}, "\n")
	cmd := exec.CommandContext(ctx, "sh", "-c", script) // nosemgrep: dangerous-exec-command -- fixed mount script; inputs are FSx-API-provided DNS/mount-name + the configured mount point, shell-quoted
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// shellQuoteArg single-quotes an argument for safe embedding in the mount script.
func shellQuoteArg(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
