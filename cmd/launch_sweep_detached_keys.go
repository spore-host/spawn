package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/spore-host/spawn/pkg/aws"
)

// detachedParamKeys are the param keys the spawn-sweep-orchestrator Lambda reads
// when it composes RunInstances. Keep in sync with
// lambda/sweep-orchestrator/main.go's getStringParam/getBoolParam calls —
// TestDetachedSweepSeedsEveryKeyTheLambdaReads enforces that mechanically.
//
// The list exists because the two sides disagreed silently (#749). The Lambda
// never sees a LaunchConfig: it reads six keys out of the uploaded params.json
// and builds RunInstances from them, and `ami` was never seeded — so it sent no
// ImageId, EC2 rejected EVERY row with `MissingParameter: The request must
// contain the parameter ImageId`, the Lambda logged "All instances launched and
// completed", and the CLI printed "Parameter sweep queued successfully" and
// exited 0. A detached sweep — the DEFAULT for parameter sweeps — launched zero
// instances for its entire existence.
var detachedParamKeys = []string{
	"ami",
	"iam_role",
	"instance_type",
	"key_name",
	"spot",
	"ttl",
}

// NOTE: `region` is deliberately NOT in the list above, and the gate caught me
// putting it there. The orchestrator takes the region from the SWEEP RECORD
// (state.Region), not from the params — so it is not part of the param contract,
// even though the seeder below does set it, because the CLI-side cost estimator
// reads defaults["region"] directly (pkg/sweep/detached.go). The list is "what
// the Lambda reads", not "what matters"; conflating the two is how the list
// stops being trustworthy.

// seedDetachedLaunchKeys fills in the launch keys the orchestrator needs but
// cannot derive, resolving an AMI PER ROW.
//
// Per row rather than one sweep-wide default, because GetRecommendedAMI keys off
// the instance type (architecture and GPU) and a heterogeneous sweep (#372) would
// otherwise get one row's AMI forced onto every row — an arm64 AMI on an x86 row
// does not boot. The foreground path already resolves per config for exactly this
// reason; the codebase even documents "only the foreground path detects an AMI per
// config" as a reason to prefer --no-detach for heterogeneous sweeps. What nobody
// noticed is that it meant the detached path resolved NO ami at all.
//
// A row's own key always wins, and a file-level default the user wrote is never
// overwritten — the same precedence as every other sweep default
// (row's params: > CLI flag > file's defaults:).
func seedDetachedLaunchKeys(ctx context.Context, client *aws.Client, paramFormat *ParamFileFormat, baseConfig *aws.LaunchConfig, region string) error {
	if paramFormat.Defaults == nil {
		paramFormat.Defaults = map[string]interface{}{}
	}

	// Sweep-wide keys the Lambda reads. Each is only set when the CLI actually
	// has a value, so an unset flag leaves the Lambda's own default in place
	// (iam_role falls back to "spawnd-role" there).
	setDefault := func(key string, val interface{}) {
		if val == nil || val == "" || val == false {
			return
		}
		if _, set := paramFormat.Defaults[key]; !set {
			paramFormat.Defaults[key] = val
		}
	}
	setDefault("region", region)
	setDefault("instance_type", baseConfig.InstanceType)
	setDefault("key_name", baseConfig.KeyName)
	setDefault("iam_role", baseConfig.IamInstanceProfile)
	setDefault("spot", baseConfig.Spot)

	// AMI, per row. An explicit --ami applies to the whole sweep, as it does on
	// the single-instance path.
	if baseConfig.AMI != "" {
		setDefault("ami", baseConfig.AMI)
		return nil
	}

	// Memoized by (region, arch, gpu) like the foreground path, so a sweep of 50
	// rows over three families makes three SSM lookups rather than 50.
	cache := map[string]string{}
	resolve := func(instanceType, rowRegion string) (string, error) {
		key := fmt.Sprintf("%s|%s|%t", rowRegion,
			aws.DetectArchitecture(instanceType), aws.DetectGPUInstance(instanceType))
		if ami, ok := cache[key]; ok {
			return ami, nil
		}
		ami, err := client.GetRecommendedAMI(ctx, rowRegion, instanceType)
		if err != nil {
			return "", err
		}
		cache[key] = ami
		return ami, nil
	}

	defaultType := stringParam(paramFormat.Defaults, "instance_type")
	defaultRegion := stringParam(paramFormat.Defaults, "region")
	if defaultRegion == "" {
		defaultRegion = region
	}

	for i := range paramFormat.Params {
		row := paramFormat.Params[i]
		if row == nil {
			continue
		}
		if existing := stringParam(row, "ami"); existing != "" {
			continue // the row chose its own
		}
		rowType := stringParam(row, "instance_type")
		if rowType == "" {
			rowType = defaultType
		}
		if rowType == "" {
			// No instance type anywhere. The estimator rejects this earlier with a
			// clearer message; bail rather than guessing an architecture.
			return fmt.Errorf("param set %d: no instance_type, so no AMI can be resolved for it", i)
		}
		rowRegion := stringParam(row, "region")
		if rowRegion == "" {
			rowRegion = defaultRegion
		}
		ami, err := resolve(rowType, rowRegion)
		if err != nil {
			// FATAL, not a warning. Continuing would upload a row with no ami,
			// which is #749 exactly: EC2 rejects it and the Lambda reports success.
			return fmt.Errorf("param set %d: resolve AMI for %s in %s: %w", i, rowType, rowRegion, err)
		}
		row["ami"] = ami
	}

	// One line, not one per row: a 200-row sweep should not bury its own output.
	if len(cache) > 0 {
		fmt.Fprintf(os.Stderr, "✓ Resolved %d AMI(s) for %d row(s)\n", len(cache), len(paramFormat.Params))
	}
	return nil
}

// stringParam reads a string-valued key from a param map, tolerating absence and
// a non-string value.
func stringParam(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}
