package provider

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func tagDescs(kv ...string) []ec2types.TagDescription {
	var out []ec2types.TagDescription
	for i := 0; i+1 < len(kv); i += 2 {
		out = append(out, ec2types.TagDescription{Key: aws.String(kv[i]), Value: aws.String(kv[i+1])})
	}
	return out
}

// #990: spored read spawn:ttl with time.ParseDuration, so spawn:ttl=1d (what
// `spawn launch --ttl 1d` wrote) left Config.TTL zero: no TTL enforced on the
// instance. Every duration tag now goes through the shared parser.
func TestConfigFromTags_DayUnits(t *testing.T) {
	cfg, _ := configFromTags(tagDescs(
		"spawn:ttl", "1d",
		"spawn:idle-timeout", "2d12h",
		"spawn:pre-stop-timeout", "1.5h",
		"spawn:webhook-timeout", "2s",
		"spawn:completion-delay", "1d",
		"spawn:on-complete", "terminate",
	))
	checks := []struct {
		name      string
		got, want time.Duration
	}{
		{"spawn:ttl", cfg.TTL, 24 * time.Hour},
		{"spawn:idle-timeout", cfg.IdleTimeout, 60 * time.Hour},
		{"spawn:pre-stop-timeout", cfg.PreStopTimeout, 90 * time.Minute},
		{"spawn:webhook-timeout", cfg.WebhookTimeout, 2 * time.Second},
		{"spawn:completion-delay", cfg.CompletionDelay, 24 * time.Hour},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}
}

// Without a deadline tag, the agent synthesizes one from launch-time + TTL; the
// TTL reaper must reach the same deadline from the same tags
// (lambda/ttl-reaper's fallbackDeadline). Both rest on Config.TTL parsing "1d".
func TestConfigFromTags_TTLWithoutDeadline(t *testing.T) {
	cfg, _ := configFromTags(tagDescs("spawn:ttl", "1d", "spawn:launch-time", "2026-10-01T00:00:00Z"))
	if cfg.TTL != 24*time.Hour || !cfg.TTLDeadline.IsZero() {
		t.Fatalf("TTL=%v TTLDeadline=%v, want 24h and no deadline tag", cfg.TTL, cfg.TTLDeadline)
	}
	if got, want := cfg.LaunchTime.Add(cfg.TTL), time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("launch + ttl = %v, want %v", got, want)
	}
}
