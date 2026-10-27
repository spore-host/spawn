package security

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
)

// KMSAPI is the subset of the KMS client EncryptSecret and DecryptSecret use.
// *kms.Client satisfies it; tests substitute a recording fake.
type KMSAPI interface {
	Encrypt(ctx context.Context, in *kms.EncryptInput, optFns ...func(*kms.Options)) (*kms.EncryptOutput, error)
	Decrypt(ctx context.Context, in *kms.DecryptInput, optFns ...func(*kms.Options)) (*kms.DecryptOutput, error)
}

// EncryptSecret encrypts a plaintext secret using AWS KMS.
// Returns a base64-encoded encrypted ciphertext.
//
// encryptionContext (may be nil) is bound to the ciphertext: DecryptSecret must
// be given the same map or KMS refuses to decrypt. Use it to tie a ciphertext to
// the record it belongs to, so it cannot be copied into another record, and so
// IAM can scope kms:Encrypt/kms:Decrypt with a kms:EncryptionContext condition.
func EncryptSecret(ctx context.Context, kmsClient KMSAPI, keyID, plaintext string, encryptionContext map[string]string) (string, error) {
	if plaintext == "" {
		return "", errors.New("plaintext cannot be empty")
	}

	if keyID == "" {
		return "", errors.New("KMS key ID cannot be empty")
	}

	if kmsClient == nil {
		return "", errors.New("KMS client cannot be nil")
	}

	result, err := kmsClient.Encrypt(ctx, &kms.EncryptInput{
		KeyId:             aws.String(keyID),
		Plaintext:         []byte(plaintext),
		EncryptionContext: encryptionContext,
	})
	if err != nil {
		return "", err
	}

	return base64.StdEncoding.EncodeToString(result.CiphertextBlob), nil
}

// DecryptSecret decrypts a base64-encoded KMS-encrypted ciphertext.
// Returns the plaintext secret. encryptionContext must equal the map the value
// was encrypted with (nil if none was used).
func DecryptSecret(ctx context.Context, kmsClient KMSAPI, ciphertext string, encryptionContext map[string]string) (string, error) {
	if ciphertext == "" {
		return "", errors.New("ciphertext cannot be empty")
	}

	// Decode from base64
	decoded, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", err
	}

	if kmsClient == nil {
		return "", errors.New("KMS client cannot be nil")
	}

	result, err := kmsClient.Decrypt(ctx, &kms.DecryptInput{
		CiphertextBlob:    decoded,
		EncryptionContext: encryptionContext,
	})
	if err != nil {
		return "", err
	}

	return string(result.Plaintext), nil
}

// MaskSecret masks a secret for display purposes.
// Shows the first 4 and last 4 characters, masking the middle.
// For short secrets (<=8 chars), returns all asterisks.
//
// No production caller, kept on purpose (#868): the one place a stored secret
// is displayed, `spawn notify workspace list`, replaces it wholesale with
// "(redacted)", which leaks less than a partial mask. Reach for this only where
// a recognisable fragment is genuinely needed.
func MaskSecret(secret string) string {
	if secret == "" {
		return ""
	}

	if len(secret) <= 8 {
		return "****"
	}

	return secret[:4] + "****" + secret[len(secret)-4:]
}

// IsEncrypted checks if a string appears to be KMS-encrypted.
// KMS-encrypted values are base64-encoded and typically longer than plaintext.
func IsEncrypted(value string) bool {
	// Empty strings are not encrypted
	if value == "" {
		return false
	}

	// Encrypted values should be valid base64
	if _, err := base64.StdEncoding.DecodeString(value); err != nil {
		return false
	}

	// Encrypted values are typically longer (>100 chars for short plaintext)
	// and don't contain URL-like patterns
	if len(value) < 100 {
		return false
	}

	// Heuristic: URLs typically contain :// or start with http
	if strings.Contains(value, "://") || strings.HasPrefix(value, "http") {
		return false
	}

	return true
}

// MaskURL masks a URL for display while keeping it recognizable.
// Shows the scheme and domain, masks the path and query parameters.
//
// No production caller, kept on purpose (#868): no URL carrying a credential is
// logged or printed anywhere today. Use it if one ever is (webhook URLs are the
// likely case).
func MaskURL(url string) string {
	if url == "" {
		return ""
	}

	// Find the position after the domain
	slashAfterDomain := strings.Index(url, "://")
	if slashAfterDomain == -1 {
		// Not a URL, use generic masking
		return MaskSecret(url)
	}

	slashAfterDomain += 3 // Move past ://
	nextSlash := strings.IndexByte(url[slashAfterDomain:], '/')
	if nextSlash == -1 {
		// No path, just show the domain
		return url
	}

	domainEnd := slashAfterDomain + nextSlash
	return url[:domainEnd] + "/****"
}
