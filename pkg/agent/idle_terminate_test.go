package agent

import (
	"context"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/provider"
)

// spawn:terminate-on-idle makes the idle action a terminate (#1151, #1146):
// `spawn service` and pool workers have nothing to be restarted for, and a
// stopped instance keeps billing its EBS volume. It wins over hibernate, and
// without it the idle action stays a stop — app sessions rely on that to be
// restartable (#1150).
func TestIdleActionHonoursTerminateOnIdle(t *testing.T) {
	cases := []struct {
		name                 string
		terminate, hibernate bool
		wantTerminated       bool
		wantStopped, wantHib bool
	}{
		{"terminate-on-idle", true, false, true, false, false},
		{"terminate-on-idle wins over hibernate", true, true, true, false, false},
		{"default stays a stop", false, false, false, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, sp, _ := idleAgent(t, &provider.Config{
				IdleTimeout: 30 * time.Minute, IdleCPUPercent: 5,
				TerminateOnIdle: tc.terminate, HibernateOnIdle: tc.hibernate,
			}, nil)
			a.regionVacatedSettle = time.Millisecond
			a.idleTimeoutReached(context.Background(), time.Now())
			if sp.terminated != tc.wantTerminated || sp.stopped != tc.wantStopped || sp.hibernated != tc.wantHib {
				t.Errorf("terminated=%v stopped=%v hibernated=%v, want %v/%v/%v",
					sp.terminated, sp.stopped, sp.hibernated, tc.wantTerminated, tc.wantStopped, tc.wantHib)
			}
		})
	}
}
