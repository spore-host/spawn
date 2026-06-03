//go:build e2e_tier1 || e2e_tier2 || e2e_tier3

package e2e

// Cost-control safety net for the real-AWS tiers (#47).
//
// e2e tests launch real instances. t.Cleanup is best-effort — it does not run
// if the test binary is killed (timeout, CI cancel, ^C, panic in TestMain), and
// a launch whose spored never boots has no TTL enforcement. That leaked ~67
// instances (one running 12 days) before this existed.
//
// TestMain reaps before AND after every run: it terminates any e2e instance
// (Name prefixed "e2e-") older than reapAgeThreshold across all test regions,
// regardless of which test or prior run created it. This makes leaks
// self-healing — the next run cleans up the previous run's escapes — and is the
// prerequisite for running Tier 2/3 safely.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// reapRegions are every region the e2e suite launches into.
var reapRegions = []string{"us-east-1", "us-east-2"}

// reapAgeThreshold is how old an e2e instance must be before TestMain reaps it.
// Long enough not to kill an in-flight launch from a parallel run, short enough
// to bound cost. Test TTLs are ≤20m, so anything older is a leak.
const reapAgeThreshold = 30 * time.Minute

// TestMain reaps stale e2e instances before and after the suite runs.
func TestMain(m *testing.M) {
	reap("pre-run")
	code := m.Run()
	reap("post-run")
	os.Exit(code)
}

// reap terminates e2e instances older than the threshold across all test
// regions. Best-effort and non-fatal: it logs what it does and never blocks the
// suite, but it guarantees leaks don't accumulate across runs.
func reap(phase string) {
	ctx := context.Background()
	profile := os.Getenv("AWS_PROFILE")
	if profile == "" && os.Getenv("AWS_ACCESS_KEY_ID") == "" {
		profile = "spore-host-dev"
	}

	for _, region := range reapRegions {
		opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(region)}
		if profile != "" && os.Getenv("AWS_ACCESS_KEY_ID") == "" {
			opts = append(opts, awsconfig.WithSharedConfigProfile(profile))
		}
		cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[reap %s] %s: load config: %v\n", phase, region, err)
			continue
		}
		client := ec2.NewFromConfig(cfg)

		out, err := client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
			Filters: []ec2types.Filter{
				{Name: strptr("tag:Name"), Values: []string{"e2e-*"}},
				{Name: strptr("instance-state-name"), Values: []string{"pending", "running", "stopping", "stopped"}},
			},
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "[reap %s] %s: describe: %v\n", phase, region, err)
			continue
		}

		var stale []string
		cutoff := time.Now().Add(-reapAgeThreshold)
		for _, r := range out.Reservations {
			for _, inst := range r.Instances {
				if inst.LaunchTime != nil && inst.LaunchTime.After(cutoff) {
					continue // too young — may be an in-flight launch
				}
				stale = append(stale, *inst.InstanceId)
			}
		}
		if len(stale) == 0 {
			continue
		}
		fmt.Fprintf(os.Stderr, "[reap %s] %s: terminating %d stale e2e instance(s): %s\n",
			phase, region, len(stale), strings.Join(stale, " "))
		if _, err := client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
			InstanceIds: stale,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "[reap %s] %s: terminate: %v\n", phase, region, err)
		}
	}
}

func strptr(s string) *string { return &s }
