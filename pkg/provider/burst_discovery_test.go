package provider

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spore-host/spawn/pkg/tagprefix"
	"github.com/spore-host/spawn/pkg/testutil"
)

// #918: the hybrid registry is written by local nodes and burst members alike,
// but EC2 members only ever read tags, so a burst member never saw the local
// nodes it was launched to work alongside. A burst member's discovery must
// include the registry's local nodes; an ordinary member's must not touch the
// registry at all (#904/#920: only hybrid members use it).

const testJobArray = "ja-918"

// burstMember launches one instance tagged as a member of testJobArray, with
// spawn:burst set to burst, and builds the EC2Provider spored would: its
// config read back from the instance's own tags.
func burstMember(t *testing.T, env *testutil.TestEnv, burst bool) (*EC2Provider, string) {
	t.Helper()
	ctx := context.Background()
	ec2Client := ec2.NewFromConfig(env.AWSConfig)

	tags := []ec2types.Tag{
		{Key: aws.String(tagprefix.Tag("job-array-id")), Value: aws.String(testJobArray)},
		{Key: aws.String(tagprefix.Tag("job-array-index")), Value: aws.String("0")},
		{Key: aws.String("Name"), Value: aws.String("spawn-burst-918-0")},
	}
	if burst {
		tags = append(tags, ec2types.Tag{Key: aws.String(tagprefix.Tag("burst")), Value: aws.String("true")})
	}
	out, err := ec2Client.RunInstances(ctx, &ec2.RunInstancesInput{
		InstanceType: ec2types.InstanceTypeT3Micro,
		ImageId:      aws.String(testutil.RegisterTestAMI(t, env.EC2Client())),
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
		TagSpecifications: []ec2types.TagSpecification{
			{ResourceType: ec2types.ResourceTypeInstance, Tags: tags},
		},
	})
	if err != nil {
		t.Fatalf("RunInstances: %v", err)
	}
	id := *out.Instances[0].InstanceId

	cfg, _, _, err := loadConfigFromEC2Tags(ctx, ec2Client, id)
	if err != nil {
		t.Fatalf("loadConfigFromEC2Tags: %v", err)
	}
	if cfg.Burst != burst {
		t.Fatalf("Config.Burst = %v from spawn:burst tag, want %v", cfg.Burst, burst)
	}

	return &EC2Provider{
		ec2Client:      ec2Client,
		identity:       &Identity{InstanceID: id, Region: "us-east-1", AccountID: "123456789012", Provider: "ec2"},
		config:         cfg,
		registryClient: dynamodb.NewFromConfig(env.AWSConfig),
		registryTable:  HybridRegistryTable,
	}, id
}

// seedRegistry creates the hybrid registry table and writes live entries in the
// shape pkg/registry.Register writes them.
func seedRegistry(t *testing.T, env *testutil.TestEnv, entries ...PeerInfo) {
	t.Helper()
	ctx := context.Background()
	client := dynamodb.NewFromConfig(env.AWSConfig)
	_, err := client.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: aws.String(HybridRegistryTable),
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
		t.Fatalf("CreateTable: %v", err)
	}
	expires := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	for _, e := range entries {
		_, err := client.PutItem(ctx, &dynamodb.PutItemInput{
			TableName: aws.String(HybridRegistryTable),
			Item: map[string]dynamodbtypes.AttributeValue{
				"job_array_id": &dynamodbtypes.AttributeValueMemberS{Value: testJobArray},
				"instance_id":  &dynamodbtypes.AttributeValueMemberS{Value: e.InstanceID},
				"index":        &dynamodbtypes.AttributeValueMemberN{Value: strconv.Itoa(e.Index)},
				"provider":     &dynamodbtypes.AttributeValueMemberS{Value: e.Provider},
				"ip_address":   &dynamodbtypes.AttributeValueMemberS{Value: e.IP},
				"expires_at":   &dynamodbtypes.AttributeValueMemberN{Value: expires},
			},
		})
		if err != nil {
			t.Fatalf("PutItem %s: %v", e.InstanceID, err)
		}
	}
}

func countByID(peers []PeerInfo) map[string]int {
	n := map[string]int{}
	for _, p := range peers {
		n[p.InstanceID]++
	}
	return n
}

// TestEC2Provider_BurstMemberDiscoversLocalNodesFromTheRegistry is #918's
// suggested test: a burst member's discovery includes a provider=local registry
// entry. The member is also in the registry itself (every burst member
// registers), so it must still be listed once, as the tag-discovered entry.
func TestEC2Provider_BurstMemberDiscoversLocalNodesFromTheRegistry(t *testing.T) {
	env := testutil.SubstrateServer(t)
	p, self := burstMember(t, env, true)
	seedRegistry(t, env,
		PeerInfo{InstanceID: "laptop-01", Index: 1, Provider: "local", IP: "203.0.113.7"},
		PeerInfo{InstanceID: self, Index: 0, Provider: "ec2", IP: "198.51.100.1"},
	)

	peers, err := p.DiscoverPeers(context.Background(), testJobArray)
	if err != nil {
		t.Fatalf("DiscoverPeers: %v", err)
	}

	n := countByID(peers)
	if n["laptop-01"] != 1 {
		t.Errorf("burst member's peers %+v: want the registry's local node laptop-01 exactly once, got %d", peers, n["laptop-01"])
	}
	if n[self] != 1 {
		t.Errorf("burst member's peers %+v: want itself exactly once (tag and registry entry deduplicated), got %d", peers, n[self])
	}
	for _, peer := range peers {
		if peer.InstanceID == self && peer.DNS == "" {
			t.Errorf("the EC2 member's merged entry %+v lost its tag-discovered DNS name", peer)
		}
		if peer.InstanceID == "laptop-01" && (peer.Provider != "local" || peer.IP != "203.0.113.7" || peer.Index != 1) {
			t.Errorf("local node entry %+v does not carry the registry's provider, IP and index", peer)
		}
	}
}

// countingRegistry records Query calls and returns a fixed answer.
type countingRegistry struct {
	calls int
	err   error
	peers []PeerInfo
}

func (c *countingRegistry) Query(_ context.Context, _ *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	out := &dynamodb.QueryOutput{}
	expires := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	for _, p := range c.peers {
		out.Items = append(out.Items, map[string]dynamodbtypes.AttributeValue{
			"instance_id": &dynamodbtypes.AttributeValueMemberS{Value: p.InstanceID},
			"index":       &dynamodbtypes.AttributeValueMemberN{Value: strconv.Itoa(p.Index)},
			"provider":    &dynamodbtypes.AttributeValueMemberS{Value: p.Provider},
			"expires_at":  &dynamodbtypes.AttributeValueMemberN{Value: expires},
		})
	}
	return out, nil
}

// TestEC2Provider_OrdinaryMemberDiscoveryIsTagOnly: a `spawn launch --count`
// member (no spawn:burst) must not read the registry, even when it holds a
// local node for the same array id. Its role has no use for the table and the
// table may not exist (#904).
func TestEC2Provider_OrdinaryMemberDiscoveryIsTagOnly(t *testing.T) {
	env := testutil.SubstrateServer(t)
	p, self := burstMember(t, env, false)
	reg := &countingRegistry{peers: []PeerInfo{{InstanceID: "laptop-01", Index: 1, Provider: "local"}}}
	p.registryClient = reg

	peers, err := p.DiscoverPeers(context.Background(), testJobArray)
	if err != nil {
		t.Fatalf("DiscoverPeers: %v", err)
	}
	if reg.calls != 0 {
		t.Errorf("an ordinary member queried the hybrid registry %d times; it must stay tag-only", reg.calls)
	}
	n := countByID(peers)
	if n["laptop-01"] != 0 {
		t.Errorf("an ordinary member discovered registry peer laptop-01: %+v", peers)
	}
	if n[self] != 1 {
		t.Errorf("an ordinary member must still find itself by tag once, got %d in %+v", n[self], peers)
	}
}

// TestEC2Provider_BurstMemberKeepsTagPeersWhenTheRegistryFails: a registry
// read failure (no table, AccessDenied) degrades to the tag peers rather than
// failing discovery, which would lose the EC2 siblings too.
func TestEC2Provider_BurstMemberKeepsTagPeersWhenTheRegistryFails(t *testing.T) {
	env := testutil.SubstrateServer(t)
	p, self := burstMember(t, env, true)
	reg := &countingRegistry{err: errors.New("ResourceNotFoundException")}
	p.registryClient = reg

	peers, err := p.DiscoverPeers(context.Background(), testJobArray)
	if err != nil {
		t.Fatalf("a registry failure must not fail discovery: %v", err)
	}
	if reg.calls == 0 {
		t.Fatal("a burst member did not query the hybrid registry")
	}
	if countByID(peers)[self] != 1 {
		t.Errorf("tag peers lost on registry failure: %+v", peers)
	}
}

// TestReadsHybridRegistry tests the gate where it lives (rule 2), including
// states the constructor does not produce.
func TestReadsHybridRegistry(t *testing.T) {
	reg := &countingRegistry{}
	cases := []struct {
		name string
		p    *EC2Provider
		want bool
	}{
		{"burst member", &EC2Provider{config: &Config{Burst: true}, registryClient: reg}, true},
		{"ordinary member", &EC2Provider{config: &Config{Burst: false}, registryClient: reg}, false},
		{"burst member without a client", &EC2Provider{config: &Config{Burst: true}}, false},
		{"no config", &EC2Provider{registryClient: reg}, false},
	}
	for _, tc := range cases {
		if got := tc.p.readsHybridRegistry(); got != tc.want {
			t.Errorf("%s: readsHybridRegistry = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestMergePeers tests the merge directly: every instance once, the tag entry
// winning a collision, registry-only peers kept, and a stable order.
func TestMergePeers(t *testing.T) {
	tag := []PeerInfo{
		{Index: 1, InstanceID: "i-b", IP: "198.51.100.2", DNS: "b.x.spore.host", Provider: "ec2"},
		{Index: 0, InstanceID: "i-a", IP: "198.51.100.1", DNS: "a.x.spore.host", Provider: "ec2"},
	}
	reg := []PeerInfo{
		{Index: 0, InstanceID: "i-a", IP: "10.0.0.1", Provider: "ec2"},
		{Index: 0, InstanceID: "laptop-01", IP: "203.0.113.7", Provider: "local"},
		{Index: 2, InstanceID: "laptop-02", IP: "203.0.113.8", Provider: "local"},
		{Index: 2, InstanceID: "laptop-02", IP: "203.0.113.9", Provider: "local"},
	}

	got := mergePeers(tag, reg)

	var ids []string
	for _, p := range got {
		ids = append(ids, p.InstanceID)
	}
	want := []string{"i-a", "laptop-01", "i-b", "laptop-02"}
	if fmt.Sprint(ids) != fmt.Sprint(want) {
		t.Fatalf("merged order = %v, want %v", ids, want)
	}
	if got[0].DNS != "a.x.spore.host" || got[0].IP != "198.51.100.1" {
		t.Errorf("i-a's merged entry %+v is not the tag-discovered one", got[0])
	}
	if got[3].IP != "203.0.113.8" {
		t.Errorf("a repeated registry entry replaced the first one: %+v", got[3])
	}
	if out := mergePeers(nil, nil); len(out) != 0 {
		t.Errorf("mergePeers(nil, nil) = %+v, want empty", out)
	}
}
