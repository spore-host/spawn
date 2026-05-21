package security

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
)

// EncryptSecret encrypts a plaintext secret using AWS KMS.
// Returns a base64-encoded encrypted ciphertext.
func EncryptSecret(ctx context.Context, kmsClient *kms.Client, keyID, plaintext string) (string, error) {
	if plaintext == "" {
		return "", errors.New("plaintext cannot be empty")
	}

	if keyID == "" {
		return "", errors.New("KMS key ID cannot be empty")
	}

	result, err := kmsClient.Encrypt(ctx, &kms.EncryptInput{
		KeyId:     aws.String(keyID),
		Plaintext: []byte(plaintext),
	})
	if err != nil {
		return "", err
	}

	return base64.StdEncoding.EncodeToString(result.CiphertextBlob), nil
}

// DecryptSecret decrypts a base64-encoded KMS-encrypted ciphertext.
// Returns the plaintext secret.
func DecryptSecret(ctx context.Context, kmsClient *kms.Client, ciphertext string) (string, error) {
	if ciphertext == "" {
		return "", errors.New("ciphertext cannot be empty")
	}

	// Decode from base64
	decoded, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", err
	}

	result, err := kmsClient.Decrypt(ctx, &kms.DecryptInput{
		CiphertextBlob: decoded,
	})
	if err != nil {
		return "", err
	}

	return string(result.Plaintext), nil
}

// MaskSecret masks a secret for display purposes.
// Shows the first 4 and last 4 characters, masking the middle.
// For short secrets (<=8 chars), returns all asterisks.
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
