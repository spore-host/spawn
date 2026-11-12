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

	// Web-UI app statuses (#590), mirrored from pkg/agent/webready.go.
	webStatusNotResponding = "web-not-responding" // terminal: app port never answered
	webStatusProxyFailed   = "web-proxy-failed"   // terminal: TLS reverse proxy couldn't start
)

// extractReadyFromTags pulls the readiness handshake outcome out of an
// instance's tags: the ready URL, the host embedded in it (FQDN, may differ
// from the raw IP), spawn:ready-status and spawn:ready-auth. Pure, so the CLI's
// tag-parse is unit-tested without EC2 (spawn#282). Any field is "" when absent.
//
// legacyToken is set only for an instance whose spored predates access-token
// registration (no spawn:ready-auth): that spored published the session token
// in spawn:ready-token and inside the ready-url. A current spored publishes no
// token anywhere, so nothing is read from those for it.
func extractReadyFromTags(tags map[string]string) (url, legacyToken, host, status, auth string) {
	status = tags["spawn:ready-status"]
	auth = tags[readyAuthTag]
	url = tags["spawn:ready-url"]
	if auth == "" {
		legacyToken = tags[legacyReadyTokenTag]
		for _, param := range []string{"authToken=", "spore_token="} {
			if legacyToken != "" {
				break
			}
			if idx := strings.Index(url, param); idx >= 0 {
				raw := url[idx+len(param):]
				if end := strings.IndexAny(raw, "&#"); end >= 0 {
					raw = raw[:end]
				}
				legacyToken = raw
			}
		}
	}
	// Host between https:// and the first ':' (port) or '/' (path). Handles both
	// the DCV ready-url (https://host:8443/...) and the web ready-url (https://host/).
	if start := strings.Index(url, "https://"); start >= 0 {
		rest := url[start+len("https://"):]
		if end := strings.IndexAny(rest, ":/"); end >= 0 {
			host = rest[:end]
		} else {
			host = rest
		}
	}
	return url, legacyToken, host, status, auth
}

// dcvScanResult is one poll pass's reading of the target instance's readiness tags.
type dcvScanResult struct {
	url         string // spawn:ready-url verbatim; token-free from a current spored
	legacyToken string // the token an older spored published (see extractReadyFromTags)
	host        string // FQDN from the ready-url, if present
	dnsName     string // spawn:dns-name, if spored registered one
	status      string // spawn:ready-status (drives terminal/keep-polling)
	auth        string // spawn:ready-auth: "token", "none", or "" (older spored)
}

// ready reports whether the session is up: a current spored says so in
// ready-status; an older DCV one only by publishing its token.
func (r dcvScanResult) ready() bool {
	return r.status == dcvStatusReady || r.legacyToken != ""
}

// scanDCVReady finds the target instance in a ListInstances result and reads its
// readiness tags. Extracted from the app-launch poll loop so the tag
// round-trip is exercisable against the Substrate emulator without a real GPU
// instance (spawn#282 phase 3). Returns a zero result if the instance isn't present.
func scanDCVReady(instances []spawnclient.InstanceInfo, instanceID string) dcvScanResult {
	for _, inst := range instances {
		if inst.InstanceID != instanceID {
			continue
		}
		url, legacy, host, status, auth := extractReadyFromTags(inst.Tags)
		return dcvScanResult{
			url:         url,
			legacyToken: legacy,
			host:        host,
			dnsName:     inst.Tags["spawn:dns-name"],
			status:      status,
			auth:        auth,
		}
	}
	return dcvScanResult{}
}

// dcvStatusTerminal reports whether a ready-status is a final failure the CLI
// should stop polling on (vs. waiting/empty, which mean keep polling).
func dcvStatusTerminal(status string) bool {
	switch status {
	case dcvStatusNotInstalled, dcvStatusServerNotRunning, dcvStatusSessionNotCreated, dcvStatusTagWriteDenied,
		webStatusNotResponding, webStatusProxyFailed:
		return true
	default:
		return false
	}
}

// dcvFailureMessage turns a ready-status into an actionable CLI line naming the
// failing layer and a remediation — replacing the single opaque
// "(timed out — DCV login screen will appear)" that made this feature churn.
//
// A DCV instance's inspect hint is `spawn connect <id> --session-manager`:
// plain connect opens the streaming session there, and the DCV security group
// has no port 22. A web app's security group does allow 22, so plain connect is
// a shell for those (#1153).
func dcvFailureMessage(status, instanceID string) string {
	switch status {
	case dcvStatusNotInstalled:
		return fmt.Sprintf(" ✗ the Amazon DCV server isn't installed — its boot-time install (spore-host#286/#389) didn't complete; inspect: spawn connect %s --session-manager, then /var/log/cloud-init-output.log.", instanceID)
	case dcvStatusServerNotRunning:
		return fmt.Sprintf(" ✗ the DCV server failed to start — inspect it: spawn connect %s --session-manager, then `systemctl status dcvserver` / `journalctl -u dcvserver`.", instanceID)
	case dcvStatusSessionNotCreated:
		return fmt.Sprintf(" ✗ DCV is up but the session was never created — check the app launch command: spawn connect %s --session-manager, then /var/log/spored.log.", instanceID)
	case dcvStatusTagWriteDenied:
		return " ✗ spored couldn't write its ready tag (instance role missing ec2:CreateTags) — re-launch to refresh the role, or check the spored IAM policy."
	case webStatusNotResponding:
		return fmt.Sprintf(" ✗ the web app never answered on its port — check the container: spawn connect %s, then `docker ps` / `docker logs`.", instanceID)
	case webStatusProxyFailed:
		return fmt.Sprintf(" ✗ the TLS reverse proxy couldn't start (likely the wildcard cert wasn't provisioned) — inspect: spawn connect %s, then /var/log/spored.log.", instanceID)
	case dcvStatusWaiting, "":
		return fmt.Sprintf(" (timed out waiting for DCV — inspect with: spawn connect %s --session-manager, then /var/log/spored.log)", instanceID)
	default:
		return fmt.Sprintf(" (DCV not ready: %s — inspect with: spawn connect %s --session-manager)", status, instanceID)
	}
}
