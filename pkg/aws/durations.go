package aws

import (
	"fmt"

	"github.com/spore-host/spawn/pkg/ttl"
)

// ValidateDurations checks every lifecycle duration in the config with
// pkg/ttl's shared parser — the one spored and the TTL reaper also use — and
// returns an error naming the first one that cannot be parsed (#990).
//
// Launch calls it before any AWS call, and callers that do AWS work of their
// own before Launch (security groups, FSx, SQS, IAM) should call it first too,
// so a typo fails in the shell rather than after half a launch. The TTL and the
// idle timeout must also be positive: a zero TTL is a deadline at the launch
// instant, and a zero idle timeout is no idle timeout, which with no TTL
// launches an instance nothing stops without the --no-timeout confirmation.
//
// Accepted: Go duration syntax plus a day unit ("8h", "90m", "1d", "2d12h").
func (c LaunchConfig) ValidateDurations() error {
	positive := []struct{ name, value string }{
		{"TTL", c.TTL},
		{"idle timeout", c.IdleTimeout},
		{"FSx TTL", c.FSxTTL},
	}
	for _, f := range positive {
		if f.value == "" {
			continue
		}
		if _, err := ttl.ParseTTL(f.value); err != nil {
			return fmt.Errorf("invalid %s %q: %w (use a duration such as 8h, 90m, 1d or 2d12h)", f.name, f.value, err)
		}
	}
	other := []struct{ name, value string }{
		{"pre-stop timeout", c.PreStopTimeout},
		{"completion delay", c.CompletionDelay},
		{"webhook timeout", c.WebhookTimeout},
	}
	for _, f := range other {
		if f.value == "" {
			continue
		}
		if _, err := ttl.ParseDuration(f.value); err != nil {
			return fmt.Errorf("invalid %s %q: %w (use a duration such as 30s, 5m or 1h)", f.name, f.value, err)
		}
	}
	return nil
}
