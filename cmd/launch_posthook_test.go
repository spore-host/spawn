package cmd

import (
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	spawnconfig "github.com/spore-host/spawn/pkg/config"
	"github.com/spore-host/spawn/pkg/platform"
)

// TestEndpointHostname covers the URL→hostname extraction preflightDNSEndpoint
// relies on, including the "can't pre-check this one" fallback for a malformed
// or hostless endpoint.
func TestEndpointHostname(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{"normal https URL", "https://f4gm19tl70.execute-api.us-east-1.amazonaws.com/prod/update-dns", "f4gm19tl70.execute-api.us-east-1.amazonaws.com"},
		{"with port", "https://api.example.com:8443/update-dns", "api.example.com"},
		{"malformed", "not a url \x7f", ""},
		{"hostless", "/relative/path", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := endpointHostname(tt.url); got != tt.want {
				t.Errorf("endpointHostname(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}

// TestIsPermanentResolutionFailure is the crux of #548: only a definitive
// NXDOMAIN-class failure (net.DNSError.IsNotFound) is "permanent" — a
// timeout, temporary failure, or non-DNSError error must NOT be treated as
// permanent, since those are exactly the transient early-boot resolver states
// the original 4-minute retry loop exists to ride out.
func TestIsPermanentResolutionFailure(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"NXDOMAIN (IsNotFound)", &net.DNSError{Err: "no such host", Name: "nx.invalid", IsNotFound: true}, true},
		{"timeout — transient", &net.DNSError{Err: "i/o timeout", Name: "example.com", IsTimeout: true}, false},
		{"temporary — transient", &net.DNSError{Err: "server misbehaving", Name: "example.com", IsTemporary: true}, false},
		{"plain non-DNSError error", errors.New("some other network error"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPermanentResolutionFailure(tt.err); got != tt.want {
				t.Errorf("isPermanentResolutionFailure(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// withFakeResolveHost temporarily replaces the package-level resolveHost hook
// and restores it on test cleanup.
func withFakeResolveHost(t *testing.T, fn func(host string) ([]string, error)) {
	t.Helper()
	orig := resolveHost
	resolveHost = fn
	t.Cleanup(func() { resolveHost = orig })
}

// TestPreflightDNSEndpoint_PermanentlyUnresolvable_ShortCircuits verifies the
// #548 fix's core behavior: a hostname that's NXDOMAIN on the controller
// returns an error from preflightDNSEndpoint WITHOUT any sleeping/retrying —
// this is the "no 4-minute wait, immediate non-fatal warning" requirement.
// The absence of a sleep loop is verified structurally: preflightDNSEndpoint
// contains no loop or timer at all, so a non-nil return here is definitionally
// immediate; we additionally assert wall-clock time stays well under a second
// to catch any future regression that adds blocking behavior.
func TestPreflightDNSEndpoint_PermanentlyUnresolvable_ShortCircuits(t *testing.T) {
	withFakeResolveHost(t, func(host string) ([]string, error) {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	})

	start := time.Now()
	err := preflightDNSEndpoint("https://stale-placeholder.invalid/update-dns")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("preflightDNSEndpoint() = nil, want an error for a permanently-unresolvable endpoint")
	}
	if !strings.Contains(err.Error(), "does not resolve") {
		t.Errorf("error = %v, want it to mention the hostname does not resolve", err)
	}
	if elapsed > time.Second {
		t.Errorf("preflightDNSEndpoint took %v — must return immediately, not wait out any deadline", elapsed)
	}
}

// TestPreflightDNSEndpoint_TransientFailure_DoesNotShortCircuit is the
// critical regression guard called out in the issue: a resolver that isn't
// up yet (timeout/temporary error) must NOT be classified as permanent, or
// the genuinely-transient early-boot case the original 4-minute retry loop
// protects (observed up to ~90s per the comment in registerDNS) would break.
func TestPreflightDNSEndpoint_TransientFailure_DoesNotShortCircuit(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"timeout", &net.DNSError{Err: "i/o timeout", Name: "api.spore.host", IsTimeout: true}},
		{"temporary", &net.DNSError{Err: "server misbehaving", Name: "api.spore.host", IsTemporary: true}},
		{"generic non-DNS error", errors.New("network is unreachable")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withFakeResolveHost(t, func(host string) ([]string, error) {
				return nil, tt.err
			})
			if err := preflightDNSEndpoint("https://api.spore.host/update-dns"); err != nil {
				t.Errorf("preflightDNSEndpoint() = %v, want nil (transient failure must fall through to the retry loop, not short-circuit)", err)
			}
		})
	}
}

// TestPreflightDNSEndpoint_Resolves_NoError confirms the common/happy path:
// a hostname that resolves fine never short-circuits.
func TestPreflightDNSEndpoint_Resolves_NoError(t *testing.T) {
	withFakeResolveHost(t, func(host string) ([]string, error) {
		return []string{"203.0.113.5"}, nil
	})
	if err := preflightDNSEndpoint("https://api.spore.host/update-dns"); err != nil {
		t.Errorf("preflightDNSEndpoint() = %v, want nil for a resolvable endpoint", err)
	}
}

// TestPreflightDNSEndpoint_MalformedEndpoint_NoError confirms a malformed or
// hostless endpoint is not treated as an error at this layer — there's
// nothing to pre-check, so it falls through to the normal (existing) retry
// behavior rather than blocking the launch on a config-parsing edge case.
func TestPreflightDNSEndpoint_MalformedEndpoint_NoError(t *testing.T) {
	called := false
	withFakeResolveHost(t, func(host string) ([]string, error) {
		called = true
		return nil, nil
	})
	if err := preflightDNSEndpoint(""); err != nil {
		t.Errorf("preflightDNSEndpoint(\"\") = %v, want nil", err)
	}
	if called {
		t.Error("resolveHost should not be called for a hostless endpoint")
	}
}

// TestShouldAttemptDNSRegistration is the #549 decision-table regression: it
// exercises the launch flow's DNS-registration guard directly, without
// running a real launch, covering the precedence rules the issue calls for —
// config disabled + no flag → skip; config enabled + --no-dns → skip (flag
// wins); nothing set → default proceeds; no DNS name at all → skip; SSH
// readiness not confirmed → skip (the pre-existing #56 behavior, now composed
// with the #549 check rather than replaced by it).
func TestShouldAttemptDNSRegistration(t *testing.T) {
	enabled := true
	disabled := false

	tests := []struct {
		name       string
		dnsName    string
		waitForSSH bool
		cfg        *spawnconfig.DNSConfig
		want       bool
	}{
		{
			name:       "no DNS name requested",
			dnsName:    "",
			waitForSSH: true,
			cfg:        &spawnconfig.DNSConfig{Enabled: &enabled},
			want:       false,
		},
		{
			name:       "SSH readiness not confirmed (#56)",
			dnsName:    "myhost",
			waitForSSH: false,
			cfg:        &spawnconfig.DNSConfig{Enabled: &enabled},
			want:       false,
		},
		{
			name:       "config disabled, no --no-dns flag involved → skip",
			dnsName:    "myhost",
			waitForSSH: true,
			cfg:        &spawnconfig.DNSConfig{Enabled: &disabled},
			want:       false,
		},
		{
			name:       "config enabled (nothing set) → default proceeds",
			dnsName:    "myhost",
			waitForSSH: true,
			cfg:        &spawnconfig.DNSConfig{Enabled: nil},
			want:       true,
		},
		{
			name:       "config explicitly enabled → proceeds",
			dnsName:    "myhost",
			waitForSSH: true,
			cfg:        &spawnconfig.DNSConfig{Enabled: &enabled},
			want:       true,
		},
		{
			name:       "nil dnsConfig (load failed) → skip",
			dnsName:    "myhost",
			waitForSSH: true,
			cfg:        nil,
			want:       false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldAttemptDNSRegistration(tt.dnsName, tt.waitForSSH, tt.cfg); got != tt.want {
				t.Errorf("shouldAttemptDNSRegistration(%q, %v, %+v) = %v, want %v",
					tt.dnsName, tt.waitForSSH, tt.cfg, got, tt.want)
			}
		})
	}
}

// TestRegisterDNS_PermanentlyUnresolvable_SkipsSSHEntirely is the end-to-end
// #548 regression at the registerDNS entry point: given a fake resolveHost
// that reports NXDOMAIN, registerDNS must return an error immediately
// without ever shelling out to ssh (which would otherwise burn up to 4
// minutes retrying). We can't easily assert "ssh was never invoked" without
// injecting the exec layer, but we CAN assert the call returns near-instantly
// and never sleeps — sufficient to prove the retry loop was never entered,
// since that loop's minimum body is a real 5s sleep per non-first iteration
// and a real ssh subprocess spawn per iteration.
func TestRegisterDNS_PermanentlyUnresolvable_SkipsSSHEntirely(t *testing.T) {
	withFakeResolveHost(t, func(host string) ([]string, error) {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	})

	plat := &platform.Platform{OS: "linux", HomeDir: t.TempDir()}
	start := time.Now()
	_, err := registerDNS(plat, "test-key", "i-deadbeef", "203.0.113.10", "test-record", "spore.host", "https://stale-placeholder.invalid/update-dns")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("registerDNS() = nil error, want a non-nil error for a permanently-unresolvable endpoint")
	}
	if !strings.Contains(err.Error(), "does not resolve") {
		t.Errorf("error = %v, want it to mention the hostname does not resolve", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("registerDNS took %v — must skip the SSH retry loop (which sleeps 5s between attempts) entirely for a permanently-unresolvable endpoint", elapsed)
	}
}
