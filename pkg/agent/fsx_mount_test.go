package agent

import (
	"context"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/provider"
)

// TestMountPendingFSx_NoopWhenUnset verifies the goroutine returns immediately
// (no AWS calls) when no FSx is pending — the common case for every instance
// that doesn't use ephemeral FSx. If it didn't early-return it would try to load
// AWS config and hang/fail; the tight deadline guards against that.
func TestMountPendingFSx_NoopWhenUnset(t *testing.T) {
	a := &Agent{
		config:   &provider.Config{ /* FSxPending empty */ },
		identity: &provider.Identity{Region: "us-east-1", InstanceID: "i-test"},
	}
	done := make(chan struct{})
	go func() {
		a.mountPendingFSx(context.Background())
		close(done)
	}()
	select {
	case <-done:
		// returned promptly, as expected
	case <-time.After(2 * time.Second):
		t.Fatal("mountPendingFSx did not return promptly when no FSx is pending")
	}
}
