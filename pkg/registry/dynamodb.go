package registry

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/spore-host/spawn/pkg/provider"
)

const (
	TableName         = "spawn-hybrid-registry"
	DefaultTTLSeconds = 3600 // 1 hour
	HeartbeatInterval = 30 * time.Second
)

// PeerRegistry manages registration and discovery of instances
type PeerRegistry struct {
	client    *dynamodb.Client
	tableName string
	identity  *provider.Identity
	ttl       int64
	// publicIP, when set, is the instance's current public IP; identity's is
	// the boot-time one, and a resume from hibernation can change it (#1019).
	publicIP func() string
}

// SetPublicIPSource makes Register record fn() as the instance's IP rather
// than the identity's boot-time public IP (#1019). Set before the first
// Register; not safe to call concurrently with it.
func (r *PeerRegistry) SetPublicIPSource(fn func() string) { r.publicIP = fn }

// ip is the public IP to register: the source's, else the identity's.
func (r *PeerRegistry) ip() string {
	if r.publicIP != nil {
		return r.publicIP()
	}
	return r.identity.PublicIP
}

// NewPeerRegistry creates a new peer registry.
//
// The client is pinned to identity.Region (#899). The table is not in the
// spore.host infra account: `spawn burst` creates it (EnsureTable) in the
// launching account and region, and launches the burst instances into that same
// region, so the instance's own region is where the table lives. It used to be
// the ambient chain alone — but the SDK does not read the region from IMDS and
// instances carry no AWS_REGION, so on EC2 every Register/Heartbeat/Deregister
// failed with "Missing Region" before reaching DynamoDB. The ambient chain is
// still the fallback for an identity with no AWS region (a local provider whose
// config omits it — see Identity.AWSRegion).
func NewPeerRegistry(ctx context.Context, identity *provider.Identity) (*PeerRegistry, error) {
	var opts []func(*config.LoadOptions) error
	if region := identity.AWSRegion(); region != "" {
		opts = append(opts, config.WithRegion(region))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	client := dynamodb.NewFromConfig(cfg)

	return &PeerRegistry{
		client:    client,
		tableName: TableName,
		identity:  identity,
		ttl:       DefaultTTLSeconds,
	}, nil
}

// NewPeerRegistryFromConfig creates a new peer registry with an explicit AWS config.
// Primarily used in tests to point at an emulator (e.g. Substrate).
func NewPeerRegistryFromConfig(identity *provider.Identity, cfg aws.Config) *PeerRegistry {
	return &PeerRegistry{
		client:    dynamodb.NewFromConfig(cfg),
		tableName: TableName,
		identity:  identity,
		ttl:       DefaultTTLSeconds,
	}
}

// Register registers this instance in the registry
func (r *PeerRegistry) Register(ctx context.Context, jobArrayID string, index int) error {
	now := time.Now().Unix()
	expiresAt := now + r.ttl

	item := map[string]types.AttributeValue{
		"job_array_id":   &types.AttributeValueMemberS{Value: jobArrayID},
		"instance_id":    &types.AttributeValueMemberS{Value: r.identity.InstanceID},
		"index":          &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", index)},
		"provider":       &types.AttributeValueMemberS{Value: r.identity.Provider},
		"ip_address":     &types.AttributeValueMemberS{Value: r.ip()},
		"private_ip":     &types.AttributeValueMemberS{Value: r.identity.PrivateIP},
		"region":         &types.AttributeValueMemberS{Value: r.identity.Region},
		"registered_at":  &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", now)},
		"last_heartbeat": &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", now)},
		"expires_at":     &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", expiresAt)},
		"status":         &types.AttributeValueMemberS{Value: "running"},
	}

	_, err := r.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(r.tableName),
		Item:      item,
	})

	if err != nil {
		return fmt.Errorf("failed to register instance: %w", err)
	}

	log.Printf("Registered in hybrid registry: job_array=%s, instance=%s, provider=%s",
		jobArrayID, r.identity.InstanceID, r.identity.Provider)

	return nil
}

// Heartbeat updates the last_heartbeat timestamp
func (r *PeerRegistry) Heartbeat(ctx context.Context, jobArrayID string) error {
	now := time.Now().Unix()
	expiresAt := now + r.ttl

	_, err := r.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(r.tableName),
		Key: map[string]types.AttributeValue{
			"job_array_id": &types.AttributeValueMemberS{Value: jobArrayID},
			"instance_id":  &types.AttributeValueMemberS{Value: r.identity.InstanceID},
		},
		UpdateExpression: aws.String("SET last_heartbeat = :now, expires_at = :expires"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":now":     &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", now)},
			":expires": &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", expiresAt)},
		},
	})

	if err != nil {
		return fmt.Errorf("failed to send heartbeat: %w", err)
	}

	return nil
}

// Deregister removes this instance from the registry
func (r *PeerRegistry) Deregister(ctx context.Context, jobArrayID string) error {
	_, err := r.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(r.tableName),
		Key: map[string]types.AttributeValue{
			"job_array_id": &types.AttributeValueMemberS{Value: jobArrayID},
			"instance_id":  &types.AttributeValueMemberS{Value: r.identity.InstanceID},
		},
	})

	if err != nil {
		return fmt.Errorf("failed to deregister instance: %w", err)
	}

	log.Printf("Deregistered from hybrid registry: job_array=%s, instance=%s",
		jobArrayID, r.identity.InstanceID)

	return nil
}

// DiscoverPeers finds all instances in the same job array
func (r *PeerRegistry) DiscoverPeers(ctx context.Context, jobArrayID string) ([]provider.PeerInfo, error) {
	result, err := r.client.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(r.tableName),
		KeyConditionExpression: aws.String("job_array_id = :job_array_id"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":job_array_id": &types.AttributeValueMemberS{Value: jobArrayID},
		},
	})

	if err != nil {
		return nil, fmt.Errorf("failed to query peers: %w", err)
	}

	var peers []provider.PeerInfo
	now := time.Now().Unix()

	for _, item := range result.Items {
		// Check if instance is still alive (heartbeat within TTL)
		expiresAt := getNumberValue(item["expires_at"])
		if expiresAt < now {
			// Instance expired, skip it
			continue
		}

		peer := provider.PeerInfo{
			Index:      int(getNumberValue(item["index"])),
			InstanceID: getStringValue(item["instance_id"]),
			IP:         getStringValue(item["ip_address"]),
			DNS:        "", // DNS can be constructed if needed
			Provider:   getStringValue(item["provider"]),
		}

		peers = append(peers, peer)
	}

	log.Printf("Discovered %d peers in job array %s", len(peers), jobArrayID)
	return peers, nil
}

// StartHeartbeat starts a background heartbeat goroutine
func (r *PeerRegistry) StartHeartbeat(ctx context.Context, jobArrayID string) {
	ticker := time.NewTicker(HeartbeatInterval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := r.Heartbeat(ctx, jobArrayID); err != nil {
					log.Printf("Heartbeat failed: %v", err)
				}
			}
		}
	}()
}

// Helper functions to extract values from DynamoDB items

func getStringValue(attr types.AttributeValue) string {
	if s, ok := attr.(*types.AttributeValueMemberS); ok {
		return s.Value
	}
	return ""
}

func getNumberValue(attr types.AttributeValue) int64 {
	if n, ok := attr.(*types.AttributeValueMemberN); ok {
		val, err := strconv.ParseInt(n.Value, 10, 64)
		if err != nil {
			log.Printf("warning: unexpected DynamoDB number value %q: %v", n.Value, err)
			return 0
		}
		return val
	}
	return 0
}

// DiscoverPeersForJobArray discovers peers without requiring an identity (for orchestrator)
func DiscoverPeersForJobArray(ctx context.Context, cfg aws.Config, jobArrayID string) ([]PeerInfo, error) {
	client := dynamodb.NewFromConfig(cfg)

	result, err := client.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(TableName),
		KeyConditionExpression: aws.String("job_array_id = :job_array_id"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":job_array_id": &types.AttributeValueMemberS{Value: jobArrayID},
		},
	})

	if err != nil {
		return nil, fmt.Errorf("failed to query peers: %w", err)
	}

	var peers []PeerInfo
	now := time.Now().Unix()

	for _, item := range result.Items {
		// Check if instance is still alive
		expiresAt := getNumberValue(item["expires_at"])
		if expiresAt < now {
			continue
		}

		peer := PeerInfo{
			Index:      int(getNumberValue(item["index"])),
			InstanceID: getStringValue(item["instance_id"]),
			IP:         getStringValue(item["ip_address"]),
			DNS:        "",
			Provider:   getStringValue(item["provider"]),
		}

		peers = append(peers, peer)
	}

	return peers, nil
}

// PeerInfo represents a peer instance (duplicate from provider package to avoid circular import)
type PeerInfo struct {
	Index      int
	InstanceID string
	IP         string
	DNS        string
	Provider   string
}

// EnsureTable creates the hybrid registry table, in cfg's account and region,
// if it does not exist.
//
// The caller passes the config it launches members with, so the table is
// created where those members look for it: spored reads the table in its own
// instance's region. EnsureTable used to load the ambient default config, which
// ignored `spawn burst --region` and `--profile` (#939).
func EnsureTable(ctx context.Context, cfg aws.Config) error {
	if cfg.Region == "" {
		return fmt.Errorf("ensure %s: no AWS region in the config", TableName)
	}
	client := dynamodb.NewFromConfig(cfg)

	// Check if table exists
	_, err := client.DescribeTable(ctx, &dynamodb.DescribeTableInput{
		TableName: aws.String(TableName),
	})

	if err == nil {
		// Table exists
		return nil
	}

	// Create table
	_, err = client.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: aws.String(TableName),
		KeySchema: []types.KeySchemaElement{
			{
				AttributeName: aws.String("job_array_id"),
				KeyType:       types.KeyTypeHash,
			},
			{
				AttributeName: aws.String("instance_id"),
				KeyType:       types.KeyTypeRange,
			},
		},
		AttributeDefinitions: []types.AttributeDefinition{
			{
				AttributeName: aws.String("job_array_id"),
				AttributeType: types.ScalarAttributeTypeS,
			},
			{
				AttributeName: aws.String("instance_id"),
				AttributeType: types.ScalarAttributeTypeS,
			},
		},
		BillingMode: types.BillingModePayPerRequest,
		Tags: []types.Tag{
			{
				Key:   aws.String("Application"),
				Value: aws.String("spawn"),
			},
			{
				Key:   aws.String("Purpose"),
				Value: aws.String("hybrid-compute-registry"),
			},
		},
	})

	if err != nil {
		return fmt.Errorf("failed to create table: %w", err)
	}

	log.Printf("Created DynamoDB table: %s", TableName)

	// Wait for table to be active
	waiter := dynamodb.NewTableExistsWaiter(client)
	return waiter.Wait(ctx, &dynamodb.DescribeTableInput{
		TableName: aws.String(TableName),
	}, 2*time.Minute)
}
