//go:build windows

package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows implementations of spored's OS-specific idle/metrics leaves (#77).
//
// These shell out to native Windows tools (PowerShell / quser) rather than
// linking a WMI binding, mirroring the Linux approach (which execs w/who/pgrep)
// and keeping the dependency surface zero. Every reader is conservative: on any
// error it returns the "assume active / unknown" sentinel so spored never
// falsely concludes a busy Windows desktop is idle.

// psOutput runs a PowerShell one-liner and returns trimmed stdout. ctx stops
// it: a cold PowerShell start takes seconds, and a reader that ignored the
// context held up Monitor's return for ~14s (#1104).
func psOutput(ctx context.Context, script string) (string, error) {
	out, err := probeOutput(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// psInt64 runs a PowerShell one-liner expected to print a single integer.
func psInt64(ctx context.Context, script string) (int64, bool) {
	s, err := psOutput(ctx, script)
	if err != nil || s == "" {
		return 0, false
	}
	// Some counters print as floats; take the integer part.
	if i := strings.IndexAny(s, ".,"); i >= 0 {
		s = s[:i]
	}
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// sysReadCPUTimes returns cumulative CPU idle and total counters (100ns units)
// from Win32_PerfRawData_PerfOS_Processor(_Total): PercentIdleTime is the
// cumulative idle counter and Timestamp_Sys100NS is the cumulative time base.
// These are monotonic, exactly like /proc/stat's idle/total, so the shared
// delta math in getCPUUsage (100 - deltaIdle/deltaTotal*100) yields CPU-busy %.
func sysReadCPUTimes(ctx context.Context) (idle, total int64, err error) {
	out, perr := psOutput(ctx, `$p = Get-CimInstance Win32_PerfRawData_PerfOS_Processor | Where-Object {$_.Name -eq "_Total"}; "$($p.PercentIdleTime) $($p.Timestamp_Sys100NS)"`)
	if perr != nil {
		return 0, 0, perr
	}
	fields := strings.Fields(out)
	if len(fields) != 2 {
		return 0, 0, errBadProcStat
	}
	idle, e1 := strconv.ParseInt(fields[0], 10, 64)
	total, e2 := strconv.ParseInt(fields[1], 10, 64)
	if e1 != nil || e2 != nil {
		return 0, 0, errBadProcStat
	}
	return idle, total, nil
}

// sysReadNetworkBytes returns cumulative RX+TX bytes across adapters, or -1, -1
// ("unknown") when there are none: the sum of nothing is $null, which [int64]
// turns into a 0 that reads as a measured "no traffic" (#919).
func sysReadNetworkBytes(ctx context.Context) (rx, tx int64) {
	total, ok := psInt64(ctx, `$s = @(Get-NetAdapterStatistics -ErrorAction SilentlyContinue); if ($s.Count -eq 0) { 'none' } else { [int64](($s | Measure-Object -Property ReceivedBytes -Sum).Sum) + [int64](($s | Measure-Object -Property SentBytes -Sum).Sum) }`)
	if !ok {
		return -1, -1
	}
	// The shared caller only uses rx+tx; put the whole total in rx.
	return total, 0
}

// sysReadDiskIOBytes returns cumulative disk bytes (read+written) across
// physical disks, or -1 ("unknown") when the counter is absent.
//
// DiskBytesPersec in the RAW perf class is a cumulative byte count despite its
// name, so the caller deltas it like /proc/diskstats. The _Total instance is
// missing when perf counters are disabled or corrupt, and [int64]$null is a 0
// that would read as a measured "no disk I/O" (#937, as #919 for network).
func sysReadDiskIOBytes(ctx context.Context) int64 {
	v, ok := psInt64(ctx, `$d = Get-CimInstance Win32_PerfRawData_PerfDisk_PhysicalDisk -ErrorAction SilentlyContinue | Where-Object {$_.Name -eq "_Total"}; if ($null -eq $d) { 'none' } else { [int64]$d.DiskBytesPersec }`)
	if !ok {
		return -1
	}
	return v
}

// sysIsProcessRunning reports whether a process with the given name is running.
func sysIsProcessRunning(ctx context.Context, name string) bool {
	// Get-Process matches without the .exe suffix.
	name = strings.TrimSuffix(strings.TrimSuffix(name, ".exe"), ".EXE")
	safe := strings.ReplaceAll(name, "'", "")
	out, err := psOutput(ctx, "if (Get-Process -Name '"+safe+"' -ErrorAction SilentlyContinue) { 'Y' } else { 'N' }")
	return err == nil && strings.TrimSpace(out) == "Y"
}

// sysCountActiveSessions counts interactive logon sessions (console + RDP) that
// are in the Active state with recent input. Parses `quser`, whose columns are
// USERNAME SESSIONNAME ID STATE IDLE-TIME LOGON-TIME. A session counts if STATE
// is Active and IDLE TIME is "none"/"." or under 5 minutes.
func sysCountActiveSessions(ctx context.Context) int {
	out, err := probeOutput(ctx, "quser")
	if err != nil {
		return 0
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) <= 1 {
		return 0
	}
	active := 0
	for _, line := range lines[1:] { // skip header
		fields := strings.Fields(line)
		// Expect at least: USERNAME [SESSIONNAME] ID STATE IDLE LOGON...
		if len(fields) < 4 {
			continue
		}
		// Find the STATE token (Active/Disc/Listen) and the IDLE token after it.
		stateIdx := -1
		for i, f := range fields {
			if f == "Active" || f == "Disc" {
				stateIdx = i
				break
			}
		}
		if stateIdx < 0 || fields[stateIdx] != "Active" {
			continue
		}
		idle := ""
		if stateIdx+1 < len(fields) {
			idle = fields[stateIdx+1]
		}
		if quserIdleIsRecent(idle) {
			active++
		}
	}
	return active
}

// quserIdleIsRecent reports whether a quser IDLE TIME token represents activity
// within the last 5 minutes. Tokens: "none"/"." (just active), "N" (minutes),
// "H:MM" (hours:minutes), "N+HH:MM" (days). Conservative: unknown → recent.
func quserIdleIsRecent(idle string) bool {
	idle = strings.TrimSpace(idle)
	switch idle {
	case "", "none", ".":
		return true
	}
	if strings.Contains(idle, "+") { // days present → not recent
		return false
	}
	if strings.Contains(idle, ":") { // H:MM
		parts := strings.SplitN(idle, ":", 2)
		h, e1 := strconv.Atoi(parts[0])
		if e1 == nil && h >= 1 {
			return false // ≥1 hour idle
		}
		return false // any H:MM ≥ a minute is not "recent" by our 5-min bar
	}
	// Plain minutes.
	if m, err := strconv.Atoi(idle); err == nil {
		return m < 5
	}
	return true // unknown format → assume recent (don't falsely idle)
}

// sysCountActivePortConnections counts ESTABLISHED TCP connections on the given
// local ports via Get-NetTCPConnection — catches browser app users (RStudio,
// Jupyter) the session list misses.
func sysCountActivePortConnections(ctx context.Context, ports []int) int {
	if len(ports) == 0 {
		return 0
	}
	portList := make([]string, len(ports))
	for i, p := range ports {
		portList[i] = strconv.Itoa(p)
	}
	script := "@(Get-NetTCPConnection -State Established -LocalPort " + strings.Join(portList, ",") +
		" -ErrorAction SilentlyContinue).Count"
	if c, ok := psInt64(ctx, script); ok {
		return int(c)
	}
	return 0
}

// sysWarnUsers broadcasts a message to interactive sessions via msg.exe.
// sysWriteConsole is a no-op on Windows: there is no /dev/console, and the EC2
// serial console for a Windows instance is not a writable character device
// (#736). A Windows workload's log has to travel by another route.
func sysWriteConsole(string) error { return nil }

func sysWarnUsers(ctx context.Context, message string) {
	_, _ = probeOutput(ctx, "msg", "*", message)
}

// sysShellCommand runs a user pre-stop hook through PowerShell on Windows. The
// user parameter (the Linux run-as-user, #63) is ignored: Windows instances run
// as a single user and have no `su` equivalent, so the root/$HOME mismatch the
// Linux path guards against doesn't arise here.
func sysShellCommand(ctx context.Context, command, user string) *exec.Cmd {
	_ = user
	return exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", command) // nosemgrep: dangerous-exec-command
}

// sysMountLustre is unsupported on Windows — FSx Lustre is a Linux-only mount
// path (#194). Windows instances don't use the ephemeral-FSx feature.
func sysMountLustre(ctx context.Context, dnsName, mountName, mountPoint string) error {
	_ = ctx
	_, _, _ = dnsName, mountName, mountPoint
	return errLustreUnsupported
}

// sysComputeClockPath is "" on Windows: there is no boot-scoped tmpfs to put the
// clock in, and a file under ProgramData would survive a stop/start and report
// the stopped interval as compute. `spored status` falls back to the
// spawn:compute-seconds checkpoint there, and says so (#896).
func sysComputeClockPath() string { return "" }
func sysBootTime() time.Time      { return time.Time{} }

// sysIdleStatePath is "" on Windows, which has no boot-scoped tmpfs either;
// `spored status` keeps its one-shot reading there and labels it (#1090).
func sysIdleStatePath() string { return "" }

// sysReloadMarkerPath is where the daemon acknowledges a reload (#1278), beside
// spored.log under %PROGRAMDATA%\spored. Unlike the compute clock it may outlive
// a boot: `spored reload` trusts it only when its pid is the running service's.
func sysReloadMarkerPath() string {
	base := os.Getenv("PROGRAMDATA")
	if base == "" {
		return ""
	}
	return filepath.Join(base, "spored", "reloaded.json")
}

// sysAcceleratorFamilies reports an NVIDIA GPU when nvidia-smi is installed: the
// NVIDIA driver installs it into System32, and Windows instances have no
// /sys to inspect. Other accelerators have no Windows driver on EC2 (#954).
func sysAcceleratorFamilies() []string {
	if _, err := exec.LookPath("nvidia-smi"); err == nil {
		return []string{accelNVIDIA}
	}
	return nil
}

// No durable compute checkpoint on Windows (#928), for the same reason as the
// compute clock: there is no boot id to tell a spored restart from a new boot.
// The spawn:compute-seconds tag, flushed on service stop, carries compute
// across a stop/start.
func sysComputeCheckpointPath() string { return "" }
func sysBootID() string                { return "" }

// sysKernelClocks reads the interrupt-time counts (#962), the Windows
// counterparts of CLOCK_MONOTONIC and CLOCK_BOOTTIME. Microsoft's documentation:
//
//   - QueryUnbiasedInterruptTime (kernel32, Windows 7 / Server 2008 R2): "Gets
//     the current unbiased interrupt-time count, in units of 100 nanoseconds.
//     The unbiased interrupt-time count does not include time the system spends
//     in sleep or hibernation." It "reflects only the time that the system is in
//     the working state." That is the Monotonic role.
//     https://learn.microsoft.com/en-us/windows/win32/api/realtimeapiset/nf-realtimeapiset-queryunbiasedinterrupttime
//   - QueryInterruptTime (api-ms-win-core-realtime-l1-1-1.dll, Windows 10 /
//     Server 2016; it is not a kernel32 export): "Gets the current
//     interrupt-time count", in 100ns units. This is the biased count, which the
//     unbiased one's page contrasts it with: the system uses it so "relative
//     timers that would have expired during sleep expire immediately upon
//     waking", so it keeps counting through sleep and hibernation. That is the
//     Boottime role, and it is also what Go's monotonic clock reads on Windows
//     (runtime nanotime, KUSER_SHARED_DATA.InterruptTime), which is why
//     time.Since counted hibernations as compute there.
//     https://learn.microsoft.com/en-us/windows/win32/api/realtimeapiset/nf-realtimeapiset-queryinterrupttime
//
// Both "begin at zero when the system starts", so they fit kernelClocks' {0, 0}
// origin. GetTickCount64 also includes sleep, but in milliseconds and from the
// tick count rather than the interrupt time, so subtracting the unbiased count
// from it would not be exact; it is not used.
//
// The unbiased count is read first, so the pair can overstate suspended time by
// the time between the calls, never understate running time. Where either proc
// is missing (Windows before Server 2016), this reports no clocks and compute is
// measured as before, through Go's monotonic reading.
//
// There is still no boot id or boot-scoped location on Windows, so the readings
// are never published or checkpointed: they make the daemon's own count exclude
// hibernation, which is what spawn:compute-seconds and --cost-limit read.
//
// Resume detection (#987) reads its suspended time through the same reader
// (readSuspendedTime), so with these clocks a Windows hibernate-and-resume also
// resets spored's per-boot state, as it does on Linux.
func sysKernelClocks() (kernelClocks, bool) {
	if procQueryUnbiasedInterruptTime.Find() != nil || procQueryInterruptTime.Find() != nil {
		return kernelClocks{}, false
	}
	var unbiased, biased uint64
	if r, _, _ := procQueryUnbiasedInterruptTime.Call(uintptr(unsafe.Pointer(&unbiased))); r == 0 {
		return kernelClocks{}, false
	}
	_, _, _ = procQueryInterruptTime.Call(uintptr(unsafe.Pointer(&biased))) // VOID return
	return interruptTimeClocks(unbiased, biased)
}

// Each proc is loaded from the DLL its documentation names. QueryInterruptTime
// is exported only by the realtime API set, so looking it up in kernel32 fails
// on every Windows version and silently disabled these clocks (#962).
var (
	kernel32                       = windows.NewLazySystemDLL("kernel32.dll")
	realtimeAPISet                 = windows.NewLazySystemDLL("api-ms-win-core-realtime-l1-1-1.dll")
	procQueryUnbiasedInterruptTime = kernel32.NewProc("QueryUnbiasedInterruptTime")
	procQueryInterruptTime         = realtimeAPISet.NewProc("QueryInterruptTime")
)
