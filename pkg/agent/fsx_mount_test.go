package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/provider"
)

// TestMaybeMountPendingFSx_GatingAndIdempotency is the #221 regression guard: the
// mount is triggered from the monitor loop (so a tag that lands after startup is
// picked up), but at most once. No pending tag → not started (re-checked next
// tick); pending tag → started exactly once.
//
// The mount itself is replaced by a counter. The real one polls the FSx API with
// the ambient credentials — it reached DescribeFileSystems from every `make
// check`, and kept polling in the background until the binary exited (#968).
func TestMaybeMountPendingFSx_GatingAndIdempotency(t *testing.T) {
	var starts atomic.Int32
	started := make(chan struct{}, 4)
	// No pending FSx: must not mark started (keeps re-checking on later ticks).
	a := &Agent{
		config:   &provider.Config{ /* FSxPending empty */ },
		identity: &provider.Identity{Region: "us-east-1", InstanceID: "i-test"},
		fsxMounter: func(context.Context) {
			starts.Add(1)
			started <- struct{}{}
		},
	}
	a.maybeMountPendingFSx(context.Background())
	if a.fsxMountStarted {
		t.Fatal("must not start the mount when no FSx is pending (must re-check later)")
	}

	// Pending FSx appears (e.g. tag landed on a later refresh): start exactly once.
	a.setConfig(&provider.Config{FSxPending: "fs-abc123"})
	a.maybeMountPendingFSx(context.Background())
	if !a.fsxMountStarted {
		t.Fatal("must start the mount once spawn:fsx-pending is observed")
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the mount goroutine was never started")
	}
	// A second call must be a no-op (no double-mount), regardless of config.
	a.maybeMountPendingFSx(context.Background())
	if !a.fsxMountStarted {
		t.Fatal("fsxMountStarted should remain set")
	}
	select {
	case <-started:
		t.Fatal("a second call started a second mount")
	case <-time.After(100 * time.Millisecond):
	}
	if n := starts.Load(); n != 1 {
		t.Fatalf("mount started %d times, want exactly 1", n)
	}
}

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
