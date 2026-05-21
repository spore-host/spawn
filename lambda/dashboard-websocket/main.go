package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	connectionsTable = "spawn-websocket-connections"
	connectionTTL    = 2 * time.Hour
)

// Connection represents a WebSocket connection in DynamoDB
type Connection struct {
	ConnectionID string    `dynamodbav:"connection_id"`
	UserID       string    `dynamodbav:"user_id"`
	ConnectedAt  time.Time `dynamodbav:"connected_at"`
	TTL          int64     `dynamodbav:"ttl"`
}

// wsTicket mirrors the ticket stored by dashboard-api's handleGetWSToken.
type wsTicket struct {
	TicketID string `dynamodbav:"ticket_id"`
	UserARN  string `dynamodbav:"user_arn"`
	TTL      int64  `dynamodbav:"ttl"`
}

// handler is the main Lambda handler for WebSocket events
func handler(ctx context.Context, request events.APIGatewayWebsocketProxyRequest) (events.APIGatewayProxyResponse, error) {
	// Load AWS config
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return errorResponse(500, "Failed to load AWS config"), nil
	}

	dynamoClient := dynamodb.NewFromConfig(cfg)

	// Route based on connection event
	switch request.RequestContext.RouteKey {
	case "$connect":
		return handleConnect(ctx, cfg, dynamoClient, request)
	case "$disconnect":
		return handleDisconnect(ctx, dynamoClient, request.RequestContext.ConnectionID)
	case "$default":
		return handleMessage(ctx, dynamoClient, request)
	default:
		return errorResponse(400, "Unknown route"), nil
	}
}

// handleConnect handles WebSocket connection establishment using a short-lived opaque ticket.
// The client obtains a ticket from POST /api/ws-token (dashboard-api) and passes it as
// ?ticket= in the WebSocket URL. This keeps AWS credentials out of URLs and server logs.
func handleConnect(ctx context.Context, cfg aws.Config, dynamoClient *dynamodb.Client, request events.APIGatewayWebsocketProxyRequest) (events.APIGatewayProxyResponse, error) {
	ticketParam, ok := request.QueryStringParameters["ticket"]
	if !ok || ticketParam == "" {
		return errorResponse(401, "Missing connection ticket"), nil
	}

	// Redeem the ticket: single-use, 30-second TTL
	userID, err := redeemWSTicket(ctx, cfg, ticketParam)
	if err != nil {
		return errorResponse(401, fmt.Sprintf("Invalid or expired ticket: %v", err)), nil
	}

	// Save connection to DynamoDB
	connection := Connection{
		ConnectionID: request.RequestContext.ConnectionID,
		UserID:       userID,
		ConnectedAt:  time.Now(),
		TTL:          time.Now().Add(connectionTTL).Unix(),
	}

	item, err := attributevalue.MarshalMap(connection)
	if err != nil {
		return errorResponse(500, fmt.Sprintf("Failed to marshal connection: %v", err)), nil
	}

	_, err = dynamoClient.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(connectionsTable),
		Item:      item,
	})
	if err != nil {
		return errorResponse(500, fmt.Sprintf("Failed to save connection: %v", err)), nil
	}

	fmt.Printf("Connection established: %s (user: %s)\n", connection.ConnectionID, userID)

	return events.APIGatewayProxyResponse{
		StatusCode: 200,
		Body:       "Connected",
	}, nil
}

// handleDisconnect handles WebSocket disconnection
func handleDisconnect(ctx context.Context, dynamoClient *dynamodb.Client, connectionID string) (events.APIGatewayProxyResponse, error) {
	// Delete connection from DynamoDB
	_, err := dynamoClient.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(connectionsTable),
		Key: map[string]types.AttributeValue{
			"connection_id": &types.AttributeValueMemberS{
				Value: connectionID,
			},
		},
	})
	if err != nil {
		fmt.Printf("Warning: failed to delete connection %s: %v\n", connectionID, err)
		// Don't fail the disconnect - connection is already closed
	} else {
		fmt.Printf("Connection disconnected: %s\n", connectionID)
	}

	return events.APIGatewayProxyResponse{
		StatusCode: 200,
		Body:       "Disconnected",
	}, nil
}

// handleMessage handles messages from client
func handleMessage(ctx context.Context, dynamoClient *dynamodb.Client, request events.APIGatewayWebsocketProxyRequest) (events.APIGatewayProxyResponse, error) {
	// Future: handle client→server messages (subscribe to specific resources)
	// v0.22.0: just acknowledge receipt
	fmt.Printf("Received message from connection %s: %s\n", request.RequestContext.ConnectionID, request.Body)

	return events.APIGatewayProxyResponse{
		StatusCode: 200,
		Body:       "Message received",
	}, nil
}

// redeemWSTicket atomically deletes a ticket from DynamoDB and returns the user ARN.
// Tickets are single-use and expire after 30 seconds.
func redeemWSTicket(ctx context.Context, cfg aws.Config, ticketID string) (string, error) {
	client := dynamodb.NewFromConfig(cfg)
	tableName := os.Getenv("SPAWN_WS_TICKETS_TABLE")
	if tableName == "" {
		tableName = "spawn-ws-tickets"
	}

	// DeleteItem with ReturnValues=ALL_OLD is atomic — if two connections race,
	// only the first delete succeeds; the second gets nil Attributes.
	result, err := client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(tableName),
		Key: map[string]types.AttributeValue{
			"ticket_id": &types.AttributeValueMemberS{Value: ticketID},
		},
		ReturnValues: types.ReturnValueAllOld,
	})
	if err != nil {
		return "", fmt.Errorf("redeem ticket: %w", err)
	}
	if result.Attributes == nil {
		return "", fmt.Errorf("ticket not found or already used")
	}

	var ticket wsTicket
	if err := attributevalue.UnmarshalMap(result.Attributes, &ticket); err != nil {
		return "", fmt.Errorf("unmarshal ticket: %w", err)
	}

	// DynamoDB TTL deletion is eventual; enforce expiry explicitly
	if time.Now().Unix() > ticket.TTL {
		return "", fmt.Errorf("ticket expired")
	}

	return ticket.UserARN, nil
}

// errorResponse creates an error response
func errorResponse(statusCode int, message string) events.APIGatewayProxyResponse {
	body := fmt.Sprintf(`{"error": "%s"}`, message)
	return events.APIGatewayProxyResponse{
		StatusCode: statusCode,
		Body:       body,
	}
}

// main is the entry point for the Lambda function
func main() {
	lambda.Start(handler)
}
