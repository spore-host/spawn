package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/apigatewaymanagementapi"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	connectionsTable = "spawn-websocket-connections"
)

// Connection represents a WebSocket connection in DynamoDB
type Connection struct {
	ConnectionID string    `dynamodbav:"connection_id"`
	UserID       string    `dynamodbav:"user_id"`
	ConnectedAt  time.Time `dynamodbav:"connected_at"`
	TTL          int64     `dynamodbav:"ttl"`
}

// WebSocketMessage is the message format sent to clients
type WebSocketMessage struct {
	Type      string      `json:"type"`
	Data      interface{} `json:"data"`
	Timestamp time.Time   `json:"timestamp"`
}

// handler processes DynamoDB Stream events and publishes to WebSocket connections
func handler(ctx context.Context, event events.DynamoDBEvent) error {
	// Load AWS config
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}

	dynamoClient := dynamodb.NewFromConfig(cfg)

	// Get WebSocket endpoint from environment
	wsEndpoint := os.Getenv("WEBSOCKET_ENDPOINT")
	if wsEndpoint == "" {
		return fmt.Errorf("WEBSOCKET_ENDPOINT environment variable not set")
	}

	// Create API Gateway Management API client
	mgmtClient := apigatewaymanagementapi.NewFromConfig(cfg, func(o *apigatewaymanagementapi.Options) {
		o.BaseEndpoint = aws.String(wsEndpoint)
	})

	// Get all active connections
	connections, err := getActiveConnections(ctx, dynamoClient)
	if err != nil {
		return fmt.Errorf("failed to get active connections: %w", err)
	}

	if len(connections) == 0 {
		fmt.Println("No active connections, skipping event processing")
		return nil
	}

	fmt.Printf("Processing %d stream records for %d connections\n", len(event.Records), len(connections))

	// Process each stream record
	for _, record := range event.Records {
		// Parse the stream record into an event
		eventType, eventData, userID, err := parseStreamRecord(record)
		if err != nil {
			fmt.Printf("Warning: failed to parse stream record: %v\n", err)
			continue
		}

		if eventType == "" {
			continue // Skip non-relevant events
		}

		// Create WebSocket message
		message := WebSocketMessage{
			Type:      eventType,
			Data:      eventData,
			Timestamp: time.Now(),
		}

		messageJSON, err := json.Marshal(message)
		if err != nil {
			fmt.Printf("Warning: failed to marshal message: %v\n", err)
			continue
		}

		// Publish to relevant connections
		staleConnections := []string{}
		for _, conn := range connections {
			// Filter by user ID
			if userID != "" && conn.UserID != userID {
				continue // Skip connections for different users
			}

			// Publish to connection
			err := publishToConnection(ctx, mgmtClient, conn.ConnectionID, messageJSON)
			if err != nil {
				// Check if connection is stale (410 Gone)
				if apiErr, ok := err.(interface{ ErrorCode() string }); ok && apiErr.ErrorCode() == "GoneException" {
					fmt.Printf("Connection %s is stale (410), marking for deletion\n", conn.ConnectionID)
					staleConnections = append(staleConnections, conn.ConnectionID)
				} else {
					fmt.Printf("Warning: failed to publish to connection %s: %v\n", conn.ConnectionID, err)
				}
			} else {
				fmt.Printf("Published %s event to connection %s (user: %s)\n", eventType, conn.ConnectionID, conn.UserID)
			}
		}

		// Clean up stale connections
		for _, connID := range staleConnections {
			err := deleteConnection(ctx, dynamoClient, connID)
			if err != nil {
				fmt.Printf("Warning: failed to delete stale connection %s: %v\n", connID, err)
			}
		}
	}

	return nil
}

// parseStreamRecord extracts event type, data, and user ID from a DynamoDB stream record
func parseStreamRecord(record events.DynamoDBEventRecord) (eventType string, eventData interface{}, userID string, err error) {
	// Extract table name from source ARN
	// Format: arn:aws:dynamodb:us-east-1:123456789012:table/TableName/stream/...
	arnParts := strings.Split(record.EventSourceArn, "/")
	if len(arnParts) < 2 {
		return "", nil, "", fmt.Errorf("invalid source ARN format: %s", record.EventSourceArn)
	}
	tableName := arnParts[1]

	switch tableName {
	case "spawn-sweep-orchestration":
		return parseSweepEvent(record)

	case "spawn-autoscale-groups-production":
		return parseAutoscaleEvent(record)

	default:
		return "", nil, "", nil // Skip unknown tables
	}
}

// parseSweepEvent parses a sweep table stream event
func parseSweepEvent(record events.DynamoDBEventRecord) (eventType string, eventData interface{}, userID string, err error) {
	switch record.EventName {
	case "INSERT", "MODIFY":
		// Extract sweep data from NewImage
		if record.Change.NewImage == nil {
			return "", nil, "", fmt.Errorf("NewImage is nil for INSERT/MODIFY event")
		}

		// Convert to map[string]interface{} directly from events.DynamoDBAttributeValue
		sweep := make(map[string]interface{})
		for key, val := range record.Change.NewImage {
			// Extract the underlying value from DynamoDBAttributeValue
			if strVal := val.String(); strVal != "" {
				sweep[key] = strVal
			} else if numVal := val.Number(); numVal != "" {
				sweep[key] = numVal
			} else if boolVal := val.Boolean(); boolVal {
				sweep[key] = boolVal
			}
		}

		// Extract user ID for filtering
		userID, _ = sweep["user_id"].(string)

		return "sweep:update", sweep, userID, nil

	case "REMOVE":
		// Extract sweep_id from OldImage
		if record.Change.OldImage == nil {
			return "", nil, "", fmt.Errorf("OldImage is nil for REMOVE event")
		}

		var sweepID, userIDVal string
		if val, ok := record.Change.OldImage["sweep_id"]; ok {
			sweepID = val.String()
		}
		if val, ok := record.Change.OldImage["user_id"]; ok {
			userIDVal = val.String()
		}

		data := map[string]string{"sweep_id": sweepID}
		return "sweep:delete", data, userIDVal, nil

	default:
		return "", nil, "", nil
	}
}

// parseAutoscaleEvent parses an autoscale group table stream event
func parseAutoscaleEvent(record events.DynamoDBEventRecord) (eventType string, eventData interface{}, userID string, err error) {
	switch record.EventName {
	case "INSERT", "MODIFY":
		// Extract autoscale group data from NewImage
		if record.Change.NewImage == nil {
			return "", nil, "", fmt.Errorf("NewImage is nil for INSERT/MODIFY event")
		}

		// Convert to map[string]interface{} directly from events.DynamoDBAttributeValue
		group := make(map[string]interface{})
		for key, val := range record.Change.NewImage {
			// Extract the underlying value from DynamoDBAttributeValue
			if strVal := val.String(); strVal != "" {
				group[key] = strVal
			} else if numVal := val.Number(); numVal != "" {
				group[key] = numVal
			} else if boolVal := val.Boolean(); boolVal {
				group[key] = boolVal
			}
		}

		// TODO: Extract user ID from group data or associated instances
		// For v0.22.0, broadcast to all users (frontend already filters properly)
		userID = ""

		return "autoscale:update", group, userID, nil

	case "REMOVE":
		// Extract autoscale_group_id from OldImage
		if record.Change.OldImage == nil {
			return "", nil, "", fmt.Errorf("OldImage is nil for REMOVE event")
		}

		var groupID string
		if val, ok := record.Change.OldImage["autoscale_group_id"]; ok {
			groupID = val.String()
		}

		data := map[string]string{"autoscale_group_id": groupID}
		return "autoscale:delete", data, "", nil

	default:
		return "", nil, "", nil
	}
}

// getActiveConnections retrieves all active connections from DynamoDB
func getActiveConnections(ctx context.Context, dynamoClient *dynamodb.Client) ([]Connection, error) {
	// Scan the connections table
	result, err := dynamoClient.Scan(ctx, &dynamodb.ScanInput{
		TableName: aws.String(connectionsTable),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to scan connections table: %w", err)
	}

	// Unmarshal connections
	var connections []Connection
	for _, item := range result.Items {
		var conn Connection
		if err := attributevalue.UnmarshalMap(item, &conn); err != nil {
			fmt.Printf("Warning: failed to unmarshal connection: %v\n", err)
			continue
		}
		connections = append(connections, conn)
	}

	return connections, nil
}

// publishToConnection sends a message to a WebSocket connection
func publishToConnection(ctx context.Context, mgmtClient *apigatewaymanagementapi.Client, connectionID string, message []byte) error {
	_, err := mgmtClient.PostToConnection(ctx, &apigatewaymanagementapi.PostToConnectionInput{
		ConnectionId: aws.String(connectionID),
		Data:         message,
	})
	return err
}

// deleteConnection removes a connection from DynamoDB
func deleteConnection(ctx context.Context, dynamoClient *dynamodb.Client, connectionID string) error {
	_, err := dynamoClient.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(connectionsTable),
		Key: map[string]types.AttributeValue{
			"connection_id": &types.AttributeValueMemberS{
				Value: connectionID,
			},
		},
	})
	return err
}

// main is the entry point for the Lambda function
func main() {
	lambda.Start(handler)
}
