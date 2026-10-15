package cmd

import (
	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/spotrequest"
)

// idleActionDescription says what the idle timeout will actually do to this
// instance, for `spawn status`.
//
// It used to say "stops the instance when idle; never terminates" for every
// instance, including a Spot instance from a one-time request — which EC2
// refuses to stop (#922). On one of those spored's idle stop fails and the
// instance keeps running until something terminates it, so that is what is said.
// spawn now launches --spot with a stop-based lifecycle as persistent Spot, so a
// one-time Spot instance with an idle timeout is one launched before that, or by
// a launcher that still makes one-time requests.
func idleActionDescription(instance *aws.InstanceInfo) string {
	if instance.SpotInstance && spotrequest.IsOneTime(instance.Tags) {
		return "cannot act: a one-time Spot instance can't be stopped, so idling does not stop it — " +
			"it runs until its termination deadline or 'spawn terminate'"
	}
	if instance.Tags["spawn:hibernate-on-idle"] == "true" {
		return "hibernates the instance when idle; never terminates"
	}
	return "stops the instance when idle; never terminates"
}
