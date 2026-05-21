package autoscaler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Client provides DynamoDB operations for autoscale groups
type Client struct {
	client    *dynamodb.Client
	tableName string
}

// NewClient creates a new DynamoDB client for autoscale groups
func NewClient(client *dynamodb.Client, tableName string) *Client {
	return &Client{
		client:    client,
		tableName: tableName,
	}
}

// CreateGroup creates a new autoscale group record
func (c *Client) CreateGroup(ctx context.Context, group *AutoScaleGroup) error {
	group.CreatedAt = time.Now()
	group.UpdatedAt = time.Now()

	item, err := attributevalue.MarshalMap(group)
	if err != nil {
		return fmt.Errorf("marshal group: %w", err)
	}

	_, err = c.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(c.tableName),
		Item:      item,
	})
	if err != nil {
		return fmt.Errorf("put group: %w", err)
	}

	return nil
}

// GetGroup retrieves an autoscale group by ID
func (c *Client) GetGroup(ctx context.Context, groupID string) (*AutoScaleGroup, error) {
	result, err := c.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(c.tableName),
		Key: map[string]types.AttributeValue{
			"autoscale_group_id": &types.AttributeValueMemberS{Value: groupID},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("get group: %w", err)
	}

	if result.Item == nil {
		return nil, fmt.Errorf("group not found: %s", groupID)
	}

	var group AutoScaleGroup
	if err := attributevalue.UnmarshalMap(result.Item, &group); err != nil {
		return nil, fmt.Errorf("unmarshal group: %w", err)
	}

	return &group, nil
}

// UpdateGroup updates an existing autoscale group
func (c *Client) UpdateGroup(ctx context.Context, group *AutoScaleGroup) error {
	group.UpdatedAt = time.Now()

	item, err := attributevalue.MarshalMap(group)
	if err != nil {
		return fmt.Errorf("marshal group: %w", err)
	}

	_, err = c.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(c.tableName),
		Item:      item,
	})
	if err != nil {
		return fmt.Errorf("update group: %w", err)
	}

	return nil
}

// DeleteGroup deletes an autoscale group
func (c *Client) DeleteGroup(ctx context.Context, groupID string) error {
	_, err := c.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(c.tableName),
		Key: map[string]types.AttributeValue{
			"autoscale_group_id": &types.AttributeValueMemberS{Value: groupID},
		},
	})
	if err != nil {
		return fmt.Errorf("delete group: %w", err)
	}

	return nil
}

// ListActiveGroups returns all groups with status "active"
func (c *Client) ListActiveGroups(ctx context.Context) ([]*AutoScaleGroup, error) {
	result, err := c.client.Scan(ctx, &dynamodb.ScanInput{
		TableName:        aws.String(c.tableName),
		FilterExpression: aws.String("#status = :active"),
		ExpressionAttributeNames: map[string]string{
			"#status": "status",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":active": &types.AttributeValueMemberS{Value: "active"},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("scan groups: %w", err)
	}

	groups := make([]*AutoScaleGroup, 0, len(result.Items))
	for _, item := range result.Items {
		var group AutoScaleGroup
		if err := attributevalue.UnmarshalMap(item, &group); err != nil {
			return nil, fmt.Errorf("unmarshal group: %w", err)
		}
		groups = append(groups, &group)
	}

	return groups, nil
}

// EnsureTable creates the autoscale groups table if it doesn't exist
func (c *Client) EnsureTable(ctx context.Context) error {
	_, err := c.client.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:   aws.String(c.tableName),
		BillingMode: types.BillingModePayPerRequest,
		AttributeDefinitions: []types.AttributeDefinition{
			{
				AttributeName: aws.String("autoscale_group_id"),
				AttributeType: types.ScalarAttributeTypeS,
			},
		},
		KeySchema: []types.KeySchemaElement{
			{
				AttributeName: aws.String("autoscale_group_id"),
				KeyType:       types.KeyTypeHash,
			},
		},
	})

	if err != nil {
		// Ignore ResourceInUseException (table already exists)
		var resourceInUse *types.ResourceInUseException
		if errors.As(err, &resourceInUse) {
			return nil
		}
		return fmt.Errorf("create table: %w", err)
	}

	return nil
}

// GetGroupByName retrieves an autoscale group by its name
func (c *Client) GetGroupByName(ctx context.Context, name string) (*AutoScaleGroup, error) {
	result, err := c.client.Scan(ctx, &dynamodb.ScanInput{
		TableName:        aws.String(c.tableName),
		FilterExpression: aws.String("group_name = :name"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":name": &types.AttributeValueMemberS{Value: name},
		},
		Limit: aws.Int32(1),
	})
	if err != nil {
		return nil, fmt.Errorf("scan for group: %w", err)
	}

	if len(result.Items) == 0 {
		return nil, fmt.Errorf("group not found: %s", name)
	}

	var group AutoScaleGroup
	if err := attributevalue.UnmarshalMap(result.Items[0], &group); err != nil {
		return nil, fmt.Errorf("unmarshal group: %w", err)
	}

	return &group, nil
}
