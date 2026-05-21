package autoscaler

import (
	"context"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/testutil"
)

const testTableName = "spawn-autoscale-groups"

func TestCreateAndGetGroup(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()

	client := NewClient(env.DynamoClient(), testTableName)
	if err := client.EnsureTable(ctx); err != nil {
		t.Fatalf("EnsureTable: %v", err)
	}

	group := &AutoScaleGroup{
		AutoScaleGroupID: "asg-001",
		GroupName:        "test-group",
		JobArrayID:       "job-123",
		DesiredCapacity:  5,
		MinCapacity:      1,
		MaxCapacity:      20,
		Status:           "active",
		LaunchTemplate: LaunchTemplate{
			InstanceType: "t3.micro",
			AMI:          "ami-12345678",
		},
	}

	if err := client.CreateGroup(ctx, group); err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}

	got, err := client.GetGroup(ctx, "asg-001")
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}

	if got.GroupName != group.GroupName {
		t.Errorf("GroupName = %q, want %q", got.GroupName, group.GroupName)
	}
	if got.DesiredCapacity != group.DesiredCapacity {
		t.Errorf("DesiredCapacity = %d, want %d", got.DesiredCapacity, group.DesiredCapacity)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt should be set by CreateGroup")
	}
}

func TestGetGroup_NotFound(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()

	client := NewClient(env.DynamoClient(), testTableName)
	if err := client.EnsureTable(ctx); err != nil {
		t.Fatalf("EnsureTable: %v", err)
	}

	if _, err := client.GetGroup(ctx, "asg-missing"); err == nil {
		t.Error("GetGroup on missing ID should return error")
	}
}

func TestUpdateGroup(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()

	client := NewClient(env.DynamoClient(), testTableName)
	if err := client.EnsureTable(ctx); err != nil {
		t.Fatalf("EnsureTable: %v", err)
	}

	group := &AutoScaleGroup{
		AutoScaleGroupID: "asg-upd-001",
		GroupName:        "update-group",
		DesiredCapacity:  3,
		MinCapacity:      1,
		MaxCapacity:      10,
		Status:           "active",
	}
	if err := client.CreateGroup(ctx, group); err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}

	group.DesiredCapacity = 7
	group.Status = "paused"
	if err := client.UpdateGroup(ctx, group); err != nil {
		t.Fatalf("UpdateGroup: %v", err)
	}

	got, err := client.GetGroup(ctx, "asg-upd-001")
	if err != nil {
		t.Fatalf("GetGroup after update: %v", err)
	}
	if got.DesiredCapacity != 7 {
		t.Errorf("DesiredCapacity = %d, want 7", got.DesiredCapacity)
	}
	if got.Status != "paused" {
		t.Errorf("Status = %q, want paused", got.Status)
	}
}

func TestDeleteGroup(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()

	client := NewClient(env.DynamoClient(), testTableName)
	if err := client.EnsureTable(ctx); err != nil {
		t.Fatalf("EnsureTable: %v", err)
	}

	group := &AutoScaleGroup{
		AutoScaleGroupID: "asg-del-001",
		GroupName:        "delete-group",
		Status:           "active",
	}
	if err := client.CreateGroup(ctx, group); err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}

	if err := client.DeleteGroup(ctx, "asg-del-001"); err != nil {
		t.Fatalf("DeleteGroup: %v", err)
	}

	if _, err := client.GetGroup(ctx, "asg-del-001"); err == nil {
		t.Error("GetGroup after delete should return error")
	}
}

func TestListActiveGroups(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()

	client := NewClient(env.DynamoClient(), testTableName)
	if err := client.EnsureTable(ctx); err != nil {
		t.Fatalf("EnsureTable: %v", err)
	}

	groups := []*AutoScaleGroup{
		{AutoScaleGroupID: "asg-act-001", GroupName: "active-1", Status: "active", CreatedAt: time.Now(), UpdatedAt: time.Now()},
		{AutoScaleGroupID: "asg-act-002", GroupName: "active-2", Status: "active", CreatedAt: time.Now(), UpdatedAt: time.Now()},
		{AutoScaleGroupID: "asg-act-003", GroupName: "paused-1", Status: "paused", CreatedAt: time.Now(), UpdatedAt: time.Now()},
	}
	for _, g := range groups {
		if err := client.CreateGroup(ctx, g); err != nil {
			t.Fatalf("CreateGroup %s: %v", g.AutoScaleGroupID, err)
		}
	}

	active, err := client.ListActiveGroups(ctx)
	if err != nil {
		t.Fatalf("ListActiveGroups: %v", err)
	}

	if len(active) != 2 {
		t.Errorf("got %d active groups, want 2", len(active))
	}
	for _, g := range active {
		if g.Status != "active" {
			t.Errorf("group %s has status %q, want active", g.AutoScaleGroupID, g.Status)
		}
	}
}
