package cmd

import (
	"context"
	"fmt"
	"io"

	"github.com/spore-host/spawn/pkg/aws"
)

// memoryLookup is the slice of the AWS client this needs — an interface so the
// warning is tested against a fake instead of a live DescribeInstanceTypes.
type memoryLookup interface {
	InstanceMemoryMiB(ctx context.Context, region, instanceType string) (int64, bool)
}

// renderSporedMemoryFloorWarning returns the warning printed when an instance
// type has too little memory to be trusted to enforce its own lifetime, or ""
// when there is nothing to say.
//
// Pure (numbers in, string out) so it is tested without AWS.
//
// spawn#682: spored enforces TTL, idle and cost from INSIDE the instance, so a
// box too starved to make progress is also too starved to run the loop that
// would kill it. A 0.5 GiB instance wedged during a dnf transaction and was
// still running at twice its 6-minute TTL, with no completion record and no
// signal of any kind, until someone noticed.
//
// The warning names the consequence rather than the number, because "0.5 GiB is
// small" is obvious and "nothing will stop this instance" is not. It is a
// warning and not a refusal for the reason given on SporedMemoryFloorMiB: the
// fatal ingredient was a heavy bootstrap, which lived inside the user's
// --command where spawn cannot see it.
func renderSporedMemoryFloorWarning(instanceType string, memMiB int64, ttl string, haveTTL bool) string {
	if memMiB <= 0 || memMiB >= aws.SporedMemoryFloorMiB {
		return ""
	}

	gib := float64(memMiB) / 1024
	msg := fmt.Sprintf("\n⚠️  %s has %.1f GiB of memory, below the %.0f GiB spored needs to be "+
		"relied on.\n", instanceType, gib, float64(aws.SporedMemoryFloorMiB)/1024)
	msg += "   spored enforces TTL, idle and cost limits from inside the instance, so a box too\n" +
		"   starved to make progress is also too starved to stop itself. One wedged during a\n" +
		"   package install and ran to TWICE its TTL before a human killed it (#682).\n"

	if haveTTL && ttl != "" {
		msg += fmt.Sprintf("   Your --ttl %s may therefore not be enforced on this instance.\n", ttl)
	}
	msg += "   The out-of-band reaper is the only backstop: check this account has one with\n" +
		"   'spawn doctor', or use a larger instance type.\n"
	return msg
}

// warnIfBelowSporedMemoryFloor looks up the type's memory and prints the
// warning. Best-effort: a failed lookup says nothing rather than guessing, since
// a warning on every launch whose memory could not be resolved would be noise
// and would train the reader to skip it.
func warnIfBelowSporedMemoryFloor(ctx context.Context, client memoryLookup, w io.Writer, region, instanceType, ttl string) {
	if instanceType == "" {
		return
	}
	mem, ok := client.InstanceMemoryMiB(ctx, region, instanceType)
	if !ok {
		return
	}
	if msg := renderSporedMemoryFloorWarning(instanceType, mem, ttl, ttl != ""); msg != "" {
		fmt.Fprint(w, msg)
	}
}
