package aws

import (
	"context"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// SporedMemoryFloorMiB is the memory below which spawn warns that the instance
// may be unable to enforce its own lifetime.
//
// spored enforces TTL, idle and cost limits from INSIDE the instance. A box too
// starved to make progress is therefore also too starved to run the loop that
// would kill it — the enforcement mechanism and the thing it must survive are
// the same resource. That is not a theory: a 0.5 GiB instance wedged during a
// dnf transaction and was still running at TWICE its 6-minute TTL, with no
// completion record and no signal of any kind, until a human noticed and killed
// it by hand (spawn#682).
//
// 1 GiB rather than something larger because the same reporter's 4 GiB runs
// behaved correctly end to end, and nothing between 0.5 and 4 GiB was tested —
// so a higher floor would be inventing evidence. 1 GiB is the point below which
// AL2023's own package transactions are unreliable, which is what starved it.
// This is a WARNING threshold, deliberately not a refusal: a nano instance
// running a trivial command works fine today, and the demonstrated failure
// needed a tiny box AND a heavy bootstrap together. spawn cannot see the second
// half — the Docker install in #682 was inside the user's --command, not a flag
// — so refusing on memory alone would block working launches to prevent a
// combination spawn cannot detect.
const SporedMemoryFloorMiB int64 = 1024

// tinyInstanceMemoryMiB is the offline fallback for the types at issue. Only the
// smallest sizes are listed: the warning exists for boxes near the floor, and
// anything absent is assumed to be above it rather than warned about on a guess.
var tinyInstanceMemoryMiB = map[string]int64{
	"nano":   512,
	"micro":  1024,
	"small":  2048,
	"medium": 4096,
}

// InstanceMemoryMiB returns an instance type's memory, authoritatively via
// DescribeInstanceTypes, falling back to the size-suffix table when the call
// fails (offline, throttled, or an unknown type). ok is false when neither
// source knows, so a caller can stay silent rather than warn on a guess.
//
// Mirrors resolveArchitecture (ami.go): API first so a new family needs no code
// change, static table as the offline floor.
func (c *Client) InstanceMemoryMiB(ctx context.Context, region, instanceType string) (mib int64, ok bool) {
	out, err := c.regionalEC2(region).DescribeInstanceTypes(ctx, &ec2.DescribeInstanceTypesInput{
		InstanceTypes: []ec2types.InstanceType{ec2types.InstanceType(instanceType)},
	})
	if err == nil && len(out.InstanceTypes) > 0 {
		if mi := out.InstanceTypes[0].MemoryInfo; mi != nil && mi.SizeInMiB != nil {
			return *mi.SizeInMiB, true
		}
	}
	return instanceMemoryFromSuffix(instanceType)
}

// instanceMemoryFromSuffix reads the size suffix (t4g.nano → 512 MiB). Pure, so
// the fallback is testable without AWS.
func instanceMemoryFromSuffix(instanceType string) (int64, bool) {
	dot := strings.LastIndex(instanceType, ".")
	if dot < 0 || dot == len(instanceType)-1 {
		return 0, false
	}
	mib, ok := tinyInstanceMemoryMiB[instanceType[dot+1:]]
	return mib, ok
}
