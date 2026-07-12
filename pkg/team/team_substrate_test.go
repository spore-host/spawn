package team

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/spore-host/spawn/pkg/testutil"
)

const (
	testTeamsTable       = "spawn-teams"
	testMembershipsTable = "spawn-team-memberships"
)

func setupTeamTables(t *testing.T, db *dynamodb.Client) {
	t.Helper()
	ctx := context.Background()

	_, err := db.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:   aws.String(testTeamsTable),
		BillingMode: dynamodbtypes.BillingModePayPerRequest,
		AttributeDefinitions: []dynamodbtypes.AttributeDefinition{
			{AttributeName: aws.String("team_id"), AttributeType: dynamodbtypes.ScalarAttributeTypeS},
		},
		KeySchema: []dynamodbtypes.KeySchemaElement{
			{AttributeName: aws.String("team_id"), KeyType: dynamodbtypes.KeyTypeHash},
		},
	})
	tolerateInUse(t, err, testTeamsTable)

	// memberships: hash team_id + range member_arn, GSI member_arn-index.
	_, err = db.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:   aws.String(testMembershipsTable),
		BillingMode: dynamodbtypes.BillingModePayPerRequest,
		AttributeDefinitions: []dynamodbtypes.AttributeDefinition{
			{AttributeName: aws.String("team_id"), AttributeType: dynamodbtypes.ScalarAttributeTypeS},
			{AttributeName: aws.String("member_arn"), AttributeType: dynamodbtypes.ScalarAttributeTypeS},
		},
		KeySchema: []dynamodbtypes.KeySchemaElement{
			{AttributeName: aws.String("team_id"), KeyType: dynamodbtypes.KeyTypeHash},
			{AttributeName: aws.String("member_arn"), KeyType: dynamodbtypes.KeyTypeRange},
		},
		GlobalSecondaryIndexes: []dynamodbtypes.GlobalSecondaryIndex{
			{
				IndexName: aws.String(memberARNIndex),
				KeySchema: []dynamodbtypes.KeySchemaElement{
					{AttributeName: aws.String("member_arn"), KeyType: dynamodbtypes.KeyTypeHash},
				},
				Projection: &dynamodbtypes.Projection{ProjectionType: dynamodbtypes.ProjectionTypeAll},
			},
		},
	})
	tolerateInUse(t, err, testMembershipsTable)
}

func tolerateInUse(t *testing.T, err error, table string) {
	t.Helper()
	if err != nil {
		var inUse *dynamodbtypes.ResourceInUseException
		if !errors.As(err, &inUse) {
			t.Fatalf("create %s: %v", table, err)
		}
	}
}

func newTestClient(t *testing.T) *Client {
	t.Helper()
	env := testutil.SubstrateServer(t)
	db := env.DynamoClient()
	setupTeamTables(t, db)
	return NewClientWithTableNames(db, testTeamsTable, testMembershipsTable)
}

func TestTeamStore_CreateListShowFlow(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()
	owner := "arn:aws:iam::111122223333:user/owner"

	if err := c.PutTeam(ctx, &TeamRecord{
		TeamID: "t1", TeamName: "alpha", OwnerARN: owner,
		CreatedAt: "2026-07-11T00:00:00Z", MemberCount: 1,
	}); err != nil {
		t.Fatalf("PutTeam: %v", err)
	}
	if err := c.PutMembership(ctx, &MemberRecord{
		TeamID: "t1", MemberARN: owner, Role: "owner", JoinedAt: "2026-07-11T00:00:00Z", InvitedBy: owner,
	}); err != nil {
		t.Fatalf("PutMembership: %v", err)
	}

	// GetTeam round-trip + missing→nil.
	got, err := c.GetTeam(ctx, "t1")
	if err != nil || got == nil {
		t.Fatalf("GetTeam: %v (%v)", got, err)
	}
	if got.TeamName != "alpha" || got.MemberCount != 1 {
		t.Errorf("team round-trip mismatch: %+v", got)
	}
	if missing, _ := c.GetTeam(ctx, "nope"); missing != nil {
		t.Errorf("expected nil for missing team, got %+v", missing)
	}

	// GetMembership: present + absent.
	m, err := c.GetMembership(ctx, "t1", owner)
	if err != nil || m == nil || m.Role != "owner" {
		t.Fatalf("GetMembership(owner): %+v %v", m, err)
	}
	if absent, _ := c.GetMembership(ctx, "t1", "arn:aws:iam::x:user/nobody"); absent != nil {
		t.Errorf("expected nil for non-member, got %+v", absent)
	}

	// ListTeamsForMember via GSI.
	mine, err := c.ListTeamsForMember(ctx, owner)
	if err != nil {
		t.Fatalf("ListTeamsForMember: %v", err)
	}
	if len(mine) != 1 || mine[0].TeamID != "t1" {
		t.Errorf("ListTeamsForMember = %+v", mine)
	}
}

func TestTeamStore_AddRemoveCounts(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	if err := c.PutTeam(ctx, &TeamRecord{TeamID: "t2", TeamName: "beta", MemberCount: 1}); err != nil {
		t.Fatalf("PutTeam: %v", err)
	}
	if err := c.PutMembership(ctx, &MemberRecord{TeamID: "t2", MemberARN: "m2", Role: "member"}); err != nil {
		t.Fatalf("PutMembership: %v", err)
	}
	if err := c.IncrementMemberCount(ctx, "t2"); err != nil {
		t.Fatalf("Increment: %v", err)
	}
	if got, _ := c.GetTeam(ctx, "t2"); got.MemberCount != 2 {
		t.Errorf("after increment MemberCount=%d, want 2", got.MemberCount)
	}

	members, err := c.ListMembers(ctx, "t2")
	if err != nil || len(members) != 1 {
		t.Fatalf("ListMembers = %+v (%v)", members, err)
	}

	if err := c.DeleteMembership(ctx, "t2", "m2"); err != nil {
		t.Fatalf("DeleteMembership: %v", err)
	}
	if err := c.DecrementMemberCount(ctx, "t2"); err != nil {
		t.Fatalf("Decrement: %v", err)
	}
	if got, _ := c.GetTeam(ctx, "t2"); got.MemberCount != 1 {
		t.Errorf("after decrement MemberCount=%d, want 1", got.MemberCount)
	}

	if err := c.DeleteTeam(ctx, "t2"); err != nil {
		t.Fatalf("DeleteTeam: %v", err)
	}
	if got, _ := c.GetTeam(ctx, "t2"); got != nil {
		t.Errorf("team still present after delete: %+v", got)
	}
}
