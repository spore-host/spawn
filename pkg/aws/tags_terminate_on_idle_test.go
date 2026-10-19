package aws

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// TerminateOnIdle reaches the instance as spawn:terminate-on-idle=true, the key
// spored reads (pkg/provider TestConfigFromTags_TerminateOnIdle) — #1151.
func TestBuildTags_TerminateOnIdle(t *testing.T) {
	tagValue := func(cfg LaunchConfig) (string, bool) {
		tags, err := buildTags(cfg, "123456789012", "arn:aws:iam::123456789012:user/u", "")
		if err != nil {
			t.Fatal(err)
		}
		for _, tg := range tags {
			if aws.ToString(tg.Key) == "spawn:terminate-on-idle" {
				return aws.ToString(tg.Value), true
			}
		}
		return "", false
	}
	if v, ok := tagValue(LaunchConfig{Name: "svc", IdleTimeout: "1h", TerminateOnIdle: true}); !ok || v != "true" {
		t.Errorf("spawn:terminate-on-idle = %q (present %v), want \"true\"", v, ok)
	}
	if _, ok := tagValue(LaunchConfig{Name: "x", IdleTimeout: "1h"}); ok {
		t.Error("spawn:terminate-on-idle written for a launch that did not ask for it")
	}
}

// An idle action of terminate never asks EC2 to stop the instance, so a Spot
// launch with one stays a one-time request; an idle stop still needs a
// persistent one (#922).
func TestSpotRequestType_TerminateOnIdleIsOneTime(t *testing.T) {
	if got := spotRequestType(LaunchConfig{Spot: true, IdleTimeout: "1h", TerminateOnIdle: true}); got != "one-time" {
		t.Errorf("terminate-on-idle Spot request type = %q, want one-time", got)
	}
	if got := spotRequestType(LaunchConfig{Spot: true, IdleTimeout: "1h"}); got != "persistent" {
		t.Errorf("idle-stop Spot request type = %q, want persistent", got)
	}
}
