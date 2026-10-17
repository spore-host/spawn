package spotrequest

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// A replacement is an instance EC2 launched from a spawn persistent Spot request
// after the instance spawn launched from it was terminated outside spawn (#950,
// #984, #995). EC2 relaunches from the request's LaunchSpecification, which
// carries the UserData and the IamInstanceProfile but no TagSpecifications. So
// the replacement boots spawn's bootstrap, runs spored under spawn's role, and
// has no spawn tags: no TTL, nothing the reaper's tag-scoped terminate grant
// matches. The request itself is still spawn-tagged.
//
// spored detects that it is running on one and retires the instance: it cancels
// the request, which its tag-scoped grant allows, and shuts down from the OS.
// Detection uses only DescribeInstances and DescribeTags, which the spored role
// already grants unconditioned.

// managedTag is the tag every spawn launch writes on the instance and, for a
// persistent request, on the request. Literal rather than tagprefix-derived: it
// is the key the spored role's IAM conditions name, so it is the key that
// decides whether the cancel is allowed at all.
const managedTag = "spawn:managed"

// Verdict is what [DetectReplacement] concluded.
type Verdict int

const (
	// VerdictUnknown: something could not be read. Do nothing. The reaper's
	// SPOT REPLACEMENT NOT TERMINATED alarm still covers a real replacement.
	VerdictUnknown Verdict = iota
	// VerdictNotReplacement: this instance is not a replacement. Do nothing.
	VerdictNotReplacement
	// VerdictReplacement: this instance was launched by a spawn persistent
	// request for a different instance. Retire it.
	VerdictReplacement
)

func (v Verdict) String() string {
	switch v {
	case VerdictNotReplacement:
		return "not-replacement"
	case VerdictReplacement:
		return "replacement"
	default:
		return "unknown"
	}
}

// Detection is the result of [DetectReplacement].
type Detection struct {
	Verdict Verdict
	// RequestID is this instance's Spot request ("" if not Spot or unknown).
	RequestID string
	// OriginalInstanceID is the request's [OriginalInstanceTag] value, when read.
	OriginalInstanceID string
	// RequestTags are the request's tags, when read. The request carries the
	// original's launch tags, so they hold its notification settings.
	RequestTags map[string]string
	// Reason says why, for the log.
	Reason string
	// Err is the read error behind a VerdictUnknown, when there was one, so a
	// caller can tell an IAM refusal (no retry will help) from a transient
	// failure.
	Err error
}

// ReplacementAPI is the slice of the EC2 client [DetectReplacement] uses.
type ReplacementAPI interface {
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	DescribeTags(ctx context.Context, in *ec2.DescribeTagsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeTagsOutput, error)
}

// DetectReplacement decides whether instanceID (this instance) is a replacement
// launched from a leaked spawn persistent Spot request.
//
// It answers [VerdictReplacement] only when every one of these holds:
//
//   - this instance's own tags were read and do not include spawn:managed=true
//     (spawn tags every instance it launches, so a tagged instance is spawn's);
//   - it has a Spot request id;
//   - that request's tags were read, and include spawn:managed=true and
//     spawn:spot-request-type=persistent;
//   - the request's spawn:original-instance-id is an instance id, and not this
//     one.
//
// The original instance can never match. Its request's original-instance-id is
// either absent (spawn writes it just after RunInstances returns) or its own id,
// and an EC2 instance keeps its id across stop, start, hibernate and a Spot
// stop-interruption. It is also tagged spawn:managed=true, which on its own rules
// it out. Any read failure is [VerdictUnknown], which means do nothing.
func DetectReplacement(ctx context.Context, api ReplacementAPI, instanceID string) Detection {
	if instanceID == "" {
		return Detection{Verdict: VerdictUnknown, Reason: "own instance id unknown"}
	}
	out, err := api.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{instanceID}})
	if err != nil {
		return Detection{Verdict: VerdictUnknown, Reason: fmt.Sprintf("describe own instance: %v", err), Err: err}
	}
	var self *types.Instance
	for ri := range out.Reservations {
		for ii := range out.Reservations[ri].Instances {
			if aws.ToString(out.Reservations[ri].Instances[ii].InstanceId) == instanceID {
				self = &out.Reservations[ri].Instances[ii]
			}
		}
	}
	if self == nil {
		return Detection{Verdict: VerdictUnknown, Reason: "own instance not in DescribeInstances output"}
	}
	for _, t := range self.Tags {
		if aws.ToString(t.Key) == managedTag && aws.ToString(t.Value) == "true" {
			return Detection{Verdict: VerdictNotReplacement, Reason: "this instance is tagged spawn:managed=true"}
		}
	}

	reqID := aws.ToString(self.SpotInstanceRequestId)
	if reqID == "" {
		return Detection{Verdict: VerdictNotReplacement, Reason: "not a Spot instance"}
	}
	d := Detection{RequestID: reqID}

	tagsOut, err := api.DescribeTags(ctx, &ec2.DescribeTagsInput{
		Filters: []types.Filter{{Name: aws.String("resource-id"), Values: []string{reqID}}},
	})
	if err != nil {
		d.Verdict, d.Reason, d.Err = VerdictUnknown, fmt.Sprintf("read tags of Spot request %s: %v", reqID, err), err
		return d
	}
	tags := make(map[string]string, len(tagsOut.Tags))
	for _, t := range tagsOut.Tags {
		if aws.ToString(t.ResourceId) != reqID {
			continue
		}
		tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	d.RequestTags = tags
	if len(tags) == 0 {
		// Not a spawn request, or the tags are not readable (yet, or any more).
		d.Verdict, d.Reason = VerdictUnknown, fmt.Sprintf("Spot request %s has no readable tags", reqID)
		return d
	}
	if tags[managedTag] != "true" {
		d.Verdict, d.Reason = VerdictNotReplacement, fmt.Sprintf("Spot request %s is not spawn-managed", reqID)
		return d
	}
	if tags[TypeTag] != TypePersistent {
		d.Verdict, d.Reason = VerdictNotReplacement, fmt.Sprintf("Spot request %s is not a persistent spawn request", reqID)
		return d
	}

	orig, ok := tags[OriginalInstanceTag]
	d.OriginalInstanceID = orig
	switch {
	case !ok || orig == "":
		d.Verdict, d.Reason = VerdictNotReplacement, fmt.Sprintf("Spot request %s has no %s yet", reqID, OriginalInstanceTag)
	case orig == instanceID:
		d.Verdict, d.Reason = VerdictNotReplacement, "this is the instance spawn launched from the request"
	case !strings.HasPrefix(orig, "i-"):
		d.Verdict, d.Reason = VerdictUnknown, fmt.Sprintf("Spot request %s has a malformed %s %q", reqID, OriginalInstanceTag, orig)
	default:
		d.Verdict, d.Reason = VerdictReplacement, fmt.Sprintf("Spot request %s was launched for %s, not this instance", reqID, orig)
	}
	return d
}

// RetireAPI is what RetireReplacement needs: cancel the request, then terminate
// the instance.
type RetireAPI interface {
	CancelAPI
	TerminateInstances(ctx context.Context, in *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
}

// RetireOutcome is how a retired replacement ended.
type RetireOutcome int

const (
	// RetiredTerminated: the instance terminated itself. Nothing is left.
	RetiredTerminated RetireOutcome = iota + 1
	// RetiredShutdown: the self-terminate was refused (a role from before #1080)
	// and the OS was shut down instead. With EC2's default shutdown behaviour that
	// STOPS the instance: it no longer computes, but it bills its EBS volume and
	// nothing will end it, so it still needs terminating by hand.
	RetiredShutdown
)

// RetireReplacement ends a replacement: it cancels the request so EC2 launches
// nothing more from it, then terminates instanceID (this instance) through the
// API (#1080). The replacement has no spawn tags, so the tag-scoped terminate
// grant does not cover it; the spored role's TerminateOnlySelf statement does.
// Only if that is refused does it fall back to shutdown, an OS shutdown, which
// usually leaves the instance STOPPED, and it says so in the outcome. Before
// #1080 the shutdown was the only step, and every retired replacement was left
// stopped, untagged and billing.
//
// It refuses unless d is [VerdictReplacement], and it neither terminates nor
// shuts down when the cancel fails: a terminate with the request still open
// reopens it, and EC2 launches yet another replacement. A request that is
// already cancelled, or that EC2 no longer knows, cancels successfully.
func RetireReplacement(ctx context.Context, api RetireAPI, d Detection, instanceID string, shutdown func(context.Context) error) (RetireOutcome, error) {
	if d.Verdict != VerdictReplacement || d.RequestID == "" {
		return 0, fmt.Errorf("refusing to retire: verdict is %s (%s)", d.Verdict, d.Reason)
	}
	// Checked before the cancel: a retire that cannot name the instance to end
	// would cancel the request and then stop at the shutdown, which is #1080.
	if instanceID == "" {
		return 0, fmt.Errorf("refusing to retire: no instance id to terminate")
	}
	if err := Cancel(ctx, api, d.RequestID); err != nil {
		return 0, fmt.Errorf("not terminating: %w", err)
	}
	_, termErr := api.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{instanceID}})
	if termErr == nil {
		return RetiredTerminated, nil
	}
	if err := shutdown(ctx); err != nil {
		return 0, fmt.Errorf("cancelled Spot request %s; terminating %s was refused (%v) and the OS shutdown failed: %w",
			d.RequestID, instanceID, termErr, err)
	}
	return RetiredShutdown, fmt.Errorf("terminating %s was refused (%v), so the OS was shut down instead, which leaves "+
		"it STOPPED and billing its EBS volume", instanceID, termErr)
}
