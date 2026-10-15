package provider

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// HybridRegistryTable is the DynamoDB table every hybrid job-array member
// registers in and reads peers from. pkg/registry writes it under the same name
// (registry.TableName). This package cannot import pkg/registry, which imports
// it, so pkg/registry's tests pin the two names together.
const HybridRegistryTable = "spawn-hybrid-registry"

// registryQueryAPI is the one DynamoDB call peer discovery makes on the hybrid
// registry. *dynamodb.Client satisfies it.
type registryQueryAPI interface {
	Query(ctx context.Context, in *dynamodb.QueryInput, optFns ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
}

// queryRegistryPeers returns the live members of jobArrayID recorded in the
// hybrid registry, skipping entries whose heartbeat TTL has passed. Both the
// local provider and burst members on EC2 read the registry through it.
func queryRegistryPeers(ctx context.Context, client registryQueryAPI, table, jobArrayID string) ([]PeerInfo, error) {
	var peers []PeerInfo
	now := time.Now().Unix()

	var startKey map[string]types.AttributeValue
	for {
		result, err := client.Query(ctx, &dynamodb.QueryInput{
			TableName:              aws.String(table),
			KeyConditionExpression: aws.String("job_array_id = :job_array_id"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":job_array_id": &types.AttributeValueMemberS{Value: jobArrayID},
			},
			ExclusiveStartKey: startKey,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to query DynamoDB registry: %w", err)
		}

		for _, item := range result.Items {
			// Check if instance is still alive (heartbeat within TTL)
			if getNumberValue(item["expires_at"]) < now {
				continue
			}
			peers = append(peers, PeerInfo{
				Index:      int(getNumberValue(item["index"])),
				InstanceID: getStringValue(item["instance_id"]),
				IP:         getStringValue(item["ip_address"]),
				DNS:        "", // Can construct if needed
				Provider:   getStringValue(item["provider"]),
			})
		}

		if len(result.LastEvaluatedKey) == 0 {
			return peers, nil
		}
		startKey = result.LastEvaluatedKey
	}
}

// mergePeers combines the peers a burst member found by tag (its EC2 siblings)
// with the ones the hybrid registry lists (local nodes, and the EC2 members
// again, since they register too). Each instance appears once. Where both
// sources list an instance, the tag-discovered entry wins: it comes from EC2's
// own view of a running instance and carries the spore.host DNS name, which a
// registry entry does not. The result is sorted by index, then provider, then
// instance ID, so the order does not depend on which source answered first.
func mergePeers(tagPeers, registryPeers []PeerInfo) []PeerInfo {
	seen := make(map[string]bool, len(tagPeers)+len(registryPeers))
	merged := make([]PeerInfo, 0, len(tagPeers)+len(registryPeers))
	for _, sources := range [][]PeerInfo{tagPeers, registryPeers} {
		for _, peer := range sources {
			if peer.InstanceID != "" {
				if seen[peer.InstanceID] {
					continue
				}
				seen[peer.InstanceID] = true
			}
			merged = append(merged, peer)
		}
	}
	sort.SliceStable(merged, func(i, j int) bool {
		a, b := merged[i], merged[j]
		if a.Index != b.Index {
			return a.Index < b.Index
		}
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		return a.InstanceID < b.InstanceID
	})
	return merged
}
