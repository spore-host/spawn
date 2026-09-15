package agent

import "testing"

func TestBuildWebReadyURL(t *testing.T) {
	got := buildWebReadyURL("box.5k0zfnmq.spore.host")
	want := "https://box.5k0zfnmq.spore.host/"
	if got != want {
		t.Errorf("buildWebReadyURL = %q, want %q", got, want)
	}
}

// TestProbeHTTP_Down confirms the probe reports "not up" when nothing is
// listening (a closed port). The "up" case is covered end-to-end by the
// real-instance validation, since it needs a live server.
func TestProbeHTTP_Down(t *testing.T) {
	// Port 1 is reserved and won't be listening in the test environment.
	if probeHTTP(1, "/") {
		t.Error("probeHTTP should report down for a port with no listener")
	}
}
