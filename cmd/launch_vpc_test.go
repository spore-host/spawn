package cmd

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestBuildLaunchConfig_VPCID is spawn#673.
//
// --vpc was bound to a package global that NOTHING read — there was no VPCID
// field on LaunchConfig at all, and every consumer called GetDefaultVPC
// unconditionally. So `spawn launch --vpc vpc-0abc` was accepted, exited 0, and
// launched the instance in the DEFAULT VPC, with its security group created
// there too.
//
// The failure is silent and lands on network placement: wrong subnet, wrong
// route table, no route to an EFS or FSx mount target, and SG rules written into
// a VPC the instance is not in. An account whose research VPC is not the default
// could not target a VPC at all.
func TestBuildLaunchConfig_VPCID(t *testing.T) {
	prevVPC, prevType := vpcID, instanceType
	t.Cleanup(func() { vpcID, instanceType = prevVPC, prevType })
	instanceType = "c7g.4xlarge"

	t.Run("flag reaches the config", func(t *testing.T) {
		vpcID = "vpc-0abc1234def567890"
		cfg, err := buildLaunchConfig(nil)
		if err != nil {
			t.Fatalf("buildLaunchConfig: %v", err)
		}
		if cfg.VPCID != vpcID {
			t.Errorf("VPCID = %q, want %q. A dropped --vpc means the instance silently "+
				"launches in the default VPC (#673).", cfg.VPCID, vpcID)
		}
	})

	t.Run("unset leaves it empty so the default VPC is used", func(t *testing.T) {
		vpcID = ""
		cfg, err := buildLaunchConfig(nil)
		if err != nil {
			t.Fatalf("buildLaunchConfig: %v", err)
		}
		if cfg.VPCID != "" {
			t.Errorf("VPCID = %q, want empty: an unset flag must leave the default-VPC "+
				"fallback reachable", cfg.VPCID)
		}
	})
}

// TestNoLaunchPathCallsGetDefaultVPCDirectly is the class gate.
//
// The bug was not one missing field — it was five call sites each independently
// assuming the default VPC. Fixing one and leaving four would reproduce the
// #539/#667 pattern, where a condition was duplicated and then drifted. Every
// launch-path consumer must go through ResolveVPC, which is the single place
// that decides explicit-or-default.
func TestNoLaunchPathCallsGetDefaultVPCDirectly(t *testing.T) {
	// Files on the launch path. cmd/app.go, cmd/image.go and pkg/doctor are
	// deliberately excluded: they are not launches and have no --vpc to honour.
	launchPath := []string{
		"launch_single.go", "launch_config.go", "launch_sweep.go",
		"launch_jobarray.go", "launch_batchqueue.go",
	}
	call := regexp.MustCompile(`GetDefaultVPC\(`)

	for _, f := range launchPath {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for i, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if call.MatchString(line) {
				t.Errorf("%s:%d calls GetDefaultVPC directly:\n    %s\n\n"+
					"Use ResolveVPC so an explicit --vpc is honoured. Five sites each "+
					"assumed the default VPC, which is why --vpc did nothing (#673); "+
					"fixing some and not others is how #539 and #667 came to be fixed on "+
					"one path and left broken on another.",
					f, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// TestVPCIsThreadedIntoStorageResolution: the SG sites are the obvious ones, but
// FSx picks a subnet too, and a filesystem in the wrong VPC is unroutable from
// the instance — a failure that looks like a broken mount rather than a dropped
// flag.
func TestVPCIsThreadedIntoStorageResolution(t *testing.T) {
	b, err := os.ReadFile("launch_single.go")
	if err != nil {
		t.Fatalf("read launch_single.go: %v", err)
	}
	src := string(b)

	if !strings.Contains(src, "fsxConfig.VPCID = config.VPCID") {
		t.Error("--vpc is not threaded into FSxConfig; with --vpc set but no --subnet-id, " +
			"FSx would pick a subnet from the DEFAULT VPC and the mount could not route")
	}
	if !strings.Contains(src, "config.AvailabilityZone, config.VPCID)") {
		t.Error("GetSubnetForAZ is called without the VPC; with --vpc and --az it would " +
			"resolve the AZ's subnet in the default VPC instead")
	}
	if !strings.Contains(src, "ValidateSubnetInVPC(ctx, config.Region, config.SubnetID, config.VPCID)") {
		t.Error("nothing checks that --subnet-id and --vpc agree; EC2's own error for the " +
			"mismatch arrives only at RunInstances and names neither flag")
	}
}
