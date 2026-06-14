package agent

import (
	"testing"

	"github.com/spore-host/spawn/pkg/provider"
)

// TestNewNotifier_PlatformDefault verifies the notify platform comes from
// config and defaults to "slack" for back-compat when unset (#2).
func TestNewNotifier_PlatformDefault(t *testing.T) {
	id := &provider.Identity{InstanceID: "i-123", Region: "us-east-1"}

	cases := []struct {
		name string
		cfg  *provider.Config
		want string
	}{
		{"default when unset", &provider.Config{NotifyURL: "https://x"}, "slack"},
		{"discord", &provider.Config{NotifyURL: "https://x", NotifyPlatform: "discord"}, "discord"},
		{"teams", &provider.Config{NotifyURL: "https://x", NotifyPlatform: "teams"}, "teams"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := NewNotifier(tc.cfg, id)
			if n == nil {
				t.Fatal("expected a Notifier (NotifyURL set)")
			}
			if n.platform != tc.want {
				t.Errorf("platform = %q, want %q", n.platform, tc.want)
			}
		})
	}
}

// TestNewNotifier_NilWhenNoURL confirms an empty NotifyURL yields a nil (no-op)
// Notifier regardless of platform.
func TestNewNotifier_NilWhenNoURL(t *testing.T) {
	if n := NewNotifier(&provider.Config{NotifyPlatform: "discord"}, &provider.Identity{}); n != nil {
		t.Error("expected nil Notifier when NotifyURL is empty")
	}
}
