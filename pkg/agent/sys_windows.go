//go:build windows

package agent

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
)

// Windows implementations of spored's OS-specific idle/metrics leaves (#77).
//
// These shell out to native Windows tools (PowerShell / quser) rather than
// linking a WMI binding, mirroring the Linux approach (which execs w/who/pgrep)
// and keeping the dependency surface zero. Every reader is conservative: on any
// error it returns the "assume active / unknown" sentinel so spored never
// falsely concludes a busy Windows desktop is idle.

// psOutput runs a PowerShell one-liner and returns trimmed stdout.
func psOutput(script string) (string, error) {
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// psInt64 runs a PowerShell one-liner expected to print a single integer.
func psInt64(script string) (int64, bool) {
	s, err := psOutput(script)
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
func sysReadCPUTimes() (idle, total int64, err error) {
	out, perr := psOutput(`$p = Get-CimInstance Win32_PerfRawData_PerfOS_Processor | Where-Object {$_.Name -eq "_Total"}; "$($p.PercentIdleTime) $($p.Timestamp_Sys100NS)"`)
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

// sysReadNetworkBytes returns cumulative RX+TX bytes across adapters.
func sysReadNetworkBytes() (rx, tx int64) {
	total, ok := psInt64(`[int64]((Get-NetAdapterStatistics | Measure-Object -Property ReceivedBytes -Sum).Sum) + [int64]((Get-NetAdapterStatistics | Measure-Object -Property SentBytes -Sum).Sum)`)
	if !ok {
		return -1, -1
	}
	// The shared caller only uses rx+tx; put the whole total in rx.
	return total, 0
}

// sysReadDiskIOBytes returns cumulative disk bytes (read+written) across disks.
func sysReadDiskIOBytes() int64 {
	v, ok := psInt64(`[int64]((Get-CimInstance Win32_PerfRawData_PerfDisk_PhysicalDisk | Where-Object {$_.Name -eq "_Total"}).DiskBytesPersec)`)
	if !ok {
		return 0
	}
	return v
}

// sysIsProcessRunning reports whether a process with the given name is running.
func sysIsProcessRunning(name string) bool {
	// Get-Process matches without the .exe suffix.
	name = strings.TrimSuffix(strings.TrimSuffix(name, ".exe"), ".EXE")
	safe := strings.ReplaceAll(name, "'", "")
	out, err := psOutput("if (Get-Process -Name '" + safe + "' -ErrorAction SilentlyContinue) { 'Y' } else { 'N' }")
	return err == nil && strings.TrimSpace(out) == "Y"
}

// sysCountActiveSessions counts interactive logon sessions (console + RDP) that
// are in the Active state with recent input. Parses `quser`, whose columns are
// USERNAME SESSIONNAME ID STATE IDLE-TIME LOGON-TIME. A session counts if STATE
// is Active and IDLE TIME is "none"/"." or under 5 minutes.
func sysCountActiveSessions() int {
	out, err := exec.Command("quser").Output()
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
func sysCountActivePortConnections(ports []int) int {
	if len(ports) == 0 {
		return 0
	}
	portList := make([]string, len(ports))
	for i, p := range ports {
		portList[i] = strconv.Itoa(p)
	}
	script := "@(Get-NetTCPConnection -State Established -LocalPort " + strings.Join(portList, ",") +
		" -ErrorAction SilentlyContinue).Count"
	if c, ok := psInt64(script); ok {
		return int(c)
	}
	return 0
}

// sysHasActiveTerminals reports interactive sessions present (console/RDP).
// On Windows this is equivalent to having any active quser session.
func sysHasActiveTerminals() bool {
	return sysCountActiveSessions() > 0
}

// sysHasRecentUserActivity reports recent interactive logon activity. We treat
// any Active session as recent activity (quser STATE), matching the
// conservative intent of the Linux wtmp check.
func sysHasRecentUserActivity() bool {
	return sysCountActiveSessions() > 0
}

// sysWarnUsers broadcasts a message to interactive sessions via msg.exe.
func sysWarnUsers(message string) {
	_ = exec.Command("msg", "*", message).Run()
}

// sysShellCommand runs a user pre-stop hook through PowerShell on Windows.
func sysShellCommand(ctx context.Context, command string) *exec.Cmd {
	return exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", command) // nosemgrep: dangerous-exec-command
}
