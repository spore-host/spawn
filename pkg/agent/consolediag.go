package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/spore-host/spawn/pkg/taskproto"
)

const (
	// commandLogPath is where the bootstrap tees a --command workload's output.
	commandLogPath = "/var/log/spawn-command.log"

	// consoleTailLines is how much of it goes to the serial console.
	//
	// A budget, not a guess. EC2 caps console output (~64 KB) and a measured
	// baseline boot already consumes 13-17 KB of that, so dumping a whole log
	// would evict the cloud-init messages that are ALSO diagnostic — and those
	// are what explain a boot-time failure. The last 50 lines is where a
	// workload's own failure reason almost always is.
	consoleTailLines = 50
)

// writeCommandLogToConsole copies the tail of the command log to the serial
// console so it survives the instance (#736).
//
// The problem: when a --command job fails and the instance self-terminates,
// /var/log/spawn-command.log goes with it. The exit code survives in a tag; the
// REASON does not. A reporter hit exactly this — a job failed, the instance
// self-terminated, and there was no way to find out why.
//
// pkg/taskproto/flush.go already solves this for the TASK path by uploading the
// log to the results prefix. A plain `spawn launch --command … --on-complete
// terminate` has no results bucket, so nothing flushed. The console needs no
// bucket, no IAM grant and no new write path.
//
// Called only on a FAILED outcome: a successful job's log is rarely read, and
// console space is shared with the boot messages.
//
// Entirely best-effort. This runs while the instance is being torn down, so no
// failure here may block or fail the teardown; every error is logged and
// swallowed.
func (a *Agent) writeCommandLogToConsole(reason string) {
	tail, err := tailFile(commandLogPath, consoleTailLines)
	if err != nil {
		// Not a warning: a launch with no --command has no such file, which is
		// the common case and not a problem.
		log.Printf("console diagnostic: no %s to copy (%v)", commandLogPath, err)
		return
	}
	if strings.TrimSpace(tail) == "" {
		log.Printf("console diagnostic: %s is empty; nothing to copy", commandLogPath)
		return
	}

	// Framed so it is findable in a console dump that also holds the whole boot.
	var b strings.Builder
	fmt.Fprintf(&b, "\n%s (last %d lines) — %s ===\n", taskproto.ConsoleLogStartPrefix, consoleTailLines, reason)
	b.WriteString(tail)
	if !strings.HasSuffix(tail, "\n") {
		b.WriteString("\n")
	}
	b.WriteString(taskproto.ConsoleLogEndMarker + "\n")

	if err := sysWriteConsole(b.String()); err != nil {
		log.Printf("console diagnostic: could not write to the serial console: %v", err)
		return
	}
	// Say where to look, and say that it is not readable immediately — the
	// post-termination capture takes ~4-5 minutes to populate, measured.
	log.Printf("console diagnostic: copied the last %d lines of %s to the serial console; "+
		"read it with `aws ec2 get-console-output --instance-id <id>` (allow ~5 minutes "+
		"after termination for the capture to appear)", consoleTailLines, commandLogPath)
}

// tailFile returns the last n lines of a file.
//
// Reads forward and keeps a ring of the last n lines rather than seeking from
// the end: the command log is a few KB to a few MB, so a single pass is cheap,
// and a ring cannot mis-split a line the way a fixed-size tail read can. Getting
// a truncated first line into a console diagnostic would be a small bug in the
// one artifact someone is reading to understand a failure.
func tailFile(path string, n int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	ring := make([]string, 0, n)
	sc := bufio.NewScanner(f)
	// A workload can emit a very long line (a stack trace, a JSON blob); the
	// default 64 KB token limit would error the scan out and lose the tail.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if len(ring) == n {
			ring = ring[1:]
		}
		ring = append(ring, sc.Text())
	}
	if err := sc.Err(); err != nil {
		// Return what was collected: a partial tail beats none when the file is
		// the only record of why a job failed.
		if len(ring) > 0 {
			return strings.Join(ring, "\n") + "\n", nil
		}
		return "", err
	}
	if len(ring) == 0 {
		return "", nil
	}
	return strings.Join(ring, "\n") + "\n", nil
}

// completionFailed reports whether a completion record describes a failure.
//
// The record is written by the bootstrap as
// {"status": "completed"|"failed", "exit_code": N}. Parsed leniently on purpose:
// this only decides whether to copy a diagnostic to the console, so an
// unparseable or absent record must mean "no" rather than erroring a teardown
// path. A malformed record is itself worth seeing, but not at the cost of the
// terminate.
func completionFailed(record []byte) bool {
	if len(record) == 0 {
		return false
	}
	var r struct {
		Status   string `json:"status"`
		ExitCode *int   `json:"exit_code"`
	}
	if err := json.Unmarshal(record, &r); err != nil {
		return false
	}
	if strings.EqualFold(r.Status, "failed") {
		return true
	}
	// A non-zero exit code is a failure even if status says otherwise — the two
	// are written together and the code is the more specific fact.
	return r.ExitCode != nil && *r.ExitCode != 0
}
