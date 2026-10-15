package agent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/provider"
)

func clockQueryAgent(t *testing.T, base int64) *Agent {
	t.Helper()
	p := queryProvider()
	p.config = &provider.Config{ComputeSeconds: base}
	a, err := NewAgentForQuery(context.Background(), p)
	if err != nil {
		t.Fatalf("NewAgentForQuery: %v", err)
	}
	return a
}

// A query agent is a fresh process: until it adopts the daemon's clock its
// compute is only the checkpoint tag, and it must say so rather than present the
// checkpoint as running time (#896).
func TestComputeClock_QueryAgentIsNotLiveUntilItAdopts(t *testing.T) {
	a := clockQueryAgent(t, 0)
	if a.ComputeClockLive() {
		t.Fatal("a fresh query agent claims a live compute clock")
	}
	if got := a.TotalComputeSeconds(); got != 0 {
		t.Fatalf("TotalComputeSeconds = %d before adoption, want the checkpoint (0)", got)
	}

	path := filepath.Join(t.TempDir(), "clock.json")
	daemon := ComputeClock{InstanceID: "i-0123456789abcdef0", StartedAt: time.Now().Add(-90 * time.Second), BaseSeconds: 600}
	if err := WriteComputeClock(path, daemon); err != nil {
		t.Fatal(err)
	}
	c, err := ReadComputeClock(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.AdoptComputeClock(c, time.Now().Add(-10*time.Minute)); err != nil {
		t.Fatalf("AdoptComputeClock: %v", err)
	}
	if !a.ComputeClockLive() {
		t.Error("adopted clock is not reported live")
	}
	if got := a.TotalComputeSeconds(); got < 689 || got > 692 {
		t.Errorf("TotalComputeSeconds = %d, want base 600 + ~90s the daemon has been up", got)
	}
	if !a.HasPriorComputeTime() {
		t.Error("the daemon's carried-over base was not adopted")
	}
}

// The daemon itself is the clock.
func TestComputeClock_DaemonIsLive(t *testing.T) {
	p := queryProvider()
	p.config = &provider.Config{}
	a, err := newAgent(context.Background(), p, true)
	if err != nil {
		t.Fatal(err)
	}
	if !a.ComputeClockLive() {
		t.Error("the activated daemon agent does not report a live clock")
	}
}

// Adopting a clock that does not describe this boot of this instance would
// count stopped time as compute — so each of these must be refused, and refusing
// must leave the agent on its checkpoint.
func TestComputeClock_RefusesAClockFromAnotherBootOrInstance(t *testing.T) {
	now := time.Now()
	booted := now.Add(-5 * time.Minute)
	cases := map[string]ComputeClock{
		"other instance": {InstanceID: "i-ffffffffffffffff0", StartedAt: now.Add(-time.Minute)},
		"predates boot":  {InstanceID: "i-0123456789abcdef0", StartedAt: booted.Add(-time.Hour)},
		"in the future":  {InstanceID: "i-0123456789abcdef0", StartedAt: now.Add(time.Hour)},
		"no start":       {InstanceID: "i-0123456789abcdef0"},
		"negative base":  {InstanceID: "i-0123456789abcdef0", StartedAt: now.Add(-time.Minute), BaseSeconds: -5},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			a := clockQueryAgent(t, 42)
			if err := a.AdoptComputeClock(c, booted); err == nil {
				t.Fatal("adopted a clock that cannot describe this boot")
			}
			if a.ComputeClockLive() || a.TotalComputeSeconds() != 42 {
				t.Errorf("a refused clock changed the agent: live=%v total=%d", a.ComputeClockLive(), a.TotalComputeSeconds())
			}
		})
	}
}

func TestComputeClock_NoPathIsAnError(t *testing.T) {
	if err := WriteComputeClock("", ComputeClock{}); err == nil || !strings.Contains(err.Error(), "no compute clock path") {
		t.Errorf("WriteComputeClock(\"\") = %v", err)
	}
	if _, err := ReadComputeClock(""); err == nil {
		t.Error("ReadComputeClock(\"\") succeeded")
	}
}
