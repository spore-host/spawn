package agent

import (
	"encoding/json"
	"strings"
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

// A Spot replacement's notification names the original instance (#1007), so
// the bot can reach the users registered for it; every other notification
// omits the field.
func TestNotifierOriginalInstanceID(t *testing.T) {
	base := NewNotifier(&provider.Config{NotifyURL: "https://x"}, &provider.Identity{InstanceID: "i-repl"})

	body, _ := json.Marshal(base.request("ttl_warning", "", ""))
	if strings.Contains(string(body), "original_instance_id") {
		t.Errorf("an ordinary notification carries original_instance_id: %s", body)
	}

	r := base.WithOriginalInstance("i-orig").request("spot_replacement_retired", "d", "")
	if r.OriginalInstanceID != "i-orig" || r.InstanceID != "i-repl" {
		t.Errorf("request = %+v, want original i-orig and instance i-repl", r)
	}
	body, _ = json.Marshal(r)
	if !strings.Contains(string(body), `"original_instance_id":"i-orig"`) {
		t.Errorf("wire body lacks original_instance_id: %s", body)
	}
	if base.originalInstanceID != "" {
		t.Error("WithOriginalInstance modified the receiver")
	}
	var nilN *Notifier
	if nilN.WithOriginalInstance("i-x") != nil {
		t.Error("WithOriginalInstance on nil must stay nil")
	}
}
