package aws

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

// GetPasswordData returns the encrypted Windows Administrator password blob for
// an instance (base64-encoded, RSA-encrypted with the instance's keypair). It is
// empty until Windows finishes generating it (~4-8 min after first boot).
//
// This is a thin wrapper over the EC2 API; decryption is DecryptWindowsPassword.
// Not yet wired into `connect` — that lands with the Windows connect flow
// (spore-host/spawn#55 Phase 1 / #77).
func (c *Client) GetPasswordData(ctx context.Context, region, instanceID string) (string, error) {
	cfg := c.cfg.Copy()
	cfg.Region = region
	ec2Client := ec2.NewFromConfig(cfg)

	out, err := ec2Client.GetPasswordData(ctx, &ec2.GetPasswordDataInput{
		InstanceId: aws.String(instanceID),
	})
	if err != nil {
		return "", fmt.Errorf("get password data: %w", err)
	}
	return strings.TrimSpace(aws.ToString(out.PasswordData)), nil
}

// WaitForPasswordData polls GetPasswordData until the blob is available or the
// timeout elapses. Windows does not generate the password until well into first
// boot, so callers that need it immediately after launch must wait.
func (c *Client) WaitForPasswordData(ctx context.Context, region, instanceID string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		blob, err := c.GetPasswordData(ctx, region, instanceID)
		if err != nil {
			return "", err
		}
		if blob != "" {
			return blob, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("password data not available within %s (Windows still generating it?)", timeout)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
}

// DecryptWindowsPassword decrypts the base64 RSA-encrypted password blob from
// GetPasswordData using the RSA private key at privateKeyPath. EC2 encrypts the
// Administrator password with RSA PKCS#1 v1.5 against the launch keypair's public
// key, so an RSA private key is required — ED25519 keys cannot decrypt it (this
// is why spawn provisions an RSA keypair for Windows launches).
func DecryptWindowsPassword(passwordData, privateKeyPath string) (string, error) {
	if passwordData == "" {
		return "", fmt.Errorf("password data is empty (not generated yet?)")
	}
	keyBytes, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return "", fmt.Errorf("read private key: %w", err)
	}
	priv, err := parseRSAPrivateKey(keyBytes)
	if err != nil {
		return "", err
	}
	return decryptWindowsPassword(passwordData, priv)
}

// decryptWindowsPassword is the pure decryption core (unit-testable without disk).
func decryptWindowsPassword(passwordData string, priv *rsa.PrivateKey) (string, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(passwordData)
	if err != nil {
		return "", fmt.Errorf("base64-decode password data: %w", err)
	}
	plaintext, err := rsa.DecryptPKCS1v15(nil, priv, ciphertext)
	if err != nil {
		return "", fmt.Errorf("decrypt password (is this the RSA key the instance launched with?): %w", err)
	}
	return string(plaintext), nil
}

// parseRSAPrivateKey parses a PEM private key and asserts it is RSA. ED25519
// keys are rejected with a clear message (they cannot decrypt EC2 passwords).
func parseRSAPrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found in private key")
	}
	// PKCS#1 (RSA PRIVATE KEY) — what `ssh-keygen -t rsa` and our generator emit.
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	// PKCS#8 (PRIVATE KEY) — assert the inner key is RSA.
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rsaKey, ok := k.(*rsa.PrivateKey); ok {
			return rsaKey, nil
		}
		return nil, fmt.Errorf("private key is not RSA (ED25519/ECDSA keys cannot decrypt Windows passwords; launch the instance with an RSA key)")
	}
	return nil, fmt.Errorf("unsupported or non-RSA private key format")
}
