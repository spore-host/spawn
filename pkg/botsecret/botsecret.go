// Package botsecret is the only code that reads or writes the signing_secret
// attribute of a spore-bot-workspaces item. Every writer seals the value with
// KMS through Put; every reader goes through Stored and Open. A repo-wide
// source gate (bot_signing_secret_test.go) fails if the attribute name appears
// in Go code anywhere else, so a new writer cannot store the value in the clear
// by building the item by hand (#868).
//
// Wire format. A sealed value is security.EncryptSecret's output: base64 of a
// KMS ciphertext blob, encrypted under KeyID() with the encryption context
// EncryptionContext(workspace_key). Binding the workspace key means a
// ciphertext copied into another item does not decrypt, and lets IAM scope the
// key to this one use with a kms:EncryptionContext:purpose condition. A reader
// in another repo (spore-bot, in spore-host) must pass the same context.
//
// Transition. Items written before #868 hold the plaintext. Open returns such a
// value unchanged (security.IsEncrypted reports it as not encrypted), so they
// keep working; the next write of the item seals it. Reads deliberately do not
// re-write: see Open.
package botsecret

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/spore-host/spawn/pkg/security"
)

// attribute is the DynamoDB attribute name. It must not change: it is shared
// with spore-bot, which reads it to verify inbound requests.
const attribute = "signing_secret"

// DefaultKeyAlias names the KMS key that seals signing secrets when
// SPORE_BOT_SECRETS_KMS_KEY is unset. deployment/cloudformation/spore-bot.yaml
// creates it.
const DefaultKeyAlias = "alias/spore-bot-secrets"

// EnvKeyID overrides DefaultKeyAlias (key id, key ARN, alias name or alias ARN).
const EnvKeyID = "SPORE_BOT_SECRETS_KMS_KEY"

// Purpose is the value of the "purpose" encryption-context entry. IAM policies
// granting use of the key condition on kms:EncryptionContext:purpose equal to
// it, so the grant cannot be used to encrypt or decrypt anything else.
const Purpose = "spore-bot-signing-secret"

// KeyID returns the KMS key used to seal signing secrets.
func KeyID() string {
	if v := os.Getenv(EnvKeyID); v != "" {
		return v
	}
	return DefaultKeyAlias
}

// EncryptionContext is the context a signing secret for workspaceKey is sealed
// and opened with.
func EncryptionContext(workspaceKey string) map[string]string {
	return map[string]string{"purpose": Purpose, "workspace_key": workspaceKey}
}

// Seal encrypts plaintext for the item keyed workspaceKey. An empty plaintext
// stays empty (the item has no per-workspace secret and spore-bot falls back to
// its app-level one). Seal never returns plaintext: with no KMS client it
// fails, and it refuses a result security.IsEncrypted would not recognise,
// because Open would then hand that value back as if it were the secret.
func Seal(ctx context.Context, kmsClient security.KMSAPI, keyID, workspaceKey, plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	if kmsClient == nil {
		return "", errors.New("signing secret not stored: no KMS client to encrypt it with")
	}
	if workspaceKey == "" {
		return "", errors.New("signing secret not stored: empty workspace key")
	}
	if keyID == "" {
		keyID = KeyID()
	}
	sealed, err := security.EncryptSecret(ctx, kmsClient, keyID, plaintext, EncryptionContext(workspaceKey))
	if err != nil {
		return "", fmt.Errorf("signing secret not stored: encrypt with KMS key %s: %w (create the key with deployment/cloudformation/spore-bot.yaml, or set %s)", keyID, err, EnvKeyID)
	}
	if !security.IsEncrypted(sealed) {
		return "", errors.New("signing secret not stored: KMS returned a value that does not read as ciphertext")
	}
	return sealed, nil
}

// Put seals plaintext and sets it as item's signing_secret attribute,
// replacing whatever the item held.
func Put(ctx context.Context, kmsClient security.KMSAPI, keyID string, item map[string]types.AttributeValue, workspaceKey, plaintext string) error {
	sealed, err := Seal(ctx, kmsClient, keyID, workspaceKey, plaintext)
	if err != nil {
		return err
	}
	item[attribute] = &types.AttributeValueMemberS{Value: sealed}
	return nil
}

// Stored returns item's signing_secret exactly as stored: a sealed value, a
// legacy plaintext, or "" when absent.
func Stored(item map[string]types.AttributeValue) string {
	if v, ok := item[attribute].(*types.AttributeValueMemberS); ok {
		return v.Value
	}
	return ""
}

// Open returns the plaintext of a stored value. A sealed value is decrypted
// with workspaceKey's encryption context; a legacy plaintext value is returned
// as is, so items written before encryption keep working.
//
// Open does not re-write a legacy item encrypted. Readers (spore-bot verifying
// a request, the CLI listing workspaces) would each need kms:Encrypt and a
// DynamoDB write they otherwise lack, and a read-triggered full-item write
// races the writers that rotate tokens. Legacy values instead get sealed on the
// item's next write, and the remaining live ones are re-keyed by the maintainer
// anyway, since encrypting a value already exposed does not un-expose it.
func Open(ctx context.Context, kmsClient security.KMSAPI, workspaceKey, stored string) (string, error) {
	if !security.IsEncrypted(stored) {
		return stored, nil
	}
	if kmsClient == nil {
		return "", errors.New("signing secret is encrypted and no KMS client was given to decrypt it")
	}
	plain, err := security.DecryptSecret(ctx, kmsClient, stored, EncryptionContext(workspaceKey))
	if err != nil {
		return "", fmt.Errorf("decrypt signing secret for %s: %w", workspaceKey, err)
	}
	return plain, nil
}
