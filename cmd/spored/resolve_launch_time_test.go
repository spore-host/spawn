package main

import (
	"testing"
	"time"
)

// TestResolveLaunchTime is the spawn#508 regression test: a status run must
// never present the status-agent's own start time as the instance's age
// (Elapsed) without at least degrading gracefully through IMDS's
// PendingTime — which needs no IAM permission — before falling all the way
// back to "estimated". Before this fix, a #502-affected instance (DescribeTags
// denied) fell straight from "no LaunchTime tag" to the agent's own start
// time, so a 7h39m-old instance reported Elapsed: 0s.
func TestResolveLaunchTime(t *testing.T) {
	tag := time.Date(2026, 8, 17, 22, 26, 0, 0, time.UTC)
	pending := time.Date(2026, 8, 17, 22, 25, 58, 0, time.UTC)
	agentStart := time.Date(2026, 8, 18, 6, 5, 30, 0, time.UTC) // ~7h39m after pending

	t.Run("tag present => tag wins, regardless of the others", func(t *testing.T) {
		got, source := resolveLaunchTime(tag, pending, agentStart)
		if !got.Equal(tag) || source != launchTimeSourceTag {
			t.Errorf("got (%v, %q), want (%v, %q)", got, source, tag, launchTimeSourceTag)
		}
	})

	t.Run("tag absent, IMDS present => IMDS wins over the agent's own start (the core #508 fix)", func(t *testing.T) {
		got, source := resolveLaunchTime(time.Time{}, pending, agentStart)
		if !got.Equal(pending) || source != launchTimeSourceIMDS {
			t.Errorf("got (%v, %q), want (%v, %q) — must not fall through to the agent's own start when IMDS PendingTime is available", got, source, pending, launchTimeSourceIMDS)
		}
	})

	t.Run("both absent => falls back to the agent's own start, labelled as an estimate", func(t *testing.T) {
		got, source := resolveLaunchTime(time.Time{}, time.Time{}, agentStart)
		if !got.Equal(agentStart) || source != launchTimeSourceEstimated {
			t.Errorf("got (%v, %q), want (%v, %q)", got, source, agentStart, launchTimeSourceEstimated)
		}
	})
}
