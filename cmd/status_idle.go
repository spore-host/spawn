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
	if instance.Tags["spawn:terminate-on-idle"] == "true" {
		// `spawn service` and pool workers (#1151, #1146). Checked before the
		// one-time Spot case: a terminate needs no stoppable request.
		return "terminates the instance when idle"
	}
	if instance.SpotInstance && spotrequest.IsOneTime(instance.Tags) {
		return "cannot act: a one-time Spot instance can't be stopped, so idling does not stop it — " +
			"it runs until its termination deadline or 'spawn terminate'"
	}
	if instance.Tags["spawn:hibernate-on-idle"] == "true" {
		// #949: EC2 hibernates only an instance configured for it. Before then
		// --on-idle hibernate did not configure it, and spored's hibernate falls
		// back to a plain stop, so say what will really happen.
		if !instance.HibernationConfigured {
			return "stops the instance when idle: hibernation was requested, but the instance is not " +
				"configured for it, so RAM is not kept; never terminates"
		}
		return "hibernates the instance when idle; never terminates"
	}
	return "stops the instance when idle; never terminates"
}
