package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/spore-host/spawn/pkg/aws"
	truffleaws "github.com/spore-host/truffle/pkg/aws"
	truffleQuotas "github.com/spore-host/truffle/pkg/quotas"
)

// resolveAutoMaxConcurrent derives a --max-concurrent ceiling from the
// account's real AWS quota headroom, instead of requiring the caller to
// already know it (spawn#492). The sweep orchestrator's wave mechanism
// (pkg/sweep + lambda/sweep-orchestrator) has always existed — it polls
// active-instance count and launches min(available, remaining) — but its
// ceiling was purely user-typed, with nothing computing or suggesting a
// value from the account's actual limits.
//
// Real-world motivation: a 10-shard fleet with no concurrency guardrail hit
// an account's real ceiling (a G/VT Spot quota of 64 vCPUs, saturated by 8
// already-running g7e.2xlarge instances) with zero warning.
//
// region is the sweep's resolved launch region (both the detached and
// non-detached callers already resolve this before this point — see
// detectBestRegion). It:
//
//  1. Extracts the distinct (instance type, spot) combinations the sweep will
//     actually launch, from the raw param sets (via buildLaunchConfigFromParams,
//     the same per-entry-override merge the non-detached path already applies —
//     see the "instance_type is a per-entry override (#372)" comment there).
//  2. Queries truffle's quota client once for headroom (quota - current usage)
//     per family, converting vCPU headroom to an instance count via
//     Capabilities.VCPUs (truffle#492 — the real EC2 value, not a guessed
//     size-suffix).
//  3. Returns the MINIMUM headroom-derived instance count across every
//     combination present in the sweep — the ceiling must respect the
//     tightest-fitting quota, not the loosest, or a heterogeneous sweep could
//     still overrun a scarce family while looking fine against a roomy one.
//
// A family this can't get a quota/vCPU answer for (missing credentials,
// unsupported family, API error) is skipped with a warning rather than
// aborting the whole sweep — this is a SAFETY DEFAULT, not a hard gate, and a
// caller that explicitly asked for --max-concurrent=auto should still get a
// best-effort number rather than an outright failure when only some rows in a
// mixed sweep are affected.
//
// awsClient is an already-constructed client PINNED TO region (the caller has
// already done this re-pinning for AMI/AZ/identity resolution elsewhere —
// see the #276 comment on the non-detached path); this function builds
// truffle clients from its config rather than constructing its own, both to
// avoid a second client construction and so tests can inject a
// Substrate-backed client.
func resolveAutoMaxConcurrent(ctx context.Context, paramFormat *ParamFileFormat, baseConfig *aws.LaunchConfig, region string, awsClient *aws.Client) (int, error) {
	if region == "" {
		return 0, fmt.Errorf("auto max-concurrent: no region resolved yet — derive the ceiling AFTER region resolution")
	}

	combos, err := sweepQuotaCombos(paramFormat, baseConfig)
	if err != nil {
		return 0, err
	}

	quotaClient := truffleQuotas.NewClientFromConfig(awsClient.Config())
	capsClient := truffleaws.NewClientFromConfig(awsClient.Config())

	info, err := quotaClient.GetQuotas(ctx, region)
	if err != nil {
		return 0, fmt.Errorf("auto max-concurrent: quota lookup for %s: %w", region, err)
	}

	best := -1 // -1 = no combo has produced a usable answer yet
	var warnings []string

	for _, c := range combos {
		family := truffleQuotas.GetQuotaFamily(c.instanceType)
		var quota, usage int32
		if c.spot {
			quota, usage = info.Spot[family], info.SpotUsage[family]
		} else {
			quota, usage = info.OnDemand[family], info.Usage[family]
		}
		headroomVCPUs := quota - usage
		if headroomVCPUs < 0 {
			headroomVCPUs = 0
		}

		caps, err := capsClient.GetCapabilities(ctx, c.instanceType, region)
		if err != nil || !caps.Found || caps.VCPUs <= 0 {
			warnings = append(warnings, fmt.Sprintf("%s: vCPU count unavailable, cannot convert quota headroom to an instance count", c.instanceType))
			continue
		}

		instances := int(headroomVCPUs / caps.VCPUs)
		if best == -1 || instances < best {
			best = instances
		}
	}

	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "⚠️  --max-concurrent=auto: %s\n", w)
	}

	if best == -1 {
		return 0, fmt.Errorf("auto max-concurrent: could not derive a ceiling for any instance type in this sweep — specify --max-concurrent explicitly")
	}
	if best == 0 {
		return 0, fmt.Errorf("auto max-concurrent: account quota headroom is 0 for at least one instance type/family in this sweep — request a quota increase, free up running capacity, or specify --max-concurrent explicitly to override this safety check")
	}
	return best, nil
}

// sweepQuotaCombo is one distinct (instance type, spot-vs-on-demand)
// combination a sweep will actually launch.
type sweepQuotaCombo struct {
	instanceType string
	spot         bool
}

// sweepQuotaCombos extracts the distinct (instance type, spot) combinations
// from a sweep's raw param sets, applying the same defaults-then-per-entry
// merge (buildLaunchConfigFromParams) and base-config fallback the rest of
// launchParameterSweep uses — pure, no AWS calls, so it's unit-testable
// without Substrate.
func sweepQuotaCombos(paramFormat *ParamFileFormat, baseConfig *aws.LaunchConfig) ([]sweepQuotaCombo, error) {
	seen := make(map[sweepQuotaCombo]bool)
	var combos []sweepQuotaCombo
	for i, paramSet := range paramFormat.Params {
		cfg, err := buildLaunchConfigFromParams(paramFormat.Defaults, paramSet, "", "", i, len(paramFormat.Params))
		if err != nil {
			return nil, fmt.Errorf("auto max-concurrent: build launch config for parameter set %d: %w", i, err)
		}
		instanceType := cfg.InstanceType
		if instanceType == "" {
			instanceType = baseConfig.InstanceType
		}
		if instanceType == "" {
			return nil, fmt.Errorf("auto max-concurrent: parameter set %d has no instance_type", i)
		}
		c := sweepQuotaCombo{instanceType: instanceType, spot: cfg.Spot || baseConfig.Spot}
		if !seen[c] {
			seen[c] = true
			combos = append(combos, c)
		}
	}
	if len(combos) == 0 {
		return nil, fmt.Errorf("auto max-concurrent: no parameter sets to derive a ceiling from")
	}
	return combos, nil
}
