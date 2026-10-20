package aws

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/spore-host/spawn/pkg/elasticip"
)

// A multi-interface EFA launch (#661) cannot ask EC2 for a public IP: EC2
// auto-assigns one only to an instance with a single network interface. So
// Launch allocates an Elastic IP once the instance is running, associates it to
// the primary interface (network card 0, device 0), and the address is released
// again by `spawn terminate` and, for instances that end any other way (spored's
// TTL/idle/cost-limit self-terminate, the console), by the TTL reaper's
// orphan-EIP sweep. An unassociated Elastic IP bills by the hour, which is why
// every one of these carries the instance it was allocated for.

// ElasticIPInstanceTagKey is the tag on a spawn-allocated Elastic IP naming the
// instance it was allocated for. With spawn:managed=true it is what marks an
// address as spawn's to release: an Elastic IP without it is user-owned and
// spawn never releases it (#262).
const ElasticIPInstanceTagKey = elasticip.InstanceIDTagKey

// ElasticIPPurpose is the spawn:purpose of a launch Elastic IP.
const ElasticIPPurpose = elasticip.Purpose

// launchElasticIPAPI is the slice of EC2 the launch Elastic IP needs. *ec2.Client
// satisfies it; tests fake it.
type launchElasticIPAPI interface {
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	elasticIPReleaseAPI
	AllocateAddress(ctx context.Context, in *ec2.AllocateAddressInput, optFns ...func(*ec2.Options)) (*ec2.AllocateAddressOutput, error)
	AssociateAddress(ctx context.Context, in *ec2.AssociateAddressInput, optFns ...func(*ec2.Options)) (*ec2.AssociateAddressOutput, error)
}

// elasticIPReleaseAPI is the slice of EC2 releasing an instance's Elastic IPs
// needs.
type elasticIPReleaseAPI interface {
	DescribeAddresses(ctx context.Context, in *ec2.DescribeAddressesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeAddressesOutput, error)
	DisassociateAddress(ctx context.Context, in *ec2.DisassociateAddressInput, optFns ...func(*ec2.Options)) (*ec2.DisassociateAddressOutput, error)
	ReleaseAddress(ctx context.Context, in *ec2.ReleaseAddressInput, optFns ...func(*ec2.Options)) (*ec2.ReleaseAddressOutput, error)
}

// Poll cadence and budgets. Package vars so tests do not pay wall-clock time.
var (
	eipPollInterval = 3 * time.Second
	// eipRunningTimeout bounds the wait for running. Large multi-card types
	// (p5en.48xlarge) take longer than a t3 to leave pending.
	eipRunningTimeout = 10 * time.Minute
	// eipRetryBackoff is the schedule for the eventual-consistency errors an
	// association or release right after a state change can hit.
	eipRetryBackoff = []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}
)

func eipSleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func awsErrorCodeIn(err error, codes ...string) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, c := range codes {
		if strings.Contains(msg, c) {
			return true
		}
	}
	return false
}

// primaryInterfaceID returns the ID of the interface at network card 0, device
// index 0. An attachment that omits NetworkCardIndex is on card 0.
func primaryInterfaceID(inst types.Instance) string {
	for _, ni := range inst.NetworkInterfaces {
		if ni.Attachment == nil {
			continue
		}
		if aws.ToInt32(ni.Attachment.NetworkCardIndex) == 0 && aws.ToInt32(ni.Attachment.DeviceIndex) == 0 {
			return aws.ToString(ni.NetworkInterfaceId)
		}
	}
	return ""
}

// waitForPrimaryInterface waits for the instance to be running and returns its
// primary interface ID. A NotFound right after RunInstances is eventual
// consistency and retried; an instance that ends instead (capacity, a Spot
// interruption) is an error at once.
func waitForPrimaryInterface(ctx context.Context, api launchElasticIPAPI, instanceID string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	last := "not yet visible"
	for {
		out, err := api.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{instanceID}})
		switch {
		case err != nil && !awsErrorCodeIn(err, "InvalidInstanceID.NotFound"):
			return "", fmt.Errorf("describe instance %s: %w", instanceID, err)
		case err == nil:
			for _, r := range out.Reservations {
				for _, inst := range r.Instances {
					state := types.InstanceStateNamePending
					if inst.State != nil {
						state = inst.State.Name
					}
					last = string(state)
					switch state {
					case types.InstanceStateNameRunning:
						if id := primaryInterfaceID(inst); id != "" {
							return id, nil
						}
						last = "running, primary interface not yet reported"
					case types.InstanceStateNamePending:
					default:
						return "", fmt.Errorf("instance %s is %s, not running", instanceID, state)
					}
				}
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("instance %s not running with a primary interface within %s (last: %s)", instanceID, timeout, last)
		}
		if err := eipSleep(ctx, eipPollInterval); err != nil {
			return "", err
		}
	}
}

// findInstanceElasticIPs lists the spawn-allocated Elastic IPs tagged for the
// instance.
func findInstanceElasticIPs(ctx context.Context, api elasticIPReleaseAPI, instanceID string) ([]types.Address, error) {
	out, err := api.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{
		Filters: []types.Filter{
			{Name: aws.String("tag:spawn:managed"), Values: []string{"true"}},
			{Name: aws.String("tag:" + ElasticIPInstanceTagKey), Values: []string{instanceID}},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("describe Elastic IPs for %s: %w", instanceID, err)
	}
	return out.Addresses, nil
}

// launchElasticIPOwnerTags are the tags a launch Elastic IP carries beyond
// LifecycleTags: the instance it belongs to, the same instance as an ARN when
// known (the instance role's ReleaseAddress grant compares that tag with
// ${ec2:SourceInstanceARN}, so without it spored cannot release the address
// and only the reaper can), and the launching principal when known (so `spawn
// cleanup`'s --mine scope covers it).
func launchElasticIPOwnerTags(instanceID, instanceARN string, instanceTags []types.Tag) []types.Tag {
	tags := []types.Tag{{Key: aws.String(ElasticIPInstanceTagKey), Value: aws.String(instanceID)}}
	if instanceARN != "" {
		tags = append(tags, types.Tag{Key: aws.String(elasticip.InstanceARNTagKey), Value: aws.String(instanceARN)})
	}
	for _, t := range instanceTags {
		if aws.ToString(t.Key) == "spawn:iam-user" {
			tags = append(tags, types.Tag{Key: t.Key, Value: t.Value})
		}
	}
	return tags
}

// attachLaunchElasticIP gives a running multi-interface instance its public IP:
// allocate an Elastic IP tagged for the instance, associate it to the primary
// interface, and return the address and allocation ID.
//
// Idempotent across a retried launch (a RunInstances ClientToken returns the same
// instance): an address already tagged for the instance is reused rather than a
// second allocated. On failure everything this call allocated is released again,
// so the caller has only the instance to clean up.
func attachLaunchElasticIP(ctx context.Context, api launchElasticIPAPI, name, instanceID, instanceARN string, instanceTags []types.Tag) (publicIP, allocationID string, err error) {
	eni, err := waitForPrimaryInterface(ctx, api, instanceID, eipRunningTimeout)
	if err != nil {
		return "", "", err
	}

	existing, err := findInstanceElasticIPs(ctx, api, instanceID)
	if err != nil {
		return "", "", err
	}
	allocatedHere := false
	if len(existing) > 0 {
		a := existing[0]
		publicIP, allocationID = aws.ToString(a.PublicIp), aws.ToString(a.AllocationId)
		if aws.ToString(a.NetworkInterfaceId) == eni && aws.ToString(a.AssociationId) != "" {
			return publicIP, allocationID, nil
		}
	} else {
		out, aerr := api.AllocateAddress(ctx, &ec2.AllocateAddressInput{
			Domain: types.DomainTypeVpc,
			TagSpecifications: []types.TagSpecification{{
				ResourceType: types.ResourceTypeElasticIp,
				Tags: append(LifecycleTags(name, ElasticIPPurpose, time.Now()),
					launchElasticIPOwnerTags(instanceID, instanceARN, instanceTags)...),
			}},
		})
		if aerr != nil {
			return "", "", fmt.Errorf("allocate Elastic IP for %s: %w", instanceID, aerr)
		}
		publicIP, allocationID = aws.ToString(out.PublicIp), aws.ToString(out.AllocationId)
		allocatedHere = true
	}

	var assocErr error
	for attempt := 0; ; attempt++ {
		_, assocErr = api.AssociateAddress(ctx, &ec2.AssociateAddressInput{
			AllocationId:       aws.String(allocationID),
			NetworkInterfaceId: aws.String(eni),
		})
		if assocErr == nil {
			return publicIP, allocationID, nil
		}
		if attempt >= len(eipRetryBackoff) ||
			!awsErrorCodeIn(assocErr, "InvalidNetworkInterfaceID.NotFound", "IncorrectInstanceState", "InvalidAllocationID.NotFound") {
			break
		}
		if serr := eipSleep(ctx, eipRetryBackoff[attempt]); serr != nil {
			assocErr = serr
			break
		}
	}
	err = fmt.Errorf("associate Elastic IP %s (%s) with %s's primary interface %s: %w", allocationID, publicIP, instanceID, eni, assocErr)
	if allocatedHere {
		if _, rerr := api.ReleaseAddress(ctx, &ec2.ReleaseAddressInput{AllocationId: aws.String(allocationID)}); rerr != nil {
			err = errors.Join(err, fmt.Errorf("releasing it also failed — release it by hand (aws ec2 release-address --allocation-id %s): %w", allocationID, rerr))
		}
	}
	return "", "", err
}

// releaseInstanceElasticIPs disassociates and releases every spawn-allocated
// Elastic IP tagged for the instance, returning the allocation IDs released.
//
// Called after TerminateInstances. EC2 disassociates the address itself once the
// primary interface is deleted, but that is later than this call, so it is
// disassociated explicitly; the instance is already shutting down, so nothing
// loses connectivity it would have kept. A release that races the
// disassociation (InvalidIPAddress.InUse) is retried.
func releaseInstanceElasticIPs(ctx context.Context, api elasticIPReleaseAPI, instanceID string) ([]string, error) {
	addrs, err := findInstanceElasticIPs(ctx, api, instanceID)
	if err != nil {
		return nil, err
	}
	var released []string
	var errs []error
	for _, a := range addrs {
		alloc := aws.ToString(a.AllocationId)
		if assoc := aws.ToString(a.AssociationId); assoc != "" {
			if _, derr := api.DisassociateAddress(ctx, &ec2.DisassociateAddressInput{AssociationId: aws.String(assoc)}); derr != nil &&
				!awsErrorCodeIn(derr, "InvalidAssociationID.NotFound") {
				errs = append(errs, fmt.Errorf("disassociate Elastic IP %s: %w", alloc, derr))
				continue
			}
		}
		var rerr error
		for attempt := 0; ; attempt++ {
			_, rerr = api.ReleaseAddress(ctx, &ec2.ReleaseAddressInput{AllocationId: aws.String(alloc)})
			if rerr == nil || awsErrorCodeIn(rerr, "InvalidAllocationID.NotFound") {
				rerr = nil
				break
			}
			if attempt >= len(eipRetryBackoff) || !awsErrorCodeIn(rerr, "InvalidIPAddress.InUse", "AuthFailure") {
				break
			}
			if serr := eipSleep(ctx, eipRetryBackoff[attempt]); serr != nil {
				rerr = serr
				break
			}
		}
		if rerr != nil {
			errs = append(errs, fmt.Errorf("release Elastic IP %s (%s): %w", alloc, aws.ToString(a.PublicIp), rerr))
			continue
		}
		released = append(released, alloc)
	}
	return released, errors.Join(errs...)
}
