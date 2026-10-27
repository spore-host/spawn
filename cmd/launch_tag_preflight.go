package cmd

import (
	"fmt"
	"strings"

	"github.com/spore-host/spawn/pkg/aws"
)

// AWS's 50-tag limit, checked before a launch creates anything (#1336).
//
// buildTags refuses a tag set over the limit, but it runs inside
// aws.Client.Launch, after `spawn launch` has imported a key pair, created an
// IAM role and security groups, uploaded a Strata lockfile and possibly created
// a durable FSx. These pre-flights count the same tags first, from the flags
// alone, with a stand-in for every value the launch only learns from those
// steps.

// tagPreflightPlaceholder stands in for a tag value the launch resolves later.
// Only the number of tags matters, never their values.
const tagPreflightPlaceholder = "preflight"

// preflightLaunchTags refuses, before any AWS call that creates something, a
// `spawn launch` whose tags will not fit. config is the launch as runLaunch
// holds it before the Strata lockfile upload and launchWithProgress; it is not
// modified.
func preflightLaunchTags(config *aws.LaunchConfig) error {
	c := *config
	// launchWithProgress resolves the target OS from the AMI, and on Linux
	// tags the local user (spawn:os, spawn:local-username). An auto-detected
	// Windows AMI has no local-username tag, so this counts one too many for
	// it: a launch exactly at the limit is refused rather than one over it
	// launched.
	if c.TargetOS == "" {
		c.TargetOS = "linux"
		if strings.EqualFold(strings.TrimSpace(osFlag), "windows") {
			c.TargetOS = "windows"
		}
	}
	if c.Username == "" && c.TargetOS != "windows" {
		c.Username = tagPreflightPlaceholder
	}

	// The Strata lockfile's URI, known once it is uploaded.
	if strataFormation != "" || strataProfile != "" {
		c.Tags = withTag(c.Tags, "strata:lockfile-s3-uri")
	}

	// A durable --fsx-create, --fsx-id and --fsx-recall tag the filesystem's
	// ID and mount name, known once it exists or has been looked up. An
	// ephemeral --fsx-create's tags are counted by ValidateTagCount itself
	// (they are written after RunInstances, or before it with the same count).
	durableCreate := c.FSxLustreCreate && c.FSxLifecycle != "ephemeral"
	if durableCreate || c.FSxLustreID != "" || c.FSxLustreRecall != "" {
		c.FSxLustreCreate = false
		if c.FSxLustreID == "" {
			c.FSxLustreID = tagPreflightPlaceholder
		}
		if c.FSxMountName == "" {
			c.FSxMountName = tagPreflightPlaceholder
		}
	}

	// --count > 1 launches a job array, each member with its spawn:job-array-*
	// tags (buildJobArrayMemberConfig).
	if count > 1 && c.JobArrayID == "" {
		c.JobArrayID, c.JobArrayName, c.JobArraySize = tagPreflightPlaceholder, jobArrayName, count
	}
	return c.ValidateTagCount()
}

// preflightSweepTags refuses, before anything is created for the sweep, a
// parameter sweep any of whose members' tags will not fit. It renders each
// member's config as both orchestration paths do (buildLaunchConfigFromParams,
// defaultSweepMember); a member's target OS, resolved later from its AMI, is
// counted as set. Sweep parameters are not counted: they are dropped to fit.
func preflightSweepTags(base aws.LaunchConfig, pf *ParamFileFormat, sweepID, sweepName string) error {
	for i, row := range pf.Params {
		cfg, err := buildLaunchConfigFromParams(base, pf.Defaults, row, sweepID, sweepName, i, len(pf.Params))
		if err != nil {
			return fmt.Errorf("param set %d: %w", i, err)
		}
		defaultSweepMember(&cfg, base, sweepName, i)
		if cfg.TargetOS == "" {
			cfg.TargetOS = "linux"
		}
		if err := cfg.ValidateTagCount(); err != nil {
			return fmt.Errorf("param set %d: %w", i, err)
		}
	}
	return nil
}

// withTag returns a copy of tags with key set to the placeholder, unless it is
// already set.
func withTag(tags map[string]string, key string) map[string]string {
	out := make(map[string]string, len(tags)+1)
	for k, v := range tags {
		out[k] = v
	}
	if _, ok := out[key]; !ok {
		out[key] = tagPreflightPlaceholder
	}
	return out
}
