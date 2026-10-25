// Package elasticip holds what every releaser of a launch Elastic IP shares:
// the tags that mark an address as one spawn allocated for a multi-interface EFA
// launch (#661), and the one-call release spored and the TTL reaper use once the
// instance is ending.
//
// Three parties release these addresses, from three vantage points:
//
//   - `spawn terminate`, from the workstation, disassociates and releases
//     (pkg/aws). It keeps its connectivity throughout, so it can do both.
//   - spored, on the instance, after its own TerminateInstances and on a Spot
//     interruption notice. It reaches EC2 through the very address it is
//     releasing, so it cannot disassociate first: once the address is gone the
//     instance has no egress left to send the release over. It makes the one
//     call that can work, ReleaseAddress, which in a default VPC disassociates
//     and releases together. In a nondefault VPC EC2 answers InUse and the
//     address is left for the reaper.
//   - the TTL reaper, after it terminates an instance, and in its orphan sweep.
//
// Kept free of pkg/aws so spored and the reaper (a nested module) can import it
// without the CLI's dependency tree.
package elasticip

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

const (
	// ManagedTagKey is spawn's ownership tag, true on everything it creates.
	ManagedTagKey = "spawn:managed"
	// InstanceIDTagKey names the instance an address was allocated for. With
	// ManagedTagKey it marks the address as spawn's to release; an Elastic IP
	// without it is the user's and is never released (#262).
	InstanceIDTagKey = "spawn:instance-id"
	// InstanceARNTagKey carries the same instance as an ARN. It exists for IAM:
	// the instance role may release an address only when this tag equals the
	// calling instance's ${ec2:SourceInstanceARN}, which is an ARN, so the
	// instance ID alone cannot be compared with it.
	InstanceARNTagKey = "spawn:instance-arn"
	// PurposeTagKey / Purpose: what the address is for.
	PurposeTagKey = "spawn:purpose"
	Purpose       = "efa-public-ip"
)

// InstanceARN is the ARN EC2 reports as ec2:SourceInstanceARN for requests
// made with the instance's role credentials.
func InstanceARN(region, accountID, instanceID string) string {
	return fmt.Sprintf("arn:%s:ec2:%s:%s:instance/%s", partition(region), region, accountID, instanceID)
}

func partition(region string) string {
	switch {
	case strings.HasPrefix(region, "cn-"):
		return "aws-cn"
	case strings.HasPrefix(region, "us-gov-"):
		return "aws-us-gov"
	case strings.HasPrefix(region, "us-iso-"):
		return "aws-iso"
	case strings.HasPrefix(region, "us-isob-"):
		return "aws-iso-b"
	default:
		return "aws"
	}
}

// IsLaunchAddress reports whether tags mark an address as one spawn allocated
// for a launch: spawn:managed=true, the purpose, and an instance.
func IsLaunchAddress(tags []types.Tag) bool {
	return tagValue(tags, ManagedTagKey) == "true" &&
		tagValue(tags, PurposeTagKey) == Purpose &&
		tagValue(tags, InstanceIDTagKey) != ""
}

func tagValue(tags []types.Tag, key string) string {
	for _, t := range tags {
		if aws.ToString(t.Key) == key {
			return aws.ToString(t.Value)
		}
	}
	return ""
}

// API is the slice of EC2 ReleaseForInstance needs. *ec2.Client satisfies it.
type API interface {
	DescribeAddresses(ctx context.Context, in *ec2.DescribeAddressesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeAddressesOutput, error)
	ReleaseAddress(ctx context.Context, in *ec2.ReleaseAddressInput, optFns ...func(*ec2.Options)) (*ec2.ReleaseAddressOutput, error)
}

// Result is what ReleaseForInstance did, per allocation ID.
type Result struct {
	// Released: EC2 accepted the release (or the address was already gone).
	Released []string
	// InUse: EC2 refused because the address is still associated, which in a
	// nondefault VPC it is until the instance's primary interface is deleted.
	// Not an error: the reaper's sweep releases it once it is unassociated.
	InUse []string
}

// AddressesForInstance returns the launch Elastic IPs tagged for instanceID:
// the addresses ReleaseForInstance would release. A caller that may only report
// (the TTL reaper in report mode, #1287) uses it so that what it reports is
// exactly what a release would touch.
func AddressesForInstance(ctx context.Context, api API, instanceID string) ([]types.Address, error) {
	if instanceID == "" {
		return nil, nil
	}
	out, err := api.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{
		Filters: []types.Filter{
			{Name: aws.String("tag:" + ManagedTagKey), Values: []string{"true"}},
			{Name: aws.String("tag:" + InstanceIDTagKey), Values: []string{instanceID}},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("describe Elastic IPs for %s: %w", instanceID, err)
	}
	var addrs []types.Address
	for _, a := range out.Addresses {
		// The filter is the API's promise; this is ours. Never return an
		// address that is not a launch address for exactly this instance.
		if aws.ToString(a.AllocationId) == "" || !IsLaunchAddress(a.Tags) || tagValue(a.Tags, InstanceIDTagKey) != instanceID {
			continue
		}
		addrs = append(addrs, a)
	}
	return addrs, nil
}

// ReleaseForInstance releases every launch Elastic IP tagged for instanceID with
// a single ReleaseAddress each, never DisassociateAddress (see the package doc
// for why). Errors other than InUse and NotFound are joined and returned; one
// address's failure does not stop the rest.
func ReleaseForInstance(ctx context.Context, api API, instanceID string) (Result, error) {
	var res Result
	addrs, err := AddressesForInstance(ctx, api, instanceID)
	if err != nil {
		return res, err
	}
	var errs []error
	for _, a := range addrs {
		alloc := aws.ToString(a.AllocationId)
		_, rerr := api.ReleaseAddress(ctx, &ec2.ReleaseAddressInput{AllocationId: aws.String(alloc)})
		switch {
		case rerr == nil || strings.Contains(rerr.Error(), "InvalidAllocationID.NotFound"):
			res.Released = append(res.Released, alloc)
		case strings.Contains(rerr.Error(), "InvalidIPAddress.InUse"):
			res.InUse = append(res.InUse, alloc)
		default:
			errs = append(errs, fmt.Errorf("release Elastic IP %s (%s): %w", alloc, aws.ToString(a.PublicIp), rerr))
		}
	}
	return res, errors.Join(errs...)
}
