package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/spf13/cobra"
)

const (
	teamTable       = "spawn-teams"
	membershipTable = "spawn-team-memberships"
)

// teamRecord mirrors the DynamoDB schema for spawn-teams.
type teamRecord struct {
	TeamID      string `dynamodbav:"team_id"`
	TeamName    string `dynamodbav:"team_name"`
	OwnerARN    string `dynamodbav:"owner_arn"`
	Description string `dynamodbav:"description,omitempty"`
	CreatedAt   string `dynamodbav:"created_at"`
	MemberCount int    `dynamodbav:"member_count"`
}

// memberRecord mirrors the DynamoDB schema for spawn-team-memberships.
type memberRecord struct {
	TeamID    string `dynamodbav:"team_id"`
	MemberARN string `dynamodbav:"member_arn"`
	Role      string `dynamodbav:"role"`
	JoinedAt  string `dynamodbav:"joined_at"`
	InvitedBy string `dynamodbav:"invited_by"`
}

var (
	teamName        string
	teamDescription string
)

var teamCmd = &cobra.Command{
	Use:   "team",
	Short: "Manage team-based resource sharing",
	Long:  "Create and manage teams for sharing spawn instances, sweeps, and autoscale groups",
}

var teamCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new team",
	RunE:  runTeamCreate,
}

var teamListCmd = &cobra.Command{
	Use:   "list",
	Short: "List teams you own or belong to",
	RunE:  runTeamList,
}

var teamShowCmd = &cobra.Command{
	Use:   "show <team_id>",
	Short: "Show team details and member list",
	Args:  cobra.ExactArgs(1),
	RunE:  runTeamShow,
}

var teamAddCmd = &cobra.Command{
	Use:   "add <team_id> <iam_arn>",
	Short: "Add a member to a team (owner only)",
	Args:  cobra.ExactArgs(2),
	RunE:  runTeamAdd,
}

var teamRemoveCmd = &cobra.Command{
	Use:   "remove <team_id> <iam_arn>",
	Short: "Remove a member from a team (owner only)",
	Args:  cobra.ExactArgs(2),
	RunE:  runTeamRemove,
}

var teamDeleteCmd = &cobra.Command{
	Use:   "delete <team_id>",
	Short: "Delete a team and all memberships (owner only)",
	Args:  cobra.ExactArgs(1),
	RunE:  runTeamDelete,
}

func init() {
	rootCmd.AddCommand(teamCmd)
	teamCmd.AddCommand(teamCreateCmd)
	teamCmd.AddCommand(teamListCmd)
	teamCmd.AddCommand(teamShowCmd)
	teamCmd.AddCommand(teamAddCmd)
	teamCmd.AddCommand(teamRemoveCmd)
	teamCmd.AddCommand(teamDeleteCmd)

	teamCreateCmd.Flags().StringVar(&teamName, "name", "", "Team name (required)")
	teamCreateCmd.Flags().StringVar(&teamDescription, "description", "", "Team description")
	_ = teamCreateCmd.MarkFlagRequired("name")
}

// teamDDBClient returns a DynamoDB client using the default config.
func teamDDBClient(ctx context.Context) (*dynamodb.Client, string, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("load AWS config: %w", err)
	}
	// Get caller identity ARN via STS
	callerARN, err := getCallerARN(ctx, cfg)
	if err != nil {
		return nil, "", fmt.Errorf("get caller identity: %w", err)
	}
	return dynamodb.NewFromConfig(cfg), callerARN, nil
}

// getCallerARN returns the caller's IAM ARN via STS GetCallerIdentity.
func getCallerARN(ctx context.Context, cfg aws.Config) (string, error) {
	stsClient := sts.NewFromConfig(cfg)
	identity, err := stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", fmt.Errorf("get caller identity: %w", err)
	}
	if identity.Arn == nil {
		return "", fmt.Errorf("caller identity ARN is nil")
	}
	return *identity.Arn, nil
}

func genTeamID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]),
	)
}

func runTeamCreate(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	ddb, callerARN, err := teamDDBClient(ctx)
	if err != nil {
		return err
	}

	teamID := genTeamID()
	now := time.Now().UTC().Format(time.RFC3339)

	team := teamRecord{
		TeamID:      teamID,
		TeamName:    teamName,
		OwnerARN:    callerARN,
		Description: teamDescription,
		CreatedAt:   now,
		MemberCount: 1,
	}
	teamItem, err := attributevalue.MarshalMap(team)
	if err != nil {
		return fmt.Errorf("marshal team: %w", err)
	}
	if _, err := ddb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(teamTable),
		Item:      teamItem,
	}); err != nil {
		return fmt.Errorf("create team: %w", err)
	}

	member := memberRecord{
		TeamID:    teamID,
		MemberARN: callerARN,
		Role:      "owner",
		JoinedAt:  now,
		InvitedBy: callerARN,
	}
	memberItem, err := attributevalue.MarshalMap(member)
	if err != nil {
		return fmt.Errorf("marshal membership: %w", err)
	}
	if _, err := ddb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(membershipTable),
		Item:      memberItem,
	}); err != nil {
		return fmt.Errorf("create membership: %w", err)
	}

	_, _ = fmt.Fprintf(os.Stdout, "Created team %q\n  ID:    %s\n  Owner: %s\n", team.TeamName, teamID, callerARN)
	return nil
}

func runTeamList(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	ddb, callerARN, err := teamDDBClient(ctx)
	if err != nil {
		return err
	}

	result, err := ddb.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(membershipTable),
		IndexName:              aws.String("member_arn-index"),
		KeyConditionExpression: aws.String("member_arn = :arn"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":arn": &types.AttributeValueMemberS{Value: callerARN},
		},
	})
	if err != nil {
		return fmt.Errorf("query memberships: %w", err)
	}

	if len(result.Items) == 0 {
		fmt.Println("No teams found.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "TEAM ID\tNAME\tROLE\tMEMBERS\tCREATED")
	for _, item := range result.Items {
		var m memberRecord
		if err := attributevalue.UnmarshalMap(item, &m); err != nil {
			continue
		}
		tr, err := ddb.GetItem(ctx, &dynamodb.GetItemInput{
			TableName: aws.String(teamTable),
			Key: map[string]types.AttributeValue{
				"team_id": &types.AttributeValueMemberS{Value: m.TeamID},
			},
		})
		if err != nil || len(tr.Item) == 0 {
			continue
		}
		var t teamRecord
		if err := attributevalue.UnmarshalMap(tr.Item, &t); err != nil {
			continue
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n", t.TeamID, t.TeamName, m.Role, t.MemberCount, t.CreatedAt)
	}
	_ = w.Flush()
	return nil
}

func runTeamShow(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	teamID := args[0]
	ddb, callerARN, err := teamDDBClient(ctx)
	if err != nil {
		return err
	}

	// Verify membership
	if _, err := resolveTeamMembership(ctx, ddb, teamID, callerARN); err != nil {
		return fmt.Errorf("access denied: %w", err)
	}

	tr, err := ddb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(teamTable),
		Key: map[string]types.AttributeValue{
			"team_id": &types.AttributeValueMemberS{Value: teamID},
		},
	})
	if err != nil || len(tr.Item) == 0 {
		return fmt.Errorf("team not found")
	}
	var t teamRecord
	if err := attributevalue.UnmarshalMap(tr.Item, &t); err != nil {
		return fmt.Errorf("unmarshal team: %w", err)
	}

	fmt.Printf("Team:        %s\n", t.TeamName)
	fmt.Printf("ID:          %s\n", t.TeamID)
	fmt.Printf("Owner:       %s\n", t.OwnerARN)
	fmt.Printf("Description: %s\n", t.Description)
	fmt.Printf("Created:     %s\n", t.CreatedAt)
	fmt.Printf("Members:     %d\n\n", t.MemberCount)

	membersResult, err := ddb.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(membershipTable),
		KeyConditionExpression: aws.String("team_id = :tid"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":tid": &types.AttributeValueMemberS{Value: teamID},
		},
	})
	if err != nil {
		return fmt.Errorf("query members: %w", err)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "MEMBER ARN\tROLE\tJOINED")
	for _, item := range membersResult.Items {
		var m memberRecord
		if err := attributevalue.UnmarshalMap(item, &m); err != nil {
			continue
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", m.MemberARN, m.Role, m.JoinedAt)
	}
	_ = w.Flush()
	return nil
}

func runTeamAdd(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	teamID, memberARN := args[0], args[1]
	ddb, callerARN, err := teamDDBClient(ctx)
	if err != nil {
		return err
	}

	role, err := resolveTeamMembership(ctx, ddb, teamID, callerARN)
	if err != nil || role != "owner" {
		return fmt.Errorf("only team owners can add members")
	}

	now := time.Now().UTC().Format(time.RFC3339)
	m := memberRecord{
		TeamID:    teamID,
		MemberARN: memberARN,
		Role:      "member",
		JoinedAt:  now,
		InvitedBy: callerARN,
	}
	item, err := attributevalue.MarshalMap(m)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if _, err := ddb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(membershipTable),
		Item:      item,
	}); err != nil {
		return fmt.Errorf("add member: %w", err)
	}

	// Increment member_count
	ddb.UpdateItem(ctx, &dynamodb.UpdateItemInput{ //nolint:errcheck
		TableName: aws.String(teamTable),
		Key: map[string]types.AttributeValue{
			"team_id": &types.AttributeValueMemberS{Value: teamID},
		},
		UpdateExpression: aws.String("SET member_count = member_count + :one"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":one": &types.AttributeValueMemberN{Value: "1"},
		},
	})

	fmt.Printf("Added %s to team %s\n", memberARN, teamID)
	return nil
}

func runTeamRemove(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	teamID, memberARN := args[0], args[1]
	ddb, callerARN, err := teamDDBClient(ctx)
	if err != nil {
		return err
	}

	role, err := resolveTeamMembership(ctx, ddb, teamID, callerARN)
	if err != nil || role != "owner" {
		return fmt.Errorf("only team owners can remove members")
	}
	if memberARN == callerARN {
		return fmt.Errorf("cannot remove yourself as owner")
	}

	if _, err := ddb.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(membershipTable),
		Key: map[string]types.AttributeValue{
			"team_id":    &types.AttributeValueMemberS{Value: teamID},
			"member_arn": &types.AttributeValueMemberS{Value: memberARN},
		},
	}); err != nil {
		return fmt.Errorf("remove member: %w", err)
	}

	ddb.UpdateItem(ctx, &dynamodb.UpdateItemInput{ //nolint:errcheck
		TableName: aws.String(teamTable),
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

	fmt.Printf("Removed %s from team %s\n", memberARN, teamID)
	return nil
}

func runTeamDelete(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	teamID := args[0]
	ddb, callerARN, err := teamDDBClient(ctx)
	if err != nil {
		return err
	}

	role, err := resolveTeamMembership(ctx, ddb, teamID, callerARN)
	if err != nil || role != "owner" {
		return fmt.Errorf("only team owners can delete teams")
	}

	// Delete all memberships
	membersResult, err := ddb.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(membershipTable),
		KeyConditionExpression: aws.String("team_id = :tid"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":tid": &types.AttributeValueMemberS{Value: teamID},
		},
		ProjectionExpression: aws.String("team_id, member_arn"),
	})
	if err != nil {
		return fmt.Errorf("query members: %w", err)
	}

	for _, item := range membersResult.Items {
		var m memberRecord
		if err := attributevalue.UnmarshalMap(item, &m); err != nil {
			continue
		}
		ddb.DeleteItem(ctx, &dynamodb.DeleteItemInput{ //nolint:errcheck
			TableName: aws.String(membershipTable),
			Key: map[string]types.AttributeValue{
				"team_id":    &types.AttributeValueMemberS{Value: teamID},
				"member_arn": &types.AttributeValueMemberS{Value: m.MemberARN},
			},
		})
	}

	if _, err := ddb.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(teamTable),
		Key: map[string]types.AttributeValue{
			"team_id": &types.AttributeValueMemberS{Value: teamID},
		},
	}); err != nil {
		return fmt.Errorf("delete team: %w", err)
	}

	fmt.Printf("Deleted team %s\n", teamID)
	return nil
}

// resolveTeamName looks up the team name by ID. Returns empty string on failure.
func resolveTeamName(ctx context.Context, teamID string) (string, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return "", err
	}
	ddb := dynamodb.NewFromConfig(cfg)
	result, err := ddb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(teamTable),
		Key: map[string]types.AttributeValue{
			"team_id": &types.AttributeValueMemberS{Value: teamID},
		},
	})
	if err != nil || len(result.Item) == 0 {
		return "", nil
	}
	var t teamRecord
	if err := attributevalue.UnmarshalMap(result.Item, &t); err != nil {
		return "", nil
	}
	return t.TeamName, nil
}

// resolveTeamMembership returns the caller's role or error if not a member.
func resolveTeamMembership(ctx context.Context, ddb *dynamodb.Client, teamID, callerARN string) (string, error) {
	result, err := ddb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(membershipTable),
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
	var m memberRecord
	if err := attributevalue.UnmarshalMap(result.Item, &m); err != nil {
		return "", fmt.Errorf("unmarshal: %w", err)
	}
	return m.Role, nil
}
