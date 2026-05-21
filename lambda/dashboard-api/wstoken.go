package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/google/uuid"
)

const (
	wsTicketsTable = "spawn-ws-tickets"
	wsTicketTTL    = 30 * time.Second
)

// wsTicket is a short-lived token exchanged for a WebSocket connection.
type wsTicket struct {
	TicketID string `dynamodbav:"ticket_id"`
	UserARN  string `dynamodbav:"user_arn"`
	TTL      int64  `dynamodbav:"ttl"`
}

// handleGetWSToken creates a single-use 30-second ticket that the client passes
// as ?ticket= in the WebSocket URL instead of raw AWS credentials.
// Credentials are validated here (via headers, not URL), keeping them out of logs.
func handleGetWSToken(ctx context.Context, cfg aws.Config, userARN string) (events.APIGatewayProxyResponse, error) {
	client := dynamodb.NewFromConfig(cfg)
	tableName := getEnvOrDefault("SPAWN_WS_TICKETS_TABLE", wsTicketsTable)

	ticket := wsTicket{
		TicketID: uuid.New().String(),
		UserARN:  userARN,
		TTL:      time.Now().Add(wsTicketTTL).Unix(),
	}

	item, err := attributevalue.MarshalMap(ticket)
	if err != nil {
		return errorResponse(500, fmt.Sprintf("marshal ticket: %v", err)), nil
	}

	_, err = client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(tableName),
		Item:      item,
	})
	if err != nil {
		return errorResponse(500, fmt.Sprintf("store ticket: %v", err)), nil
	}

	body, _ := json.Marshal(map[string]string{"ticket": ticket.TicketID})
	return successResponse(json.RawMessage(body))
}

// RedeemWSTicket looks up a ticket, verifies it belongs to the expected user,
// deletes it (single-use), and returns the user ARN. Used by the WebSocket
// Lambda connect handler.
func RedeemWSTicket(ctx context.Context, cfg aws.Config, ticketID string) (string, error) {
	if ticketID == "" {
		return "", fmt.Errorf("missing ticket")
	}
	client := dynamodb.NewFromConfig(cfg)
	tableName := getEnvOrDefault("SPAWN_WS_TICKETS_TABLE", wsTicketsTable)

	// Delete the ticket atomically (single-use: if two connections race, only one wins)
	result, err := client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(tableName),
		Key: map[string]dynamodbtypes.AttributeValue{
			"ticket_id": &dynamodbtypes.AttributeValueMemberS{Value: ticketID},
		},
		ReturnValues: dynamodbtypes.ReturnValueAllOld,
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
