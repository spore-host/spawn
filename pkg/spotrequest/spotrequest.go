// Package spotrequest owns the two Spot-request facts every spawn lifecycle path
// has to agree on (#922):
//
//  1. Which kind of request a launch makes. A one-time request launches with
//     InstanceInterruptionBehavior=terminate, and EC2 refuses to stop such an
//     instance ("You can only stop Spot Instances associated with persistent Spot
//     Instance requests"). So a launch that asks for a stop or hibernate — the
//     idle timeout's default action, --on-idle, --on-complete stop|hibernate,
//     --hibernate — needs a PERSISTENT request. [NeedsStoppable] decides.
//
//  2. That a persistent request must be cancelled when its instance is
//     terminated. Terminating the instance alone reopens the request and EC2
//     launches a replacement — on every termination path: the CLI, spored's own
//     TTL / on-complete / cost-limit terminate, and the TTL reaper. [Cancel] and
//     [Terminate] are the shared implementation, so no path can forget it.
//
// Internal by convention (not in the public-surface table): pkg/aws, pkg/provider
// (spored) and lambda/ttl-reaper import it.
package spotrequest

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"

	"github.com/spore-host/spawn/pkg/ttl"
)

// TypeTag records which kind of Spot request a spawn launch made. Written at
// RunInstances on every spawn Spot launch; absent on instances launched before
// #922, which were all one-time.
const TypeTag = "spawn:spot-request-type"

// Values of [TypeTag].
const (
	TypePersistent = "persistent"
	TypeOneTime    = "one-time"
)

// NeedsStoppable reports whether a Spot launch with these lifecycle settings
// must use a persistent request, because something will ask EC2 to stop or
// hibernate the instance:
//
//   - an idle timeout: the idle action is stop (or hibernate with
//     --on-idle hibernate); the idle daemon never terminates;
//   - --on-complete stop or hibernate;
//   - --hibernate (configured for hibernation so `spawn hibernate` can use it).
//
// A terminate-only lifecycle (TTL, --on-complete terminate, cost limit) stays
// one-time: a one-time request closes with its instance, so there is nothing to
// cancel and nothing that can relaunch.
func NeedsStoppable(idleTimeout, onComplete string, hibernate, hibernateOnIdle bool) bool {
	if hibernate {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(onComplete)) {
	case "stop", "hibernate":
		return true
	}
	return idleTimeoutSet(idleTimeout)
}

// idleTimeoutSet mirrors how spored reads spawn:idle-timeout: a duration that
// parses to more than zero arms the idle monitor; "", "0" or garbage does not
// (pkg/provider/ec2.go ignores an unparseable value). It parses with pkg/ttl,
// spored's parser since #990, so a day-unit idle timeout ("1d") also asks for a
// stoppable request.
func idleTimeoutSet(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}
	d, err := ttl.ParseDuration(v)
	return err == nil && d > 0
}

// IsOneTime reports whether a Spot instance's request is one-time, from its
// tags. An instance without [TypeTag] predates #922, when every spawn Spot
// launch was one-time, so absence means one-time.
func IsOneTime(tags map[string]string) bool {
	return tags[TypeTag] != TypePersistent
}

// CancelAPI is the slice of the EC2 client [Cancel] uses.
type CancelAPI interface {
	CancelSpotInstanceRequests(ctx context.Context, in *ec2.CancelSpotInstanceRequestsInput, optFns ...func(*ec2.Options)) (*ec2.CancelSpotInstanceRequestsOutput, error)
}

// Cancel cancels one Spot request. An empty id is a no-op (the instance is not
// Spot), and a request EC2 no longer knows is already gone, which is success.
//
// Cancelling never terminates the instance; it only stops the request from
// launching a replacement. Cancelling a one-time request whose instance is still
// running is harmless for the same reason, which is why callers may cancel
// whatever request id an instance carries without first asking which kind it is.
func Cancel(ctx context.Context, api CancelAPI, requestID string) error {
	if requestID == "" {
		return nil
	}
	_, err := api.CancelSpotInstanceRequests(ctx, &ec2.CancelSpotInstanceRequestsInput{
		SpotInstanceRequestIds: []string{requestID},
	})
	if err != nil {
		if strings.Contains(err.Error(), "InvalidSpotInstanceRequestID.NotFound") {
			return nil
		}
		return fmt.Errorf("cancel spot request %s: %w", requestID, err)
	}
	return nil
}

// TerminateAPI is the slice of the EC2 client [Terminate] uses.
type TerminateAPI interface {
	CancelAPI
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	TerminateInstances(ctx context.Context, in *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
}

// Lookup returns the Spot request id the instance was launched from ("" for an
// on-demand instance) and its tags.
func Lookup(ctx context.Context, api TerminateAPI, instanceID string) (string, map[string]string, error) {
	out, err := api.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{instanceID}})
	if err != nil {
		return "", nil, err
	}
	for _, res := range out.Reservations {
		for _, inst := range res.Instances {
			if aws.ToString(inst.InstanceId) != instanceID {
				continue
			}
			tags := make(map[string]string, len(inst.Tags))
			for _, t := range inst.Tags {
				tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
			}
			return aws.ToString(inst.SpotInstanceRequestId), tags, nil
		}
	}
	return "", nil, nil
}

// Mode says what [Terminate] does when the Spot request cannot be cancelled.
type Mode int

const (
	// BestEffort terminates anyway and reports the cancel failure through warn.
	// For the unattended paths (spored at its TTL, the reaper): an instance past
	// its deadline must die, and refusing would leave it billing indefinitely.
	BestEffort Mode = iota

	// Strict refuses to terminate an instance whose PERSISTENT request could not
	// be cancelled, because terminating it would make EC2 launch a replacement.
	// For the CLI, where a person can fix the permission or cancel by hand and
	// re-run. A one-time request closes with its instance, so a failure to cancel
	// one is still only a warning.
	Strict
)

// Terminate cancels the instance's Spot request, if it has one, and then
// terminates the instance. The order matters: terminating first reopens a
// persistent request, which launches a replacement before a later cancel lands.
//
// warn receives non-fatal problems (a failed lookup or cancel that did not stop
// the terminate); nil discards them.
func Terminate(ctx context.Context, api TerminateAPI, region, instanceID string, mode Mode, warn func(string)) error {
	if warn == nil {
		warn = func(string) {}
	}

	requestID, tags, err := Lookup(ctx, api, instanceID)
	if err != nil {
		if mode == Strict && !strings.Contains(err.Error(), "InvalidInstanceID.NotFound") {
			return fmt.Errorf("look up %s's spot request before terminating (a persistent request "+
				"left open would launch a replacement): %w", instanceID, err)
		}
		// NotFound falls through so TerminateInstances reports it in the shape
		// callers already match on.
		warn(fmt.Sprintf("could not look up %s's spot request before terminating: %v", instanceID, err))
	}

	if err := Cancel(ctx, api, requestID); err != nil {
		persistent := !IsOneTime(tags)
		hint := fmt.Sprintf("aws ec2 cancel-spot-instance-requests --region %s --spot-instance-request-ids %s",
			region, requestID)
		if mode == Strict && persistent {
			return fmt.Errorf("not terminating %s: its persistent Spot request %s could not be cancelled, "+
				"and terminating would make EC2 launch a replacement instance.\n"+
				"  Cancel it first, then re-run:  %s\n"+
				"  (needs ec2:CancelSpotInstanceRequests)\n  cause: %w", instanceID, requestID, hint, err)
		}
		if persistent {
			warn(fmt.Sprintf("terminating %s, but its persistent Spot request %s could not be cancelled and "+
				"may launch a replacement — cancel it with: %s (%v)", instanceID, requestID, hint, err))
		} else {
			warn(fmt.Sprintf("could not cancel %s's one-time Spot request %s (it closes with the instance): %v",
				instanceID, requestID, err))
		}
	}

	_, err = api.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{instanceID}})
	return err
}
