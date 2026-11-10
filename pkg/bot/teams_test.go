package bot

import (
	"context"
	"errors"
	"testing"

	"github.com/spore-host/spawn/pkg/botsecret"
)

// A Teams workspace takes no signing secret: spore-bot verifies Teams requests
// with Bot Framework tokens only, and the value once documented for the field
// (the Azure Bot App ID) is public. PutWorkspace refuses one before anything
// is encrypted or written, and stores a Teams workspace without one.

func TestPutWorkspace_RefusesTeamsSigningSecret(t *testing.T) {
	c, _, counter := newCountingKMSTestClient(t)
	ctx := context.Background()
	for _, ws := range []*Workspace{
		{WorkspaceKey: "teams#tenant-a", Platform: "teams"},
		{WorkspaceKey: "teams#tenant-b", Platform: "Teams"},
		{WorkspaceKey: "teams#tenant-c"}, // platform unset; the key says teams
	} {
		ws.SigningSecret = "00000000-0000-0000-0000-000000000000" // an App ID's shape
		ws.BotToken = testCredentials[botsecret.BotToken]
		err := c.PutWorkspace(ctx, ws)
		if !errors.Is(err, ErrTeamsSigningSecret) {
			t.Errorf("PutWorkspace(%s) = %v, want ErrTeamsSigningSecret", ws.WorkspaceKey, err)
		}
		if got, _ := c.GetWorkspace(ctx, "teams", ws.WorkspaceKey[len("teams#"):]); got != nil {
			t.Errorf("a refused PutWorkspace(%s) left an item", ws.WorkspaceKey)
		}
	}
	if counter.encrypts != 0 {
		t.Errorf("refused writes reached KMS Encrypt %d times; want 0", counter.encrypts)
	}
}

func TestPutWorkspace_TeamsWithoutSigningSecretStored(t *testing.T) {
	c, db := newKMSTestClient(t)
	ctx := context.Background()
	ws := &Workspace{WorkspaceKey: "teams#tenant-ok", Platform: "teams", BotToken: testCredentials[botsecret.BotToken]}
	if err := c.PutWorkspace(ctx, ws); err != nil {
		t.Fatalf("PutWorkspace(teams, no signing secret) = %v", err)
	}
	item := rawItem(t, db, "teams#tenant-ok")
	if _, ok := item["signing_secret"]; ok {
		t.Errorf("a Teams item without a signing secret stored a signing_secret attribute")
	}
	got, err := c.GetWorkspace(ctx, "teams", "tenant-ok")
	if err != nil || got == nil {
		t.Fatalf("GetWorkspace = %v, %v", got, err)
	}
	if tok, err := c.Credential(ctx, got, botsecret.BotToken); err != nil || tok != testCredentials[botsecret.BotToken] {
		t.Errorf("bot token round trip = %q, %v", tok, err)
	}
}

// Slack still stores its signing secret; only Teams refuses one.
func TestPutWorkspace_SlackSigningSecretStillStored(t *testing.T) {
	c, _ := newKMSTestClient(t)
	ctx := context.Background()
	ws := &Workspace{WorkspaceKey: "slack#TOK", Platform: "slack", SigningSecret: testCredentials[botsecret.SigningSecret]}
	if err := c.PutWorkspace(ctx, ws); err != nil {
		t.Fatalf("PutWorkspace(slack) = %v", err)
	}
	got, _ := c.GetWorkspace(ctx, "slack", "TOK")
	if got == nil || !got.HasCredential(botsecret.SigningSecret) {
		t.Fatalf("slack signing secret not stored")
	}
}
