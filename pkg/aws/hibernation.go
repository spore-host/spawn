// Hibernation configuration for RunInstances (#949).

package aws

import (
	"context"
	"strings"
)

// HibernationRequested reports whether anything in this launch will ask EC2 to
// hibernate the instance: --hibernate itself, the idle action (--on-idle
// hibernate, or the deprecated --hibernate-on-idle), or --on-complete hibernate.
//
// EC2 hibernates only an instance launched with HibernationOptions.Configured
// (and an encrypted root volume big enough for RAM). Before #949 only
// --hibernate configured it, so --on-idle hibernate and --on-complete hibernate
// launched an instance EC2 cannot hibernate: spored's StopInstances(Hibernate)
// was rejected and EC2Provider.Hibernate fell back to a plain stop, losing RAM —
// and a persistent Spot request was interrupted by stop, not hibernate. Launch
// now configures hibernation whenever this is true, so every one of those paths
// gets what it asked for, or the launch is refused up front.
func (lc LaunchConfig) HibernationRequested() bool {
	return lc.Hibernate || lc.HibernateOnIdle || strings.EqualFold(strings.TrimSpace(lc.OnComplete), "hibernate")
}

// hibernationOSHeadroomGiB is the root-volume space reserved for the OS and
// applications on top of RAM, matching the old estimate's "+10".
const hibernationOSHeadroomGiB = 10

// hibernationRootGiB returns the root volume size for a hibernation-configured
// launch with no explicit --volume-size: RAM (rounded up to whole GiB) plus OS
// headroom. EC2 writes RAM to the root volume when it hibernates, and a volume
// too small for it makes the hibernation fail. ok is false when the memory is
// unknown, leaving the caller on the per-family estimate.
func hibernationRootGiB(memMiB int64) (int32, bool) {
	if memMiB <= 0 {
		return 0, false
	}
	return int32((memMiB+1023)/1024) + hibernationOSHeadroomGiB, true
}

// prepareHibernation normalizes a launch config before RunInstances: it sets
// Hibernate when any lifecycle path will hibernate (#949) and, with no explicit
// root size, sizes the root volume from the type's real memory. The per-family
// table in estimateVolumeSize ignores the size (every m7i is "16 GiB"), so an
// m7i.4xlarge (64 GiB) got a 26 GiB root and could not hibernate.
func (c *Client) prepareHibernation(ctx context.Context, lc *LaunchConfig) {
	prepareHibernationWith(lc, func() (int64, bool) {
		return c.InstanceMemoryMiB(ctx, lc.Region, lc.InstanceType)
	})
}

// prepareHibernationWith is prepareHibernation with the type's memory lookup
// supplied, so a detached member's settings (#1353) and a test resolve the
// same way without AWS.
func prepareHibernationWith(lc *LaunchConfig, memoryMiB func() (int64, bool)) {
	if !lc.HibernationRequested() {
		return
	}
	lc.Hibernate = true
	if lc.RootVolumeSizeGiB > 0 {
		return // the user's explicit size wins (buildBlockDevices still floors it at the AMI's)
	}
	if mib, ok := memoryMiB(); ok {
		if gib, ok := hibernationRootGiB(mib); ok {
			lc.RootVolumeSizeGiB = gib
		}
	}
}
