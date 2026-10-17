package agent

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/provider"
)

// One monitor tick publishes the evaluation it made, for `spored status`,
// which cannot sample for itself (#1090). Without it every status screen read
// "unknown (single sample)" however long the daemon had been measuring.
func TestCheckAndAct_PublishesTheIdleEvaluation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idle-state.json")
	old := idleStatePath
	idleStatePath = func() string { return path }
	defer func() { idleStatePath = old }()
	noSessions(t)

	identity := &provider.Identity{InstanceID: "i-idle", Region: "us-east-1", Provider: "stub"}
	cfg := &provider.Config{IdleTimeout: time.Hour}
	a := &Agent{
		provider:              &stubProvider{identity: identity, config: cfg},
		identity:              identity,
		config:                cfg,
		startTime:             time.Now().Add(-time.Hour),
		lastActivityTime:      time.Now(),
		lastSessionTagWrite:   time.Now().Add(time.Hour),
		lastComputeTagWrite:   time.Now().Add(time.Hour),
		lastHeartbeatTagWrite: time.Now().Add(time.Hour),
	}
	before := time.Now()
	a.checkAndAct(context.Background())

	st, err := ReadIdleState(path)
	if err != nil {
		t.Fatalf("no idle state published after a monitor tick: %v", err)
	}
	snap := a.idleSnapshotNow()
	if st.Taken.Before(before) || !st.Taken.Equal(snap.taken) {
		t.Errorf("published taken=%v, want this tick's evaluation %v", st.Taken, snap.taken)
	}
	if st.Reason == "" || st.Reason != snap.reason || st.Idle != snap.idle {
		t.Errorf("published %+v, want the tick's decision (idle=%v, %q)", st, snap.idle, snap.reason)
	}
	if !st.Fresh(time.Now()) {
		t.Errorf("a state published just now is not fresh: %+v", st)
	}
}

// A state older than two ticks, or from the future, describes some other
// moment, and status must not present it as the instance now.
func TestIdleState_Fresh(t *testing.T) {
	now := time.Date(2026, 10, 17, 22, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		taken time.Time
		want  bool
	}{
		{now.Add(-61 * time.Second), true},
		{now.Add(-IdleStateFreshFor), true},
		{now.Add(-IdleStateFreshFor - time.Second), false},
		{now.Add(time.Minute), false},
		{time.Time{}, false},
	} {
		if got := (IdleState{Taken: c.taken}).Fresh(now); got != c.want {
			t.Errorf("Fresh(taken %v before now) = %v, want %v", now.Sub(c.taken), got, c.want)
		}
	}
}

func TestIdleState_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "idle-state.json")
	in := IdleState{
		Taken: time.Date(2026, 10, 17, 21, 48, 33, 0, time.UTC), Reason: "Not idle: Disk I/O 1204224 bytes",
		CPUPercent: 0.4, CPUMeasured: true, NetworkBytes: 41000, NetworkMeasured: true,
		DiskBytes: 1204224, DiskMeasured: true, LastActivity: time.Date(2026, 10, 17, 21, 48, 33, 0, time.UTC),
	}
	if err := WriteIdleState(path, in); err != nil {
		t.Fatal(err)
	}
	out, err := ReadIdleState(path)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Taken.Equal(in.Taken) || out.Reason != in.Reason || out.DiskBytes != in.DiskBytes || !out.DiskMeasured {
		t.Errorf("round trip: got %+v, want %+v", out, in)
	}
	if err := WriteIdleState("", in); err == nil {
		t.Error("writing with no path on this platform succeeded")
	}
}
