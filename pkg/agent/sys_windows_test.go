//go:build windows

package agent

import (
	"context"
	"testing"
	"time"
)

func TestQuserIdleIsRecent(t *testing.T) {
	cases := []struct {
		idle string
		want bool
	}{
		{"none", true},
		{".", true},
		{"", true},
		{"2", true},     // 2 minutes < 5
		{"4", true},     // 4 minutes < 5
		{"5", false},    // 5 minutes not < 5
		{"30", false},   // 30 minutes
		{"1:05", false}, // over an hour
		{"3+02:00", false},
		{"garbage", true}, // unknown → conservative
	}
	for _, tc := range cases {
		if got := quserIdleIsRecent(tc.idle); got != tc.want {
			t.Errorf("quserIdleIsRecent(%q) = %v, want %v", tc.idle, got, tc.want)
		}
	}
}

// The real Windows reader path, on a real PowerShell: cancelling the context
// stops a sample in flight (#1104). Before, psOutput used exec.Command and this
// waited for the full sleep.
func TestPsOutput_StopsWhenContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(time.Second, cancel)
	done := make(chan error, 1)
	go func() { _, err := psOutput(ctx, "Start-Sleep -Seconds 60; 'late'"); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled PowerShell sample reported success")
		}
	case <-time.After(probeReturnBound + time.Second):
		t.Fatal("psOutput did not return after its context was cancelled (#1104)")
	}
}
