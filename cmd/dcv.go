package cmd

import (
	"fmt"
	"strings"

	spawnclient "github.com/spore-host/spawn/pkg/aws"
)

// DCV ready-status values written by spored to the spawn:ready-status tag
// (mirrors the dcvStatus enum in pkg/agent/dcv.go). The CLI branches on these to
// report WHY a launch didn't stream instead of one opaque timeout (spawn#282).
// They're duplicated here (not imported) because the agent enum is unexported and
// the wire contract is the tag string, not a shared type.
const (
	dcvStatusNotInstalled      = "dcv-not-installed"
	dcvStatusServerNotRunning  = "dcvserver-not-running"
	dcvStatusSessionNotCreated = "session-never-created"
	dcvStatusTagWriteDenied    = "tag-write-denied"
	dcvStatusWaiting           = "dcv-waiting"
	dcvStatusReady             = "ready"
)

// extractReadyFromTags pulls the DCV handshake outcome out of an instance's tags:
// the ready URL, the auth token (parsed from the URL's authToken= query param),
// the host embedded in the URL (FQDN, may differ from the raw IP), and the
// spawn:ready-status. Pure, so the CLI's tag-parse is unit-tested without EC2
// (spawn#282). Any field is "" when absent.
func extractReadyFromTags(tags map[string]string) (url, token, host, status string) {
	status = tags["spawn:ready-status"]
	url = tags["spawn:ready-url"]
	if url == "" {
		return url, "", "", status
	}
	// authToken= query param — stop at & or # or end.
	if idx := strings.Index(url, "authToken="); idx >= 0 {
		raw := url[idx+len("authToken="):]
		if end := strings.IndexAny(raw, "&#"); end >= 0 {
			raw = raw[:end]
		}
		token = raw
	}
	// Host between https:// and :8443.
	if start := strings.Index(url, "https://"); start >= 0 {
		rest := url[start+len("https://"):]
		if end := strings.Index(rest, ":8443"); end >= 0 {
			host = rest[:end]
		}
	}
	return url, token, host, status
}

// dcvScanResult is one poll pass's reading of the target instance's DCV tags.
type dcvScanResult struct {
	token   string // spawn:ready-token (auth token); "" until ready
	host    string // FQDN from the ready-url, if present
	dnsName string // spawn:dns-name, if spored registered one
	status  string // spawn:ready-status (drives terminal/keep-polling)
}

// scanDCVReady finds the target instance in a ListInstances result and reads its
// DCV handshake tags. Extracted from the app-launch poll loop so the tag
// round-trip is exercisable against the Substrate emulator without a real GPU
// instance (spawn#282 phase 3). Returns a zero result if the instance isn't present.
func scanDCVReady(instances []spawnclient.InstanceInfo, instanceID string) dcvScanResult {
	for _, inst := range instances {
		if inst.InstanceID != instanceID {
			continue
		}
		_, token, host, status := extractReadyFromTags(inst.Tags)
		return dcvScanResult{
			token:   token,
			host:    host,
			dnsName: inst.Tags["spawn:dns-name"],
			status:  status,
		}
	}
	return dcvScanResult{}
}

// dcvStatusTerminal reports whether a ready-status is a final failure the CLI
// should stop polling on (vs. waiting/empty, which mean keep polling).
func dcvStatusTerminal(status string) bool {
	switch status {
	case dcvStatusNotInstalled, dcvStatusServerNotRunning, dcvStatusSessionNotCreated, dcvStatusTagWriteDenied:
		return true
	default:
		return false
	}
}

// dcvFailureMessage turns a ready-status into an actionable CLI line naming the
// failing layer and a remediation — replacing the single opaque
// "(timed out — DCV login screen will appear)" that made this feature churn.
func dcvFailureMessage(status, instanceID string) string {
	switch status {
	case dcvStatusNotInstalled:
		return " ✗ this AMI has no NICE DCV server installed — use a catalog app with a DCV AMI (paraview, chimerax), or build one (infra/amis)."
	case dcvStatusServerNotRunning:
		return fmt.Sprintf(" ✗ the DCV server failed to start — inspect it: spawn connect %s, then `systemctl status dcvserver` / `journalctl -u dcvserver`.", instanceID)
	case dcvStatusSessionNotCreated:
		return fmt.Sprintf(" ✗ DCV is up but the session was never created — check the app launch command: spawn connect %s, then /var/log/spored.log.", instanceID)
	case dcvStatusTagWriteDenied:
		return " ✗ spored couldn't write its ready tag (instance role missing ec2:CreateTags) — re-launch to refresh the role, or check the spored IAM policy."
	case dcvStatusWaiting, "":
		return fmt.Sprintf(" (timed out waiting for DCV — inspect with: spawn connect %s, then /var/log/spored.log)", instanceID)
	default:
		return fmt.Sprintf(" (DCV not ready: %s — inspect with: spawn connect %s)", status, instanceID)
	}
}
