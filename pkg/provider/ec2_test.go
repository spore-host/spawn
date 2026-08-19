package provider

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spore-host/spawn/pkg/testutil"
)

// newTestEC2Provider builds an EC2Provider wired to a substrate EC2 client and
// launches one instance for it to act on. imdsClient is left nil — IMDS-backed
// methods are tested separately for their error paths.
func newTestEC2Provider(t *testing.T) (*EC2Provider, string) {
	t.Helper()
	env := testutil.SubstrateServer(t)
	ec2Client := ec2.NewFromConfig(env.AWSConfig)

	out, err := ec2Client.RunInstances(context.Background(), &ec2.RunInstancesInput{
		InstanceType: ec2types.InstanceTypeT3Micro,
		ImageId:      aws.String("ami-12345678"),
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
	})
	if err != nil {
		t.Fatalf("RunInstances: %v", err)
	}
	id := *out.Instances[0].InstanceId

	p := &EC2Provider{
		ec2Client: ec2Client,
		identity:  &Identity{InstanceID: id, Region: "us-east-1", Provider: "ec2"},
		config:    &Config{IdleCPUPercent: 5.0},
	}
	return p, id
}

func TestEC2Provider_GetIdentityAndConfig(t *testing.T) {
	p, id := newTestEC2Provider(t)
	ctx := context.Background()

	identity, err := p.GetIdentity(ctx)
	if err != nil {
		t.Fatalf("GetIdentity: %v", err)
	}
	if identity.InstanceID != id || identity.Provider != "ec2" {
		t.Errorf("unexpected identity: %+v", identity)
	}

	cfg, err := p.GetConfig(ctx)
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if cfg.IdleCPUPercent != 5.0 {
		t.Errorf("config IdleCPUPercent = %v, want 5.0", cfg.IdleCPUPercent)
	}
}

func TestEC2Provider_Terminate(t *testing.T) {
	p, _ := newTestEC2Provider(t)
	if err := p.Terminate(context.Background(), "ttl expired"); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
}

func TestEC2Provider_Stop(t *testing.T) {
	p, _ := newTestEC2Provider(t)
	if err := p.Stop(context.Background(), "idle"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestEC2Provider_Hibernate(t *testing.T) {
	p, _ := newTestEC2Provider(t)
	// Hibernate falls back to Stop if hibernation isn't supported; either way
	// the call should not return an error against substrate.
	if err := p.Hibernate(context.Background()); err != nil {
		t.Fatalf("Hibernate: %v", err)
	}
}

func TestEC2Provider_DiscoverPeers_EmptyArrayID(t *testing.T) {
	p, _ := newTestEC2Provider(t)
	// Empty job array ID short-circuits to (nil, nil).
	peers, err := p.DiscoverPeers(context.Background(), "")
	if err != nil {
		t.Fatalf("DiscoverPeers(empty): %v", err)
	}
	if peers != nil {
		t.Errorf("expected nil peers for empty array ID, got %v", peers)
	}
}

func TestEC2Provider_DiscoverPeers_QueryPath(t *testing.T) {
	p, _ := newTestEC2Provider(t)
	// Exercises the DescribeInstances query + peer-parsing path. Substrate's
	// tag-filter fidelity varies, so we only require a clean (no-error) run.
	if _, err := p.DiscoverPeers(context.Background(), "ja-test"); err != nil {
		t.Fatalf("DiscoverPeers: %v", err)
	}
}

// TestEC2Provider_LookupAndTagEBSCost_CachedTagIsMeasured confirms a
// pre-populated config.EBSHourlyCost (loaded from the spawn:ebs-hourly-cost
// tag on a prior run) is reported as measured — the cache is trusted data,
// not a fallback.
func TestEC2Provider_LookupAndTagEBSCost_CachedTagIsMeasured(t *testing.T) {
	p, _ := newTestEC2Provider(t)
	p.config.EBSHourlyCost = 0.0021

	cost, measured := p.LookupAndTagEBSCost(context.Background())
	if !measured {
		t.Error("a cached (previously measured) EBSHourlyCost must report measured=true")
	}
	if cost != 0.0021 {
		t.Errorf("cost = %v, want 0.0021 (the cached value)", cost)
	}
}

// TestEC2Provider_LookupAndTagEBSCost_NoVolumesIsNotMeasured is the
// regression test for spawn#517's defect 2: a fallback value (returned when
// the instance has no EBS-backed block device mappings, or DescribeVolumes
// fails) must be distinguishable from a real measurement. Before the fix,
// LookupAndTagEBSCost returned a bare float64, so the caller in
// pkg/agent/agent.go could not tell 0.003-because-unmeasured apart from
// 0.003-because-that-really-is-the-rate, and logged both identically.
//
// The test instance from newTestEC2Provider has no block device mappings (the
// substrate emulator's DescribeInstances response doesn't populate them), so
// this exercises the "no volume IDs found" fallback branch — the same shape
// of fallback as a DescribeVolumes failure, just reached one branch earlier.
func TestEC2Provider_LookupAndTagEBSCost_NoVolumesIsNotMeasured(t *testing.T) {
	p, _ := newTestEC2Provider(t)

	cost, measured := p.LookupAndTagEBSCost(context.Background())
	if measured {
		t.Error("an instance with no discoverable EBS volumes must report measured=false, not silently claim a real rate")
	}
	if cost != 0.003 {
		t.Errorf("fallback cost = %v, want the documented safe-fallback constant 0.003", cost)
	}
}
