package plugin_test

import (
	"context"
	"testing"

	"github.com/spore-host/spawn/pkg/plugin"
)

// TestBufferingPushClient_RecordsValues verifies the buffering push client used
// by the unified install flow accumulates pushed values in memory instead of
// delivering them, so the CLI can hand them to spored with the install request.
func TestBufferingPushClient_RecordsValues(t *testing.T) {
	c := plugin.NewBufferingPushClient()
	ctx := context.Background()

	if err := c.Push(ctx, "myplugin", "setup_key", "abc"); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if err := c.Push(ctx, "myplugin", "other", "xyz"); err != nil {
		t.Fatalf("Push: %v", err)
	}

	got := c.Values()
	if got["setup_key"] != "abc" || got["other"] != "xyz" {
		t.Errorf("Values() = %v, want setup_key=abc other=xyz", got)
	}

	// Last write wins for a repeated key.
	if err := c.Push(ctx, "myplugin", "setup_key", "def"); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if c.Values()["setup_key"] != "def" {
		t.Errorf("repeated key not overwritten: got %q, want def", c.Values()["setup_key"])
	}
}

// TestBufferingPushClient_SatisfiesInterface ensures it is usable wherever a
// PushClient is expected.
func TestBufferingPushClient_SatisfiesInterface(t *testing.T) {
	var _ plugin.PushClient = plugin.NewBufferingPushClient()
}
