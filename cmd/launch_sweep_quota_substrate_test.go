package cmd

import (
	"context"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/testutil"
)

// TestResolveAutoMaxConcurrent_DerivesFromRealQuota is the spawn#492
// end-to-end guard: --max-concurrent-auto must derive an instance-count
// ceiling from a REAL quota answer + a REAL per-instance-type vCPU count, not
// a guessed one. Substrate seeds the Standard On-Demand quota (L-1216C47A) at
// 32 vCPUs and models c5.xlarge at 4 vCPUs, so with zero running instances the
// expected ceiling is exactly 32/4 = 8.
func TestResolveAutoMaxConcurrent_DerivesFromRealQuota(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	client := aws.NewClientFromConfig(env.AWSConfig)

	paramFormat := &ParamFileFormat{
		Params: []map[string]interface{}{
			{"instance_type": "c5.xlarge"},
			{"instance_type": "c5.xlarge"},
		},
	}
	baseConfig := &aws.LaunchConfig{}

	got, err := resolveAutoMaxConcurrent(ctx, paramFormat, baseConfig, "us-east-1", client)
	if err != nil {
		t.Fatalf("resolveAutoMaxConcurrent: %v", err)
	}
	if got != 8 {
		t.Errorf("derived max-concurrent = %d, want 8 (32 vCPU quota / 4 vCPU per c5.xlarge)", got)
	}
}

// TestResolveAutoMaxConcurrent_TightestFamilyWins is the heterogeneous-sweep
// guard (#492's own ask): a sweep mixing a scarce family with a roomy one must
// derive its ceiling from the TIGHTEST-fitting family, not the loosest —
// otherwise a sweep that "looks fine" against one family could still overrun
// another. p3.2xlarge's family (P) has no seeded Substrate quota, so its
// headroom is 0 — which correctly makes the WHOLE sweep's derived ceiling 0
// (and thus an error, safety-first) even though c5.xlarge alone would derive
// 8. This is deliberate: silently dropping the P-family rows to "derive 8
// anyway" would let the sweep launch p3.2xlarge instances with no quota
// headroom verified at all, exactly the blind spot #492 exists to close.
func TestResolveAutoMaxConcurrent_TightestFamilyWins(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	client := aws.NewClientFromConfig(env.AWSConfig)

	paramFormat := &ParamFileFormat{
		Params: []map[string]interface{}{
			{"instance_type": "c5.xlarge"},  // Standard family: real 32-vCPU quota, headroom for 8
			{"instance_type": "p3.2xlarge"}, // P family: no seeded quota in Substrate -> headroom 0
		},
	}
	baseConfig := &aws.LaunchConfig{}

	_, err := resolveAutoMaxConcurrent(ctx, paramFormat, baseConfig, "us-east-1", client)
	if err == nil {
		t.Fatal("want an error: the P-family row's zero quota headroom must drag the WHOLE sweep's ceiling to 0, not be silently outvoted by c5.xlarge's roomier 8")
	}
}

// TestResolveAutoMaxConcurrent_RequiresResolvedRegion guards the ordering
// invariant this function's doc comment states: it must be called AFTER
// region resolution, never before.
func TestResolveAutoMaxConcurrent_RequiresResolvedRegion(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	client := aws.NewClientFromConfig(env.AWSConfig)

	paramFormat := &ParamFileFormat{
		Params: []map[string]interface{}{{"instance_type": "c5.xlarge"}},
	}
	if _, err := resolveAutoMaxConcurrent(ctx, paramFormat, &aws.LaunchConfig{}, "", client); err == nil {
		t.Error("want error when region is empty")
	}
}

// TestResolveAutoMaxConcurrentFromConfigs_DerivesFromRealQuota is the
// spawn#494 (`spawn resume --max-concurrent-auto`) end-to-end guard, mirroring
// TestResolveAutoMaxConcurrent_DerivesFromRealQuota but from already-built
// *aws.LaunchConfig entries (resume's pending configs) rather than raw
// ParamFileFormat params. Same Substrate seeding: 32 vCPU Standard On-Demand
// quota / 4 vCPU per c5.xlarge = 8.
func TestResolveAutoMaxConcurrentFromConfigs_DerivesFromRealQuota(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	client := aws.NewClientFromConfig(env.AWSConfig)

	launchConfigs := []*aws.LaunchConfig{
		{InstanceType: "c5.xlarge", Region: "us-east-1"},
		{InstanceType: "c5.xlarge", Region: "us-east-1"},
	}

	got, err := resolveAutoMaxConcurrentFromConfigs(ctx, launchConfigs, "us-east-1", client)
	if err != nil {
		t.Fatalf("resolveAutoMaxConcurrentFromConfigs: %v", err)
	}
	if got != 8 {
		t.Errorf("derived max-concurrent = %d, want 8 (32 vCPU quota / 4 vCPU per c5.xlarge)", got)
	}
}

// TestResolveAutoMaxConcurrentFromConfigs_RequiresResolvedRegion is the
// resume-path counterpart of TestResolveAutoMaxConcurrent_RequiresResolvedRegion.
func TestResolveAutoMaxConcurrentFromConfigs_RequiresResolvedRegion(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	client := aws.NewClientFromConfig(env.AWSConfig)

	launchConfigs := []*aws.LaunchConfig{{InstanceType: "c5.xlarge"}}
	if _, err := resolveAutoMaxConcurrentFromConfigs(ctx, launchConfigs, "", client); err == nil {
		t.Error("want error when region is empty")
	}
}
