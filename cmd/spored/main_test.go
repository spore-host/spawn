package main

import (
	"os"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/agent"
)

// Every test starts with no daemon reading, so a test of buildStatusReport
// never picks up the live /run/spored/idle-state.json of a host that happens to
// be running spored (#1090). Tests of the daemon path set daemonIdleState
// themselves.
func TestMain(m *testing.M) {
	daemonIdleState = func(time.Time) (agent.IdleState, bool) { return agent.IdleState{}, false }
	os.Exit(m.Run())
}
