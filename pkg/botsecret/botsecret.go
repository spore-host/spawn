// Package botsecret is the only code that reads or writes the credential
// attributes of a spore-bot-workspaces item: signing_secret, bot_token,
// refresh_token and incoming_webhook_url (one Field each). Every writer seals
// the value with KMS through Field.Put (or RotatedTokens, for a token refresh);
// every reader goes through Field.Stored and Field.Open. A repo-wide source gate
// (bot_signing_secret_test.go) fails if one of those attribute names appears in
// Go code anywhere else, so a new writer cannot store a credential in the clear
// by building the item by hand (#868).
//
// Wire format. A sealed value is security.EncryptSecret's output: base64 of a
// KMS ciphertext blob, encrypted under KeyID() with the encryption context
// Field.EncryptionContext(workspace_key) = {purpose: Field.Purpose(),
// workspace_key: <item key>}. Binding the workspace key means a ciphertext
// copied into another item does not decrypt; a distinct purpose per field means
// one field's ciphertext does not open as another, and lets IAM grant each
// principal only the fields it uses with a kms:EncryptionContext:purpose
// condition. A reader in another repo (spore-bot, in spore-host) must pass the
// same context.
//
// Transition. Items written before #868 hold the plaintext. Open returns such a
// value unchanged (security.IsEncrypted reports it as not encrypted: Slack
// tokens contain '-', which is not base64, and URLs contain "://"), so they keep
// working; the next write of the item seals it. Reads deliberately do not
// re-write: see Field.Open.
package botsecret

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/spore-host/spawn/pkg/security"
)

// Field is one credential attribute of a workspace item, with the purpose its
// values are sealed for.
type Field struct {
	attribute string // DynamoDB attribute name; shared with spore-bot, must not change
	purpose   string // the "purpose" encryption-context value
	label     string // for messages
}

// The credential fields. Their attribute names and purposes must not change:
// spore-bot reads the same attributes, and IAM policies condition on the
// purposes.
var (
	// SigningSecret verifies inbound Slack/Teams requests.
	SigningSecret = Field{"signing_secret", "spore-bot-signing-secret", "signing secret"}
	// BotToken authenticates calls to the chat platform's API.
	BotToken = Field{"bot_token", "spore-bot-bot-token", "bot token"}
	// RefreshToken exchanges for a new bot token when Slack token rotation is on.
	RefreshToken = Field{"refresh_token", "spore-bot-refresh-token", "refresh token"}
	// IncomingWebhookURL posts to a channel; the URL is itself the credential.
	IncomingWebhookURL = Field{"incoming_webhook_url", "spore-bot-incoming-webhook-url", "incoming webhook URL"}
)

// Fields lists every credential field.
func Fields() []Field { return []Field{SigningSecret, BotToken, RefreshToken, IncomingWebhookURL} }

// DefaultKeyAlias names the KMS key that seals workspace credentials when
// SPORE_BOT_SECRETS_KMS_KEY is unset. deployment/cloudformation/spore-bot.yaml
// creates it.
const DefaultKeyAlias = "alias/spore-bot-secrets"

// EnvKeyID overrides DefaultKeyAlias (key id, key ARN, alias name or alias ARN).
const EnvKeyID = "SPORE_BOT_SECRETS_KMS_KEY"

// KeyID returns the KMS key used to seal workspace credentials.
func KeyID() string {
	if v := os.Getenv(EnvKeyID); v != "" {
		return v
	}
	return DefaultKeyAlias
}

// Purpose is the value of the "purpose" encryption-context entry for f. IAM
// policies granting use of the key condition on kms:EncryptionContext:purpose,
// so a grant covers only the fields it names.
func (f Field) Purpose() string { return f.purpose }

// String names the field for messages.
func (f Field) String() string { return f.label }

// EncryptionContext is the context f's value for workspaceKey is sealed and
// opened with.
func (f Field) EncryptionContext(workspaceKey string) map[string]string {
	return map[string]string{"purpose": f.purpose, "workspace_key": workspaceKey}
}

// Seal encrypts plaintext for the item keyed workspaceKey. An empty plaintext
// stays empty (the item has no such credential; for the signing secret,
// spore-bot then falls back to its app-level one). Seal never returns
// plaintext: with no KMS client it fails, and it refuses a result
// security.IsEncrypted would not recognise, because Open would then hand that
// value back as if it were the credential.
func (f Field) Seal(ctx context.Context, kmsClient security.KMSAPI, keyID, workspaceKey, plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	if kmsClient == nil {
		return "", fmt.Errorf("%s not stored: no KMS client to encrypt it with", f)
	}
	if workspaceKey == "" {
		return "", fmt.Errorf("%s not stored: empty workspace key", f)
	}
	if keyID == "" {
		keyID = KeyID()
	}
	sealed, err := security.EncryptSecret(ctx, kmsClient, keyID, plaintext, f.EncryptionContext(workspaceKey))
	if err != nil {
		return "", fmt.Errorf("%s not stored: encrypt with KMS key %s: %w (create the key with deployment/cloudformation/spore-bot.yaml, or set %s)", f, keyID, err, EnvKeyID)
	}
	if !security.IsEncrypted(sealed) {
		return "", fmt.Errorf("%s not stored: KMS returned a value that does not read as ciphertext", f)
	}
	return sealed, nil
}

// Put seals plaintext and sets it as item's f attribute, replacing whatever the
// item held. An empty plaintext removes the attribute, so an absent credential
// is absent rather than an empty string.
func (f Field) Put(ctx context.Context, kmsClient security.KMSAPI, keyID string, item map[string]types.AttributeValue, workspaceKey, plaintext string) error {
	sealed, err := f.Seal(ctx, kmsClient, keyID, workspaceKey, plaintext)
	if err != nil {
		return err
	}
	if sealed == "" {
		delete(item, f.attribute)
		return nil
	}
	item[f.attribute] = &types.AttributeValueMemberS{Value: sealed}
	return nil
}

// Stored returns item's f attribute exactly as stored: a sealed value, a legacy
// plaintext, or "" when absent.
func (f Field) Stored(item map[string]types.AttributeValue) string {
	if v, ok := item[f.attribute].(*types.AttributeValueMemberS); ok {
		return v.Value
	}
	return ""
}

// Open returns the plaintext of a stored f value. A sealed value is decrypted
// with workspaceKey's encryption context; a legacy plaintext value is returned
// as is, without a KMS call, so items written before encryption keep working.
//
// Open does not re-write a legacy item encrypted. Readers (spore-bot verifying
// a request, the CLI listing workspaces) would each need kms:Encrypt and a
// DynamoDB write they otherwise lack, and a read-triggered full-item write
// races the writers that rotate tokens. Legacy values instead get sealed on the
// item's next write (for a rotating Slack app, its next token refresh), and the
// remaining live ones are re-keyed by the maintainer.
func (f Field) Open(ctx context.Context, kmsClient security.KMSAPI, workspaceKey, stored string) (string, error) {
	if !security.IsEncrypted(stored) {
		return stored, nil
	}
	if kmsClient == nil {
		return "", fmt.Errorf("%s is encrypted and no KMS client was given to decrypt it", f)
	}
	plain, err := security.DecryptSecret(ctx, kmsClient, stored, f.EncryptionContext(workspaceKey))
	if err != nil {
		return "", fmt.Errorf("decrypt %s for %s: %w", f, workspaceKey, err)
	}
	return plain, nil
}

// RotatedTokens returns a DynamoDB UpdateExpression and its values that store
// a refreshed bot token and refresh token, both sealed, with the new expiry
// (Unix seconds). It is the write-back of every token refresh, and so what
// moves a rotating app's legacy plaintext tokens to sealed ones.
func RotatedTokens(ctx context.Context, kmsClient security.KMSAPI, keyID, workspaceKey, botToken, refreshToken string, expiresAt int64) (string, map[string]types.AttributeValue, error) {
	if botToken == "" {
		return "", nil, errors.New("rotated tokens not stored: empty bot token")
	}
	t, err := BotToken.Seal(ctx, kmsClient, keyID, workspaceKey, botToken)
	if err != nil {
		return "", nil, err
	}
	r, err := RefreshToken.Seal(ctx, kmsClient, keyID, workspaceKey, refreshToken)
	if err != nil {
		return "", nil, err
	}
	values := map[string]types.AttributeValue{
		":t": &types.AttributeValueMemberS{Value: t},
		":e": &types.AttributeValueMemberN{Value: strconv.FormatInt(expiresAt, 10)},
	}
	if r == "" {
		// The exchange returned no refresh token: store none (the code this
		// replaces stored an empty string).
		return "SET " + BotToken.attribute + " = :t, token_expires_at = :e REMOVE " + RefreshToken.attribute, values, nil
	}
	values[":r"] = &types.AttributeValueMemberS{Value: r}
	return "SET " + BotToken.attribute + " = :t, " + RefreshToken.attribute + " = :r, token_expires_at = :e", values, nil
}
