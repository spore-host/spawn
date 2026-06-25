package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// dcvRunner abstracts the `dcv` CLI shell-outs so the handshake/idle logic is
// testable without a real DCV server (spawn#282). The real impl shells to `dcv`;
// tests inject a fake returning canned output/errors. installed() distinguishes
// "no DCV on this AMI" from "dcvserver not up yet" (a key named-failure split).
type dcvRunner interface {
	installed() bool
	listSessions(ctx context.Context) (string, error)
	describeSession(ctx context.Context, sessionID string) ([]byte, error)
}

// execDCVRunner is the production dcvRunner: it shells out to the `dcv` binary.
type execDCVRunner struct{}

func (execDCVRunner) installed() bool {
	_, err := exec.LookPath("dcv")
	return err == nil
}

func (execDCVRunner) listSessions(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "dcv", "list-sessions").Output()
	return string(out), err
}

func (execDCVRunner) describeSession(ctx context.Context, sessionID string) ([]byte, error) {
	return exec.CommandContext(ctx, "dcv", "describe-session", sessionID, "--json").Output()
}

// tagPutter abstracts the instance CreateTags call behind writeReadyTags so the
// DCV handshake retry/terminal logic is testable without real EC2 (spawn#282).
// The real impl is ec2TagPutter (in agent.go, where the AWS SDK wiring lives);
// tests inject a fake that records writes and can return a canned error (e.g. an
// AccessDenied to exercise the tag-write-denied path).
type tagPutter interface {
	putTags(ctx context.Context, instanceID string, tags map[string]string) error
}

// dcvStatus is the named outcome of the DCV readiness handshake, written to the
// spawn:ready-status tag so the CLI can report WHY a launch didn't stream rather
// than the single opaque "(timed out)" that made this feature churn (spawn#282).
//
// Terminal states are distinguishable failures; dcvWaiting is the transient
// "still working" value; dcvReady is success.
type dcvStatus string

const (
	dcvNotInstalled      dcvStatus = "dcv-not-installed"     // the `dcv` binary isn't on PATH (AMI has no DCV server)
	dcvServerNotRunning  dcvStatus = "dcvserver-not-running" // `dcv` exists but the daemon errors / isn't up
	dcvSessionNotCreated dcvStatus = "session-never-created" // dcvserver is up but the session never appeared
	dcvTagWriteDenied    dcvStatus = "tag-write-denied"      // spored couldn't write its ready tag (IAM)
	dcvWaiting           dcvStatus = "dcv-waiting"           // transient: still polling for the session
	dcvReady             dcvStatus = "ready"                 // session present, token issued
)

// terminal reports whether a status is a final failure the CLI should stop and
// report on (vs. dcvWaiting, which means keep polling, or dcvReady, success).
func (s dcvStatus) terminal() bool {
	switch s {
	case dcvNotInstalled, dcvServerNotRunning, dcvSessionNotCreated, dcvTagWriteDenied:
		return true
	default:
		return false
	}
}

// classifyDCVStatus decides the handshake status from the observable inputs,
// with no side effects — so it is unit-testable without a `dcv` binary or live
// EC2 (the spawn#282 testability fix). Inputs:
//   - dcvInstalled: was the `dcv` binary found on PATH?
//   - listErr:      error from `dcv list-sessions` (nil if it ran)
//   - listOutput:   stdout of `dcv list-sessions`
//   - sessionID:    the session we're waiting for
//   - exhausted:    has the bounded session-wait elapsed (no more polls)?
//
// While not exhausted and the session isn't present yet, it returns dcvWaiting
// so the caller keeps polling; once exhausted it returns the specific terminal
// reason instead of falling through and writing a ready-url for a session that
// may not exist (the old silent bug).
func classifyDCVStatus(dcvInstalled bool, listErr error, listOutput, sessionID string, exhausted bool) dcvStatus {
	if !dcvInstalled {
		return dcvNotInstalled
	}
	if listErr != nil {
		// dcvserver down / not answering. Transient early on; terminal once the
		// wait is exhausted.
		if exhausted {
			return dcvServerNotRunning
		}
		return dcvWaiting
	}
	if strings.Contains(listOutput, sessionID) {
		return dcvReady
	}
	if exhausted {
		return dcvSessionNotCreated
	}
	return dcvWaiting
}

// buildReadyURL constructs the DCV browser connect URL: the auth token rides the
// query string and the session id the URL hash, per AWS's external-auth spec.
// Pure (extracted from setupDCVAuth) so it's unit-tested.
func buildReadyURL(host, token, sessionID string) string {
	return fmt.Sprintf("https://%s:8443/?authToken=%s#%s", host, token, sessionID)
}

// parseDCVConnections extracts the connected-client count from
// `dcv describe-session --json` output. Pure; returns an error the caller maps
// to its "DCV not ready" signal (distinct from a real 0-connections idle).
func parseDCVConnections(jsonOut []byte) (int, error) {
	var result struct {
		NumConnections int `json:"num-of-connections"`
	}
	if err := json.Unmarshal(jsonOut, &result); err != nil {
		return 0, fmt.Errorf("parse dcv describe-session json: %w", err)
	}
	return result.NumConnections, nil
}

// dcvIdleDecision is the pure DCV-branch idle logic, extracted from isIdle so the
// bounded-grace behavior is unit-testable (spawn#282). It returns whether the
// instance is idle, and whether the caller should fall through to the standard
// CPU/network idle checks instead of trusting the DCV signal.
//
// Inputs:
//   - activityFileExists / activityAge: the kiosk-wm X11 activity file (the
//     accurate signal when DCV is up)
//   - connCount: DCV connected clients; <0 means "DCV not ready / unknown"
//   - sinceStart: how long spored has been running
//   - grace:      how long to tolerate a not-yet-ready DCV before giving up on it
//   - idleTimeout: the configured idle threshold
//
// Semantics:
//   - activity file present → trust it (idle iff activityAge >= idleTimeout).
//   - else connCount >= 0 → trust the connection count (idle iff 0 clients).
//   - else (connCount < 0, DCV not ready): within grace → not idle (startup);
//     past grace → fall through to standard checks so a never-ready DCV stops
//     billing forever (the old unbounded-grace cost leak).
func dcvIdleDecision(activityFileExists bool, activityAge time.Duration, connCount int, sinceStart, grace, idleTimeout time.Duration) (idle, fallThrough bool) {
	if activityFileExists {
		return activityAge >= idleTimeout, false
	}
	if connCount >= 0 {
		return connCount == 0, false
	}
	// DCV not ready / unknown.
	if sinceStart < grace {
		return false, false // startup grace — assume not idle, don't fall through
	}
	return false, true // grace exhausted — let standard idle checks decide
}
