//go:build linux

package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// This file holds the Linux implementations of spored's OS-specific "leaf"
// operations — the only platform-dependent surface. The agent core (agent.go)
// is OS-agnostic and calls these. Windows equivalents live in sys_windows.go;
// sys_other.go has conservative stubs so darwin/dev builds compile (#77).

// sysReadCPUTimes returns cumulative idle and total CPU jiffies from /proc/stat.
func sysReadCPUTimes(_ context.Context) (idle, total int64, err error) {
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

// sysReadNetworkBytes returns cumulative RX+TX bytes over the physical
// interfaces, or -1, -1 ("unknown") when /proc/net/dev is unreadable or no
// interface in it is backed by a device (#919).
func sysReadNetworkBytes(_ context.Context) (rx, tx int64) {
	rx, tx, _, err := readNetDevBytes("/proc/net/dev", "/sys/class/net")
	if err != nil {
		return -1, -1
	}
	return rx, tx
}

// sysReadDiskIOBytes returns cumulative bytes read+written over the whole
// disks, or -1 ("unknown") when /proc/diskstats is unreadable or no device in it
// is a whole disk backed by hardware (#937).
func sysReadDiskIOBytes(_ context.Context) int64 {
	read, written, _, err := readDiskstatsBytes("/proc/diskstats", "/sys/block")
	if err != nil {
		return -1
	}
	return read + written
}

// sysIsProcessRunning reports whether a process with the exact name is running.
func sysIsProcessRunning(ctx context.Context, name string) bool {
	_, err := probeOutput(ctx, "pgrep", "-x", name)
	return err == nil
}

// sysCountActiveSessions returns the login sessions with keyboard input in the
// last 5 minutes, from the idle time of each terminal (ttyidle.go, #953).
func sysCountActiveSessions(ctx context.Context) int {
	return countTTYSessions(ctx, time.Now())
}

// sysCountActivePortConnections counts ESTABLISHED connections on the given
// ports via /proc/net/tcp — catches browser app users (RStudio/Jupyter) that
// don't show in `who`.
func sysCountActivePortConnections(_ context.Context, ports []int) int {
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

// sysWarnUsers broadcasts a message to logged-in terminals (wall).
func sysWarnUsers(ctx context.Context, message string) {
	_, _ = probeOutput(ctx, "wall", message)
}

// sysWriteConsole writes text to the serial console, where it survives the
// instance (#736).
//
// This is the only way to get a diagnostic off an instance that is about to
// terminate without an S3 bucket and an IAM grant. Measured on real hardware: a
// userspace write to /dev/console DOES appear in ec2 get-console-output, and
// that output outlives the instance's visibility in describe-instances by hours.
//
// Two measured caveats drove the design:
//
//   - The post-termination capture takes ~4-5 MINUTES to populate. Before that
//     the API returns nothing at all, so a caller must not promise an
//     immediately-readable log.
//   - Only SOME cloud-init output reaches the console on its own — rank 0's
//     mpirun result does, the surrounding script's echoes do not. So this has to
//     be an EXPLICIT write; relying on a log line drifting there does not work.
//
// Best-effort by construction. It runs on a terminate path where the instance is
// going away regardless, so a failure here must never block or fail that.
func sysWriteConsole(text string) error {
	f, err := os.OpenFile("/dev/console", os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = f.WriteString(text)
	return err
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

// sysComputeClockPath is where the daemon publishes its compute clock for
// `spored status` (#896). /run is a tmpfs on every systemd distro spawn
// supports, so the file cannot outlive the boot it describes: a stop/start
// clears it and the next daemon start rewrites it.
func sysComputeClockPath() string { return "/run/spored/compute-clock.json" }

// sysIdleStatePath is where the daemon publishes each tick's idle evaluation
// for `spored status` (#1090). On /run for the compute clock's reasons, and
// because tmpfs writes are not disk I/O: a per-minute write to a disk would be
// counted against idle itself.
func sysIdleStatePath() string { return "/run/spored/idle-state.json" }

// sysReloadMarkerPath is where the daemon acknowledges a reload (#1278). On
// /run with the compute clock: the pid it records means nothing after a boot.
func sysReloadMarkerPath() string { return "/run/spored/reloaded.json" }

// sysBootTime returns when the running kernel booted, from /proc/uptime, or the
// zero time when it cannot be read.
func sysBootTime() time.Time {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return time.Time{}
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return time.Time{}
	}
	secs, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || secs <= 0 {
		return time.Time{}
	}
	return time.Now().Add(-time.Duration(secs * float64(time.Second)))
}

// errEmptyProcStat: /proc/stat had no lines. Linux-only (#923): declared in the
// OS-agnostic agent.go it was dead code on every other GOOS.
var errEmptyProcStat = fmt.Errorf("empty cpu stat")

// sysAcceleratorFamilies finds GPUs and Gaudi by PCI vendor and class, and
// Neuron by its device nodes (accel.go, #954).
func sysAcceleratorFamilies() []string {
	return detectAccelerators("/sys/bus/pci/devices", "/dev")
}

// sysComputeCheckpointPath is the daemon's durable compute checkpoint (#928).
// Unlike the compute clock in /run, it must outlive the boot: /var/lib is on the
// root EBS volume, which a stop preserves.
func sysComputeCheckpointPath() string { return "/var/lib/spored/compute-checkpoint.json" }

// sysBootID is the kernel's random per-boot id, or "" when it cannot be read.
func sysBootID() string {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// sysKernelClocks reads CLOCK_MONOTONIC and CLOCK_BOOTTIME (#945). Monotonic is
// read first, so the pair can only overstate suspended time by the nanoseconds
// between the two calls, never understate running time.
func sysKernelClocks() (kernelClocks, bool) {
	var mono, boot unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &mono); err != nil {
		return kernelClocks{}, false
	}
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &boot); err != nil {
		return kernelClocks{}, false
	}
	return kernelClocks{Monotonic: time.Duration(mono.Nano()), Boottime: time.Duration(boot.Nano())}, true
}
