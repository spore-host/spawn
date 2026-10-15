package aws

import (
	"testing"
)

// TestLaunch_HibernateLifecycleConfiguresHibernation is #949, through the real
// Client.Launch: EC2 hibernates only an instance launched with
// HibernationOptions.Configured=true and an encrypted root. --on-idle hibernate
// (HibernateOnIdle) and --on-complete hibernate used to send neither, so the
// idle or completion hibernate became a plain stop. pkg/aws is where this has to
// hold, because SDK callers (lagotto, spore-host-mcp, sweeps) build LaunchConfig
// themselves and never pass through the CLI.
func TestLaunch_HibernateLifecycleConfiguresHibernation(t *testing.T) {
	cases := []struct {
		name string
		lc   LaunchConfig
		want bool
	}{
		{"HibernateOnIdle", LaunchConfig{TTL: "1h", IdleTimeout: "15m", HibernateOnIdle: true}, true},
		{"OnComplete hibernate", LaunchConfig{TTL: "1h", OnComplete: "hibernate"}, true},
		{"OnComplete Hibernate (case)", LaunchConfig{TTL: "1h", OnComplete: " Hibernate "}, true},
		{"Hibernate", LaunchConfig{TTL: "1h", Hibernate: true}, true},
		{"OnComplete stop", LaunchConfig{TTL: "1h", OnComplete: "stop", IdleTimeout: "15m"}, false},
		{"plain", LaunchConfig{TTL: "1h"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := launchSpot(t, tc.lc)
			configured := run.Get("HibernationOptions.Configured") == "true"
			if configured != tc.want {
				t.Errorf("HibernationOptions.Configured sent = %v, want %v", configured, tc.want)
			}
			encrypted := run.Get("BlockDeviceMapping.1.Ebs.Encrypted") == "true"
			if tc.want && !encrypted {
				t.Error("a hibernation-configured launch must encrypt the root volume (EC2 requires it)")
			}
		})
	}
}

// The root volume must hold RAM: EC2 writes memory to it on hibernate. Sized
// from the type's real memory (DescribeInstanceTypes), not the per-family table
// that calls every m7i 16 GiB.
func TestLaunch_HibernationRootSizedFromRealMemory(t *testing.T) {
	run := launchWith(t, &fakeEC2{memMiB: "65536"}, LaunchConfig{TTL: "1h", OnComplete: "hibernate"})
	if got := run.Get("BlockDeviceMapping.1.Ebs.VolumeSize"); got != "74" { // 64 GiB RAM + 10
		t.Errorf("root VolumeSize = %q, want 74 (64 GiB RAM + 10 GiB OS)", got)
	}

	// An explicit --volume-size is the user's call and is kept.
	run = launchWith(t, &fakeEC2{memMiB: "65536"}, LaunchConfig{TTL: "1h", OnComplete: "hibernate", RootVolumeSizeGiB: 200})
	if got := run.Get("BlockDeviceMapping.1.Ebs.VolumeSize"); got != "200" {
		t.Errorf("explicit root VolumeSize = %q, want 200", got)
	}
}

func TestHibernationRootGiB(t *testing.T) {
	cases := []struct {
		mib  int64
		want int32
		ok   bool
	}{
		{0, 0, false},
		{-1, 0, false},
		{512, 11, true},     // rounds up to 1 GiB
		{1024, 11, true},    // exactly 1 GiB
		{1025, 12, true},    // just over
		{65536, 74, true},   // 64 GiB
		{393216, 394, true}, // 384 GiB
	}
	for _, tc := range cases {
		got, ok := hibernationRootGiB(tc.mib)
		if got != tc.want || ok != tc.ok {
			t.Errorf("hibernationRootGiB(%d) = %d,%v want %d,%v", tc.mib, got, ok, tc.want, tc.ok)
		}
	}
}
