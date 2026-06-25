package agent

import (
	"errors"
	"testing"
	"time"
)

func TestClassifyDCVStatus(t *testing.T) {
	listErr := errors.New("exit status 1")
	cases := []struct {
		name      string
		installed bool
		err       error
		out       string
		session   string
		exhausted bool
		want      dcvStatus
	}{
		{"not installed", false, nil, "", "console", false, dcvNotInstalled},
		{"not installed even when exhausted", false, nil, "", "console", true, dcvNotInstalled},
		{"server erroring, still polling", true, listErr, "", "console", false, dcvWaiting},
		{"server erroring, exhausted -> not running", true, listErr, "", "console", true, dcvServerNotRunning},
		{"session present -> ready", true, nil, "Session: 'console' (owner ec2-user)", "console", false, dcvReady},
		{"session present even at last poll -> ready", true, nil, "console", "console", true, dcvReady},
		{"running but session absent, still polling", true, nil, "no sessions", "console", false, dcvWaiting},
		{"running but session absent, exhausted -> never created", true, nil, "no sessions", "console", true, dcvSessionNotCreated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyDCVStatus(tc.installed, tc.err, tc.out, tc.session, tc.exhausted)
			if got != tc.want {
				t.Errorf("classifyDCVStatus = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDCVStatusTerminal(t *testing.T) {
	terminal := []dcvStatus{dcvNotInstalled, dcvServerNotRunning, dcvSessionNotCreated, dcvTagWriteDenied}
	for _, s := range terminal {
		if !s.terminal() {
			t.Errorf("%q should be terminal", s)
		}
	}
	for _, s := range []dcvStatus{dcvWaiting, dcvReady} {
		if s.terminal() {
			t.Errorf("%q should NOT be terminal", s)
		}
	}
}

func TestBuildReadyURL(t *testing.T) {
	got := buildReadyURL("box.5k0zfnmq.spore.host", "abc123", "console")
	want := "https://box.5k0zfnmq.spore.host:8443/?authToken=abc123#console"
	if got != want {
		t.Errorf("buildReadyURL = %q, want %q", got, want)
	}
}

func TestParseDCVConnections(t *testing.T) {
	n, err := parseDCVConnections([]byte(`{"num-of-connections":3,"id":"console"}`))
	if err != nil || n != 3 {
		t.Errorf("parseDCVConnections = (%d,%v), want (3,nil)", n, err)
	}
	if _, err := parseDCVConnections([]byte("not json")); err == nil {
		t.Error("expected error on malformed json")
	}
	// Absent field defaults to 0 (no connections).
	n, err = parseDCVConnections([]byte(`{"id":"console"}`))
	if err != nil || n != 0 {
		t.Errorf("missing field: got (%d,%v), want (0,nil)", n, err)
	}
}

func TestDCVIdleDecision(t *testing.T) {
	const idleTimeout = 20 * time.Minute
	const grace = 8 * time.Minute

	// Activity file present: trust it.
	if idle, ft := dcvIdleDecision(true, 5*time.Minute, -1, time.Hour, grace, idleTimeout); idle || ft {
		t.Errorf("recent activity: want (false,false), got (%v,%v)", idle, ft)
	}
	if idle, ft := dcvIdleDecision(true, 25*time.Minute, -1, time.Hour, grace, idleTimeout); !idle || ft {
		t.Errorf("stale activity: want (true,false), got (%v,%v)", idle, ft)
	}

	// No file, connection count known.
	if idle, ft := dcvIdleDecision(false, 0, 2, time.Hour, grace, idleTimeout); idle || ft {
		t.Errorf("2 clients: want (false,false), got (%v,%v)", idle, ft)
	}
	if idle, ft := dcvIdleDecision(false, 0, 0, time.Hour, grace, idleTimeout); !idle || ft {
		t.Errorf("0 clients: want (true,false), got (%v,%v)", idle, ft)
	}

	// DCV not ready (count<0): within grace → not idle, don't fall through;
	// past grace → fall through to standard checks (the bounded-grace cost fix).
	if idle, ft := dcvIdleDecision(false, 0, -1, 2*time.Minute, grace, idleTimeout); idle || ft {
		t.Errorf("not ready within grace: want (false,false), got (%v,%v)", idle, ft)
	}
	if idle, ft := dcvIdleDecision(false, 0, -1, 30*time.Minute, grace, idleTimeout); idle || !ft {
		t.Errorf("not ready past grace: want (false,true=fallthrough), got (%v,%v)", idle, ft)
	}
}
