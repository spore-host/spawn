package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// memberARNRe matches valid IAM user or role ARNs.
var memberARNRe = regexp.MustCompile(`^arn:aws:iam::\d{12}:(user|role)/.{1,256}$`)

const (
	dynamoTeamsTable       = "spawn-teams"
	dynamoMembershipsTable = "spawn-team-memberships"
)

// newUUID generates a random UUID v4 string.
func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant bits
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]),
	)
}

// resolveTeamContext returns the caller's role in a team ("owner" or "member"),
// or an error if the caller is not a member.
func resolveTeamContext(ctx context.Context, cfg aws.Config, teamID, callerARN string) (string, error) {
	ddb := dynamodb.NewFromConfig(cfg)
	result, err := ddb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(dynamoMembershipsTable),
		Key: map[string]types.AttributeValue{
			"team_id":    &types.AttributeValueMemberS{Value: teamID},
			"member_arn": &types.AttributeValueMemberS{Value: callerARN},
		},
	})
	if err != nil {
		return "", fmt.Errorf("get membership: %w", err)
	}
	if len(result.Item) == 0 {
		return "", fmt.Errorf("not a member of team %s", teamID)
	}
	var m TeamMemberRecord
	if err := attributevalue.UnmarshalMap(result.Item, &m); err != nil {
		return "", fmt.Errorf("unmarshal membership: %w", err)
	}
	return m.Role, nil
}

// handleCreateTeam handles POST /teams
func handleCreateTeam(ctx context.Context, cfg aws.Config, body, callerARN string) (events.APIGatewayProxyResponse, error) {
	var req struct {
		TeamName    string `json:"team_name"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil || req.TeamName == "" {
		return errorResponse(400, "team_name is required"), nil
	}
	if len(req.TeamName) > 100 {
		return errorResponse(400, "team_name must be 100 characters or fewer"), nil
	}
	if len(req.Description) > 1000 {
		return errorResponse(400, "description must be 1000 characters or fewer"), nil
	}

	teamID := newUUID()
	now := time.Now().UTC().Format(time.RFC3339)

	team := TeamRecord{
		TeamID:      teamID,
		TeamName:    req.TeamName,
		OwnerARN:    callerARN,
		Description: req.Description,
		CreatedAt:   now,
		MemberCount: 1,
	}

	ddb := dynamodb.NewFromConfig(cfg)

	// Write team record
	teamItem, err := attributevalue.MarshalMap(team)
	if err != nil {
		return errorResponse(500, fmt.Sprintf("marshal team: %v", err)), nil
	}
	if _, err := ddb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(dynamoTeamsTable),
		Item:      teamItem,
	}); err != nil {
		return errorResponse(500, fmt.Sprintf("create team: %v", err)), nil
	}

	// Write owner membership
	member := TeamMemberRecord{
		TeamID:    teamID,
		MemberARN: callerARN,
		Role:      "owner",
		JoinedAt:  now,
		InvitedBy: callerARN,
	}
	memberItem, err := attributevalue.MarshalMap(member)
	if err != nil {
		return errorResponse(500, fmt.Sprintf("marshal membership: %v", err)), nil
	}
	if _, err := ddb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(dynamoMembershipsTable),
		Item:      memberItem,
	}); err != nil {
		return errorResponse(500, fmt.Sprintf("create membership: %v", err)), nil
	}

	resp := map[string]interface{}{"success": true, "team": team}
	body2, _ := json.Marshal(resp)
	return events.APIGatewayProxyResponse{StatusCode: 201, Headers: corsHeaders, Body: string(body2)}, nil
}

// handleListMyTeams handles GET /teams
func handleListMyTeams(ctx context.Context, cfg aws.Config, callerARN string) (events.APIGatewayProxyResponse, error) {
	ddb := dynamodb.NewFromConfig(cfg)

	// Query memberships by caller ARN
	result, err := ddb.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(dynamoMembershipsTable),
		IndexName:              aws.String("member_arn-index"),
		KeyConditionExpression: aws.String("member_arn = :arn"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":arn": &types.AttributeValueMemberS{Value: callerARN},
		},
	})
	if err != nil {
		return errorResponse(500, fmt.Sprintf("query memberships: %v", err)), nil
	}

	type TeamWithRole struct {
		TeamRecord
		Role string `json:"role"`
	}

	var teams []TeamWithRole
	for _, item := range result.Items {
		var m TeamMemberRecord
		if err := attributevalue.UnmarshalMap(item, &m); err != nil {
			continue
		}

		// Fetch team record
		teamResult, err := ddb.GetItem(ctx, &dynamodb.GetItemInput{
			TableName: aws.String(dynamoTeamsTable),
			Key: map[string]types.AttributeValue{
				"team_id": &types.AttributeValueMemberS{Value: m.TeamID},
			},
		})
		if err != nil || len(teamResult.Item) == 0 {
			continue
		}
		var t TeamRecord
		if err := attributevalue.UnmarshalMap(teamResult.Item, &t); err != nil {
			continue
		}
		teams = append(teams, TeamWithRole{TeamRecord: t, Role: m.Role})
	}

	if teams == nil {
		teams = []TeamWithRole{}
	}

	return successResponse(map[string]interface{}{"success": true, "teams": teams})
}

// handleGetTeam handles GET /teams/{team_id}
func handleGetTeam(ctx context.Context, cfg aws.Config, teamID, callerARN string) (events.APIGatewayProxyResponse, error) {
	if _, err := resolveTeamContext(ctx, cfg, teamID, callerARN); err != nil {
		return errorResponse(403, "access denied"), nil
	}

	ddb := dynamodb.NewFromConfig(cfg)

	// Fetch team record
	teamResult, err := ddb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(dynamoTeamsTable),
		Key: map[string]types.AttributeValue{
			"team_id": &types.AttributeValueMemberS{Value: teamID},
		},
	})
	if err != nil || len(teamResult.Item) == 0 {
		return errorResponse(404, "team not found"), nil
	}
	var t TeamRecord
	if err := attributevalue.UnmarshalMap(teamResult.Item, &t); err != nil {
		return errorResponse(500, "unmarshal team"), nil
	}

	// Fetch all members
	membersResult, err := ddb.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(dynamoMembershipsTable),
		KeyConditionExpression: aws.String("team_id = :tid"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":tid": &types.AttributeValueMemberS{Value: teamID},
		},
		Limit: aws.Int32(500),
	})
	if err != nil {
		return errorResponse(500, fmt.Sprintf("query members: %v", err)), nil
	}

	var members []TeamMemberRecord
	for _, item := range membersResult.Items {
		var m TeamMemberRecord
		if err := attributevalue.UnmarshalMap(item, &m); err != nil {
			continue
		}
		members = append(members, m)
	}
	if members == nil {
		members = []TeamMemberRecord{}
	}

	return successResponse(map[string]interface{}{"success": true, "team": t, "members": members})
}

// handleAddMember handles POST /teams/{team_id}/members
func handleAddMember(ctx context.Context, cfg aws.Config, teamID, callerARN, body string) (events.APIGatewayProxyResponse, error) {
	role, err := resolveTeamContext(ctx, cfg, teamID, callerARN)
	if err != nil || role != "owner" {
		return errorResponse(403, "only team owners can add members"), nil
	}

	var req struct {
		MemberARN string `json:"member_arn"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil || req.MemberARN == "" {
		return errorResponse(400, "member_arn is required"), nil
	}
	if !memberARNRe.MatchString(req.MemberARN) {
		return errorResponse(400, "member_arn must be a valid IAM user or role ARN"), nil
	}

	ddb := dynamodb.NewFromConfig(cfg)
	now := time.Now().UTC().Format(time.RFC3339)

	member := TeamMemberRecord{
		TeamID:    teamID,
		MemberARN: req.MemberARN,
		Role:      "member",
		JoinedAt:  now,
		InvitedBy: callerARN,
	}
	item, err := attributevalue.MarshalMap(member)
	if err != nil {
		return errorResponse(500, fmt.Sprintf("marshal: %v", err)), nil
	}
	if _, err := ddb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           aws.String(dynamoMembershipsTable),
		Item:                item,
		ConditionExpression: aws.String("attribute_not_exists(team_id) AND attribute_not_exists(member_arn)"),
	}); err != nil {
		var ccfe *types.ConditionalCheckFailedException
		if errors.As(err, &ccfe) {
			// Already a member — idempotent success.
			return successResponse(map[string]interface{}{"success": true, "member": member})
		}
		return errorResponse(500, fmt.Sprintf("add member: %v", err)), nil
	}

	// Increment member_count
	if _, err := ddb.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(dynamoTeamsTable),
		Key: map[string]types.AttributeValue{
			"team_id": &types.AttributeValueMemberS{Value: teamID},
		},
		UpdateExpression: aws.String("SET member_count = member_count + :one"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":one": &types.AttributeValueMemberN{Value: "1"},
		},
	}); err != nil {
		log.Printf("warning: failed to update member_count: %v", err)
	}

	return successResponse(map[string]interface{}{"success": true, "member": member})
}

// handleRemoveMember handles DELETE /teams/{team_id}/members/{member_arn}
func handleRemoveMember(ctx context.Context, cfg aws.Config, teamID, callerARN, memberARN string) (events.APIGatewayProxyResponse, error) {
	role, err := resolveTeamContext(ctx, cfg, teamID, callerARN)
	if err != nil || role != "owner" {
		return errorResponse(403, "only team owners can remove members"), nil
	}
	if memberARN == callerARN {
		return errorResponse(400, "cannot remove yourself as owner"), nil
	}

	ddb := dynamodb.NewFromConfig(cfg)
	if _, err := ddb.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(dynamoMembershipsTable),
		Key: map[string]types.AttributeValue{
			"team_id":    &types.AttributeValueMemberS{Value: teamID},
			"member_arn": &types.AttributeValueMemberS{Value: memberARN},
		},
	}); err != nil {
		return errorResponse(500, fmt.Sprintf("remove member: %v", err)), nil
	}

	// Decrement member_count
	if _, err := ddb.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(dynamoTeamsTable),
		Key: map[string]types.AttributeValue{
			"team_id": &types.AttributeValueMemberS{Value: teamID},
		},
		UpdateExpression:    aws.String("SET member_count = member_count - :one"),
		ConditionExpression: aws.String("member_count > :zero"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":one":  &types.AttributeValueMemberN{Value: "1"},
			":zero": &types.AttributeValueMemberN{Value: "0"},
		},
	}); err != nil {
		log.Printf("warning: failed to update member_count: %v", err)
	}

	return successResponse(map[string]interface{}{"success": true})
}

// handleDeleteTeam handles DELETE /teams/{team_id}
func handleDeleteTeam(ctx context.Context, cfg aws.Config, teamID, callerARN string) (events.APIGatewayProxyResponse, error) {
	role, err := resolveTeamContext(ctx, cfg, teamID, callerARN)
	if err != nil || role != "owner" {
		return errorResponse(403, "only team owners can delete teams"), nil
	}

	ddb := dynamodb.NewFromConfig(cfg)

	// Delete all memberships
	membersResult, err := ddb.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(dynamoMembershipsTable),
		KeyConditionExpression: aws.String("team_id = :tid"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":tid": &types.AttributeValueMemberS{Value: teamID},
		},
		ProjectionExpression: aws.String("team_id, member_arn"),
		Limit:                aws.Int32(500),
	})
	if err != nil {
		return errorResponse(500, fmt.Sprintf("query members: %v", err)), nil
	}

	var deleteErrs int
	for _, item := range membersResult.Items {
		var m TeamMemberRecord
		if err := attributevalue.UnmarshalMap(item, &m); err != nil {
			continue
		}
		if _, err := ddb.DeleteItem(ctx, &dynamodb.DeleteItemInput{
			TableName: aws.String(dynamoMembershipsTable),
			Key: map[string]types.AttributeValue{
				"team_id":    &types.AttributeValueMemberS{Value: teamID},
				"member_arn": &types.AttributeValueMemberS{Value: m.MemberARN},
			},
		}); err != nil {
			log.Printf("warning: failed to delete membership %s/%s: %v", teamID, m.MemberARN, err)
			deleteErrs++
		}
	}
	if deleteErrs > 0 {
		return errorResponse(500, fmt.Sprintf("delete team: failed to remove %d membership(s)", deleteErrs)), nil
	}

	// Delete team record
	if _, err := ddb.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(dynamoTeamsTable),
		Key: map[string]types.AttributeValue{
			"team_id": &types.AttributeValueMemberS{Value: teamID},
		},
	}); err != nil {
		return errorResponse(500, fmt.Sprintf("delete team: %v", err)), nil
	}

	return successResponse(map[string]interface{}{"success": true})
}
