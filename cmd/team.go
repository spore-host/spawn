package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/spf13/cobra"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/team"
)

var (
	teamName        string
	teamDescription string
	teamDeleteYes   bool
	teamRemoveYes   bool
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

	teamDeleteCmd.Flags().BoolVarP(&teamDeleteYes, "yes", "y", false, "Skip the confirmation prompt")
	teamRemoveCmd.Flags().BoolVarP(&teamRemoveYes, "yes", "y", false, "Skip the confirmation prompt")
}

// teamStore returns a team store Client (default account/config) plus the
// caller's IAM ARN, used for ownership/membership checks.
func teamStore(ctx context.Context) (*team.Client, string, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("load AWS config: %w", err)
	}
	// Get caller identity ARN via STS
	callerARN, err := getCallerARN(ctx, cfg)
	if err != nil {
		return nil, "", fmt.Errorf("get caller identity: %w", err)
	}
	return team.NewClient(dynamodb.NewFromConfig(cfg)), callerARN, nil
}

// getCallerARN returns the caller's IAM ARN via STS GetCallerIdentity.
func getCallerARN(ctx context.Context, cfg aws.Config) (string, error) {
	_, arn, err := spawnaws.NewClientFromConfig(cfg).GetCallerIdentityInfo(ctx)
	if err != nil {
		return "", fmt.Errorf("get caller identity: %w", err)
	}
	return arn, nil
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
	store, callerARN, err := teamStore(ctx)
	if err != nil {
		return err
	}

	teamID := genTeamID()
	now := time.Now().UTC().Format(time.RFC3339)

	tr := &team.TeamRecord{
		TeamID:      teamID,
		TeamName:    teamName,
		OwnerARN:    callerARN,
		Description: teamDescription,
		CreatedAt:   now,
		MemberCount: 1,
	}
	if err := store.PutTeam(ctx, tr); err != nil {
		return fmt.Errorf("create team: %w", err)
	}

	if err := store.PutMembership(ctx, &team.MemberRecord{
		TeamID:    teamID,
		MemberARN: callerARN,
		Role:      "owner",
		JoinedAt:  now,
		InvitedBy: callerARN,
	}); err != nil {
		return fmt.Errorf("create membership: %w", err)
	}

	_, _ = fmt.Fprintf(os.Stdout, "Created team %q\n  ID:    %s\n  Owner: %s\n", tr.TeamName, teamID, callerARN)
	return nil
}

func runTeamList(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	store, callerARN, err := teamStore(ctx)
	if err != nil {
		return err
	}

	memberships, err := store.ListTeamsForMember(ctx, callerARN)
	if err != nil {
		return err
	}

	if len(memberships) == 0 {
		fmt.Println("No teams found.")
		return nil
	}

	w := newTableWriter(os.Stdout)
	_, _ = fmt.Fprintln(w, "TEAM ID\tNAME\tROLE\tMEMBERS\tCREATED")
	for _, m := range memberships {
		t, err := store.GetTeam(ctx, m.TeamID)
		if err != nil || t == nil {
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
	store, callerARN, err := teamStore(ctx)
	if err != nil {
		return err
	}

	// Verify membership
	if _, err := requireMembership(ctx, store, teamID, callerARN); err != nil {
		return fmt.Errorf("access denied: %w", err)
	}

	t, err := store.GetTeam(ctx, teamID)
	if err != nil {
		return fmt.Errorf("unmarshal team: %w", err)
	}
	if t == nil {
		return fmt.Errorf("team not found")
	}

	fmt.Printf("Team:        %s\n", t.TeamName)
	fmt.Printf("ID:          %s\n", t.TeamID)
	fmt.Printf("Owner:       %s\n", t.OwnerARN)
	fmt.Printf("Description: %s\n", t.Description)
	fmt.Printf("Created:     %s\n", t.CreatedAt)
	fmt.Printf("Members:     %d\n\n", t.MemberCount)

	members, err := store.ListMembers(ctx, teamID)
	if err != nil {
		return fmt.Errorf("query members: %w", err)
	}

	w := newTableWriter(os.Stdout)
	_, _ = fmt.Fprintln(w, "MEMBER ARN\tROLE\tJOINED")
	for _, m := range members {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", m.MemberARN, m.Role, m.JoinedAt)
	}
	_ = w.Flush()
	return nil
}

func runTeamAdd(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	teamID, memberARN := args[0], args[1]
	store, callerARN, err := teamStore(ctx)
	if err != nil {
		return err
	}

	role, err := requireMembership(ctx, store, teamID, callerARN)
	if err != nil || role != "owner" {
		return fmt.Errorf("only team owners can add members")
	}

	now := time.Now().UTC().Format(time.RFC3339)
	if err := store.PutMembership(ctx, &team.MemberRecord{
		TeamID:    teamID,
		MemberARN: memberARN,
		Role:      "member",
		JoinedAt:  now,
		InvitedBy: callerARN,
	}); err != nil {
		return fmt.Errorf("add member: %w", err)
	}

	_ = store.IncrementMemberCount(ctx, teamID)

	fmt.Printf("Added %s to team %s\n", memberARN, teamID)
	return nil
}

func runTeamRemove(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	teamID, memberARN := args[0], args[1]
	store, callerARN, err := teamStore(ctx)
	if err != nil {
		return err
	}

	role, err := requireMembership(ctx, store, teamID, callerARN)
	if err != nil || role != "owner" {
		return fmt.Errorf("only team owners can remove members")
	}
	if memberARN == callerARN {
		return fmt.Errorf("cannot remove yourself as owner")
	}

	if !confirmYes(teamRemoveYes, fmt.Sprintf("Remove %s from team %s?", memberARN, teamID)) {
		fmt.Println("Aborted.")
		return nil
	}

	if err := store.DeleteMembership(ctx, teamID, memberARN); err != nil {
		return fmt.Errorf("remove member: %w", err)
	}

	_ = store.DecrementMemberCount(ctx, teamID)

	fmt.Printf("Removed %s from team %s\n", memberARN, teamID)
	return nil
}

func runTeamDelete(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	teamID := args[0]
	store, callerARN, err := teamStore(ctx)
	if err != nil {
		return err
	}

	role, err := requireMembership(ctx, store, teamID, callerARN)
	if err != nil || role != "owner" {
		return fmt.Errorf("only team owners can delete teams")
	}

	if !confirmYes(teamDeleteYes, fmt.Sprintf("Delete team %s and all its memberships? This cannot be undone.", teamID)) {
		fmt.Println("Aborted.")
		return nil
	}

	// Delete all memberships, then the team itself.
	members, err := store.ListMembers(ctx, teamID)
	if err != nil {
		return fmt.Errorf("query members: %w", err)
	}
	for _, m := range members {
		_ = store.DeleteMembership(ctx, teamID, m.MemberARN)
	}

	if err := store.DeleteTeam(ctx, teamID); err != nil {
		return fmt.Errorf("delete team: %w", err)
	}

	fmt.Printf("Deleted team %s\n", teamID)
	return nil
}

// resolveTeamName looks up the team name by ID. Returns empty string on failure.
// Used by the launch path to tag instances with a human-readable team name.
func resolveTeamName(ctx context.Context, teamID string) (string, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return "", err
	}
	t, err := team.NewClient(dynamodb.NewFromConfig(cfg)).GetTeam(ctx, teamID)
	if err != nil || t == nil {
		return "", nil
	}
	return t.TeamName, nil
}

// requireMembership returns the caller's role in a team, or an error if they are
// not a member (used for ownership/access checks).
func requireMembership(ctx context.Context, store *team.Client, teamID, callerARN string) (string, error) {
	m, err := store.GetMembership(ctx, teamID, callerARN)
	if err != nil {
		return "", err
	}
	if m == nil {
		return "", fmt.Errorf("not a member of team %s", teamID)
	}
	return m.Role, nil
}
