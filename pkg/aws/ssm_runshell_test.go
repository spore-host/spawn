package aws

import (
	"context"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/testutil"
)

// TestRunShellScript exercises the Linux SSM RunCommand path (used by
// `spawn connect` key-injection) against substrate, which models SendCommand +
// GetCommandInvocation. Skips if the substrate build doesn't.
func TestRunShellScript(t *testing.T) {
	env := testutil.SubstrateServer(t)
	c := NewClientFromConfig(env.AWSConfig)

	res, err := c.RunShellScript(context.Background(), "us-east-1", "i-0123456789abcdef0",
		"echo hello", 10*time.Second)
	if err != nil {
		t.Skipf("substrate does not model SSM RunCommand fully: %v", err)
	}
	if res == nil {
		t.Fatal("RunShellScript returned nil result without error")
	}
	// Substrate reports a terminal status; we just assert we got one back.
	if res.Status == "" {
		t.Error("RunShellScript returned an empty status")
	}
}

func TestRunShellScript_Timeout(t *testing.T) {
	env := testutil.SubstrateServer(t)
	c := NewClientFromConfig(env.AWSConfig)

	// A near-zero timeout should return promptly — either a completed result
	// (substrate finishes instantly) or a timeout error, never a hang.
	done := make(chan struct{})
	go func() {
		_, _ = c.RunShellScript(context.Background(), "us-east-1", "i-0123456789abcdef0",
			"sleep 1", time.Nanosecond)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("RunShellScript did not return within 15s for a nanosecond timeout")
	}
}
