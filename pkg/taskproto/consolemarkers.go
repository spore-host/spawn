package taskproto

import "strings"

// The markers framing a command-log tail written to the serial console.
//
// This is a WIRE FORMAT, not decoration. spored writes the block on a failed
// terminate (#736) because the log dies with the instance; `spawn logs` reads it
// back out of ec2 get-console-output afterwards. The two live in different
// packages — pkg/agent writes, cmd/ reads — and cmd does not import pkg/agent, so
// the markers live here, in the package both sides already share.
//
// Duplicating these as literals on each side is how a reader and a writer drift
// apart silently: the console output would still contain a block, and the reader
// would simply never find it. TestConsoleMarkersRoundTrip pins the pair.
const (
	ConsoleLogStartPrefix = "=== spawn command log"
	ConsoleLogEndMarker   = "=== end spawn command log ==="
)

// ExtractConsoleLog pulls the command-log block out of a raw console dump.
//
// Returns ok=false when the block is absent, which has two very different causes
// the caller must distinguish for the user:
//
//   - the job did not fail, so spored wrote nothing; or
//   - the job DID fail, but the post-termination console capture has not
//     populated yet — measured at ~4-5 minutes, during which the API returns an
//     empty or boot-only dump rather than a partial one.
//
// Reporting "no log" for the second case is the trap: an immediate read looks
// exactly like a missing log. ExtractConsoleLog cannot tell them apart; the
// caller has the instance's termination time and can.
//
// The start marker is matched by PREFIX because the written line carries a line
// count and a reason ("=== spawn command log (last 50 lines) — workload failed
// ===") that the reader must not have to predict.
func ExtractConsoleLog(consoleOutput string) (string, bool) {
	start := strings.Index(consoleOutput, ConsoleLogStartPrefix)
	if start < 0 {
		return "", false
	}
	// Begin after the start line, so the framing itself is not part of the body.
	nl := strings.IndexByte(consoleOutput[start:], '\n')
	if nl < 0 {
		return "", false
	}
	bodyStart := start + nl + 1

	end := strings.Index(consoleOutput[bodyStart:], ConsoleLogEndMarker)
	if end < 0 {
		// A start with no end means the console buffer truncated mid-block — the
		// capture is capped, and a long boot can evict the tail. Return what is
		// there rather than nothing: a partial log still answers "why did it
		// fail" more often than silence does.
		return strings.TrimRight(consoleOutput[bodyStart:], "\n"), true
	}
	return strings.TrimRight(consoleOutput[bodyStart:bodyStart+end], "\n"), true
}
