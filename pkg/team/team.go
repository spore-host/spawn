// Package team provides a DynamoDB store for spawn's team + membership records.
// It mirrors the pkg/alerts.Client pattern: a Client wraps a *dynamodb.Client
// (built by the caller from the correct account's config) and holds the two
// table names so tests can override them.
//
// The item structs (TeamRecord, MemberRecord) and their dynamodbav tags are the
// on-the-wire format shared with the dashboard-api Lambda (a separate module), so
// the tag values and table names must not change.
package team

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Default table names for the team domain.
const (
	DefaultTeamsTable       = "spawn-teams"
	DefaultMembershipsTable = "spawn-team-memberships"

	// memberARNIndex is the GSI on the memberships table keyed by member_arn,
	// used to list the teams a given member belongs to.
	memberARNIndex = "member_arn-index"
)

// TeamRecord mirrors the DynamoDB schema for the teams table (hash key team_id).
type TeamRecord struct {
	TeamID      string `dynamodbav:"team_id"`
	TeamName    string `dynamodbav:"team_name"`
	OwnerARN    string `dynamodbav:"owner_arn"`
	Description string `dynamodbav:"description,omitempty"`
	CreatedAt   string `dynamodbav:"created_at"`
	MemberCount int    `dynamodbav:"member_count"`
}

// MemberRecord mirrors the DynamoDB schema for the memberships table
// (hash key team_id, range key member_arn).
type MemberRecord struct {
	TeamID    string `dynamodbav:"team_id"`
	MemberARN string `dynamodbav:"member_arn"`
	Role      string `dynamodbav:"role"`
	JoinedAt  string `dynamodbav:"joined_at"`
	InvitedBy string `dynamodbav:"invited_by"`
}

// Client provides team + membership operations backed by DynamoDB.
type Client struct {
	db               *dynamodb.Client
	teamsTable       string
	membershipsTable string
}

// NewClient returns a Client using the default table names.
func NewClient(db *dynamodb.Client) *Client {
	return &Client{db: db, teamsTable: DefaultTeamsTable, membershipsTable: DefaultMembershipsTable}
}

// NewClientWithTableNames returns a Client using explicit table names (tests).
func NewClientWithTableNames(db *dynamodb.Client, teamsTable, membershipsTable string) *Client {
	return &Client{db: db, teamsTable: teamsTable, membershipsTable: membershipsTable}
}

// PutTeam writes a team record.
func (c *Client) PutTeam(ctx context.Context, team *TeamRecord) error {
	item, err := attributevalue.MarshalMap(team)
	if err != nil {
		return fmt.Errorf("marshal team: %w", err)
	}
	if _, err := c.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(c.teamsTable),
		Item:      item,
	}); err != nil {
		return fmt.Errorf("put team: %w", err)
	}
	return nil
}

// GetTeam reads one team by ID. Returns (nil, nil) when the team does not exist.
func (c *Client) GetTeam(ctx context.Context, teamID string) (*TeamRecord, error) {
	result, err := c.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(c.teamsTable),
		Key: map[string]types.AttributeValue{
			"team_id": &types.AttributeValueMemberS{Value: teamID},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("get team: %w", err)
	}
	if len(result.Item) == 0 {
		return nil, nil
	}
	var t TeamRecord
	if err := attributevalue.UnmarshalMap(result.Item, &t); err != nil {
		return nil, fmt.Errorf("unmarshal team: %w", err)
	}
	return &t, nil
}

// DeleteTeam deletes a team record (not its memberships).
func (c *Client) DeleteTeam(ctx context.Context, teamID string) error {
	if _, err := c.db.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(c.teamsTable),
		Key: map[string]types.AttributeValue{
			"team_id": &types.AttributeValueMemberS{Value: teamID},
		},
	}); err != nil {
		return fmt.Errorf("delete team: %w", err)
	}
	return nil
}

// IncrementMemberCount adds one to a team's member_count.
func (c *Client) IncrementMemberCount(ctx context.Context, teamID string) error {
	_, err := c.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(c.teamsTable),
		Key: map[string]types.AttributeValue{
			"team_id": &types.AttributeValueMemberS{Value: teamID},
		},
		UpdateExpression: aws.String("SET member_count = member_count + :one"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":one": &types.AttributeValueMemberN{Value: "1"},
		},
	})
	return err
}

// DecrementMemberCount subtracts one from a team's member_count, guarded so it
// never goes below zero (ConditionExpression member_count > 0).
func (c *Client) DecrementMemberCount(ctx context.Context, teamID string) error {
	_, err := c.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(c.teamsTable),
		Key: map[string]types.AttributeValue{
			"team_id": &types.AttributeValueMemberS{Value: teamID},
		},
		UpdateExpression:    aws.String("SET member_count = member_count - :one"),
		ConditionExpression: aws.String("member_count > :zero"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":one":  &types.AttributeValueMemberN{Value: "1"},
			":zero": &types.AttributeValueMemberN{Value: "0"},
		},
	})
	return err
}

// PutMembership writes a membership record.
func (c *Client) PutMembership(ctx context.Context, m *MemberRecord) error {
	item, err := attributevalue.MarshalMap(m)
	if err != nil {
		return fmt.Errorf("marshal membership: %w", err)
	}
	if _, err := c.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(c.membershipsTable),
		Item:      item,
	}); err != nil {
		return fmt.Errorf("put membership: %w", err)
	}
	return nil
}

// DeleteMembership removes a member from a team.
func (c *Client) DeleteMembership(ctx context.Context, teamID, memberARN string) error {
	if _, err := c.db.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(c.membershipsTable),
		Key: map[string]types.AttributeValue{
			"team_id":    &types.AttributeValueMemberS{Value: teamID},
			"member_arn": &types.AttributeValueMemberS{Value: memberARN},
		},
	}); err != nil {
		return fmt.Errorf("delete membership: %w", err)
	}
	return nil
}

// GetMembership returns the membership for (teamID, memberARN). Returns
// (nil, nil) when the caller is not a member.
func (c *Client) GetMembership(ctx context.Context, teamID, memberARN string) (*MemberRecord, error) {
	result, err := c.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(c.membershipsTable),
		Key: map[string]types.AttributeValue{
			"team_id":    &types.AttributeValueMemberS{Value: teamID},
			"member_arn": &types.AttributeValueMemberS{Value: memberARN},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("get membership: %w", err)
	}
	if len(result.Item) == 0 {
		return nil, nil
	}
	var m MemberRecord
	if err := attributevalue.UnmarshalMap(result.Item, &m); err != nil {
		return nil, fmt.Errorf("unmarshal membership: %w", err)
	}
	return &m, nil
}

// ListTeamsForMember returns every membership for memberARN (via the
// member_arn-index GSI) — i.e. the teams the caller belongs to.
func (c *Client) ListTeamsForMember(ctx context.Context, memberARN string) ([]MemberRecord, error) {
	result, err := c.db.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(c.membershipsTable),
		IndexName:              aws.String(memberARNIndex),
		KeyConditionExpression: aws.String("member_arn = :arn"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":arn": &types.AttributeValueMemberS{Value: memberARN},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("query memberships: %w", err)
	}
	return unmarshalMembers(result.Items), nil
}

// ListMembers returns every membership for a team.
func (c *Client) ListMembers(ctx context.Context, teamID string) ([]MemberRecord, error) {
	result, err := c.db.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(c.membershipsTable),
		KeyConditionExpression: aws.String("team_id = :tid"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":tid": &types.AttributeValueMemberS{Value: teamID},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("query members: %w", err)
	}
	return unmarshalMembers(result.Items), nil
}

func unmarshalMembers(items []map[string]types.AttributeValue) []MemberRecord {
	members := make([]MemberRecord, 0, len(items))
	for _, item := range items {
		var m MemberRecord
		if err := attributevalue.UnmarshalMap(item, &m); err != nil {
			continue
		}
		members = append(members, m)
	}
	return members
}
