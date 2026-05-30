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
