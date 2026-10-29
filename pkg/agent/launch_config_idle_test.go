package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/provider"
)

// #1359: while the launch config object this instance names has not loaded,
// its --active-ports, --active-processes and idle CPU threshold are unknown,
// so an untouched minute must not read as idle: an instance in use would be
// stopped for want of a setting spored could not read. Once it loads, the
// same minute is idle again.
func TestNoIdleActionWhileTheLaunchConfigIsUnavailable(t *testing.T) {
	noSessions(t)
	stubAccelerators(t, nil, func(string) ([]byte, error) { return nil, nil })
	quiet := idleSnapshot{taken: time.Now(), cpu: 0.5, cpuMeasured: true, netMeasured: true, diskMeasured: true}

	a := newTestAgent(t, &provider.Config{
		IdleCPUPercent:    5,
		LaunchConfigSHA:   strings.Repeat("a", 64),
		LaunchConfigError: "fetch launch config object: AccessDenied",
	})
	if idle, reason := a.idleDecision(context.Background(), quiet); idle || !strings.Contains(reason, "launch config object") {
		t.Errorf("idle=%v (%s) with the launch config object unavailable; want not idle, saying why", idle, reason)
	}

	a = newTestAgent(t, &provider.Config{IdleCPUPercent: 5, LaunchConfigSHA: strings.Repeat("a", 64)})
	if idle, reason := a.idleDecision(context.Background(), quiet); !idle {
		t.Errorf("the same minute with the object loaded read as active: %s", reason)
	}
}
