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
}

// NewPeerRegistry creates a new peer registry
func NewPeerRegistry(ctx context.Context, identity *provider.Identity) (*PeerRegistry, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
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
		"ip_address":     &types.AttributeValueMemberS{Value: r.identity.PublicIP},
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

// EnsureTable creates the DynamoDB table if it doesn't exist
func EnsureTable(ctx context.Context) error {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}

	client := dynamodb.NewFromConfig(cfg)

	// Check if table exists
	_, err = client.DescribeTable(ctx, &dynamodb.DescribeTableInput{
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
