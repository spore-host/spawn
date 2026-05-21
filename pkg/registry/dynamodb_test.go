package registry

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/spore-host/spawn/pkg/provider"
	"github.com/spore-host/spawn/pkg/testutil"
)

// createRegistryTable creates the spawn-hybrid-registry table in the emulator.
func createRegistryTable(t *testing.T, client *dynamodb.Client) {
	t.Helper()
	ctx := context.Background()
	_, err := client.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: aws.String(TableName),
		KeySchema: []dynamodbtypes.KeySchemaElement{
			{AttributeName: aws.String("job_array_id"), KeyType: dynamodbtypes.KeyTypeHash},
			{AttributeName: aws.String("instance_id"), KeyType: dynamodbtypes.KeyTypeRange},
		},
		AttributeDefinitions: []dynamodbtypes.AttributeDefinition{
			{AttributeName: aws.String("job_array_id"), AttributeType: dynamodbtypes.ScalarAttributeTypeS},
			{AttributeName: aws.String("instance_id"), AttributeType: dynamodbtypes.ScalarAttributeTypeS},
		},
		BillingMode: dynamodbtypes.BillingModePayPerRequest,
	})
	if err != nil {
		t.Fatalf("createRegistryTable: %v", err)
	}
}

func TestPeerRegistry_RegisterAndDiscover(t *testing.T) {
	env := testutil.SubstrateServer(t)
	createRegistryTable(t, env.DynamoClient())

	ctx := context.Background()
	identity := &provider.Identity{
		InstanceID: "test-instance-01",
		Region:     "us-east-1",
		AccountID:  "123456789012",
		Provider:   "local",
		PublicIP:   "192.168.1.100",
		PrivateIP:  "192.168.1.100",
	}

	reg := NewPeerRegistryFromConfig(identity, env.AWSConfig)
	jobArrayID := fmt.Sprintf("test-array-%d", time.Now().UnixNano())

	if err := reg.Register(ctx, jobArrayID, 0); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	peers, err := reg.DiscoverPeers(ctx, jobArrayID)
	if err != nil {
		t.Fatalf("DiscoverPeers() error = %v", err)
	}

	if len(peers) != 1 {
		t.Errorf("got %d peers, want 1", len(peers))
	}

	if len(peers) > 0 {
		if peers[0].InstanceID != identity.InstanceID {
			t.Errorf("InstanceID = %v, want %v", peers[0].InstanceID, identity.InstanceID)
		}
		if peers[0].Provider != identity.Provider {
			t.Errorf("Provider = %v, want %v", peers[0].Provider, identity.Provider)
		}
		if peers[0].IP != identity.PublicIP {
			t.Errorf("IP = %v, want %v", peers[0].IP, identity.PublicIP)
		}
	}

	if err := reg.Deregister(ctx, jobArrayID); err != nil {
		t.Errorf("Deregister() error = %v", err)
	}
}

func TestPeerRegistry_Heartbeat(t *testing.T) {
	env := testutil.SubstrateServer(t)
	createRegistryTable(t, env.DynamoClient())

	ctx := context.Background()
	identity := &provider.Identity{
		InstanceID: "test-heartbeat-01",
		Region:     "us-east-1",
		Provider:   "local",
		PublicIP:   "192.168.1.101",
	}

	reg := NewPeerRegistryFromConfig(identity, env.AWSConfig)
	jobArrayID := fmt.Sprintf("test-heartbeat-%d", time.Now().UnixNano())

	if err := reg.Register(ctx, jobArrayID, 0); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	defer reg.Deregister(ctx, jobArrayID) //nolint:errcheck

	if err := reg.Heartbeat(ctx, jobArrayID); err != nil {
		t.Errorf("Heartbeat() error = %v", err)
	}

	peers, err := reg.DiscoverPeers(ctx, jobArrayID)
	if err != nil {
		t.Fatalf("DiscoverPeers() error = %v", err)
	}

	if len(peers) != 1 {
		t.Errorf("got %d peers after heartbeat, want 1", len(peers))
	}
}

func TestPeerRegistry_MultipleInstances(t *testing.T) {
	env := testutil.SubstrateServer(t)
	createRegistryTable(t, env.DynamoClient())

	ctx := context.Background()
	jobArrayID := fmt.Sprintf("test-multi-%d", time.Now().UnixNano())

	identity1 := &provider.Identity{InstanceID: "test-multi-01", Region: "us-east-1", Provider: "local", PublicIP: "192.168.1.103"}
	identity2 := &provider.Identity{InstanceID: "test-multi-02", Region: "us-east-1", Provider: "ec2", PublicIP: "54.1.2.3"}

	reg1 := NewPeerRegistryFromConfig(identity1, env.AWSConfig)
	reg2 := NewPeerRegistryFromConfig(identity2, env.AWSConfig)

	if err := reg1.Register(ctx, jobArrayID, 0); err != nil {
		t.Fatalf("Register1() error = %v", err)
	}
	defer reg1.Deregister(ctx, jobArrayID) //nolint:errcheck

	if err := reg2.Register(ctx, jobArrayID, 1); err != nil {
		t.Fatalf("Register2() error = %v", err)
	}
	defer reg2.Deregister(ctx, jobArrayID) //nolint:errcheck

	peers, err := reg1.DiscoverPeers(ctx, jobArrayID)
	if err != nil {
		t.Fatalf("DiscoverPeers() error = %v", err)
	}

	if len(peers) != 2 {
		t.Errorf("got %d peers, want 2", len(peers))
	}

	foundLocal, foundEC2 := false, false
	for _, p := range peers {
		if p.Provider == "local" {
			foundLocal = true
		}
		if p.Provider == "ec2" {
			foundEC2 = true
		}
	}
	if !foundLocal {
		t.Error("did not find local provider instance")
	}
	if !foundEC2 {
		t.Error("did not find ec2 provider instance")
	}
}

func TestDiscoverPeersForJobArray(t *testing.T) {
	env := testutil.SubstrateServer(t)
	createRegistryTable(t, env.DynamoClient())

	ctx := context.Background()
	jobArrayID := fmt.Sprintf("test-discover-%d", time.Now().UnixNano())

	identity := &provider.Identity{
		InstanceID: "test-discover-01",
		Region:     "us-east-1",
		Provider:   "local",
		PublicIP:   "192.168.1.104",
	}

	reg := NewPeerRegistryFromConfig(identity, env.AWSConfig)
	if err := reg.Register(ctx, jobArrayID, 0); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	defer reg.Deregister(ctx, jobArrayID) //nolint:errcheck

	peers, err := DiscoverPeersForJobArray(ctx, env.AWSConfig, jobArrayID)
	if err != nil {
		t.Fatalf("DiscoverPeersForJobArray() error = %v", err)
	}

	if len(peers) != 1 {
		t.Errorf("got %d peers, want 1", len(peers))
	}
}
