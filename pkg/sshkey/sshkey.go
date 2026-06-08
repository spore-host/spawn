// Package sshkey is the single source of truth for spawn's SSH/EC2 keypair
// management across operating systems.
//
// Why this exists: key handling had grown SSH/Linux-shaped and inconsistent —
// RSA hardcoded in one place, two divergent key-search functions with different
// priority orders, and a keyName↔filename indirection. Windows adds a hard
// requirement: the EC2 Administrator password (GetPasswordData) can only be
// decrypted with an RSA private key — ED25519 cannot.
//
// This package owns spawn's own keypair under ~/.spawn/keys (separate from the
// user's personal ~/.ssh), generated in-process (no ssh-keygen shell-out, which
// isn't guaranteed on Windows), with a single Resolve() used by connect/status/
// queue. ED25519 is the default; RSA is provisioned when the target is Windows.
package sshkey

import (
	"crypto/ed25519"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
)

// Algorithm identifies the keypair algorithm.
type Algorithm string

const (
	// ED25519 is the default for SSH access (AL2023, Ubuntu).
	ED25519 Algorithm = "ed25519"
	// RSA is required when the target is Windows (EC2 password decryption needs
	// an RSA private key; ED25519 cannot decrypt GetPasswordData).
	RSA Algorithm = "rsa"

	rsaBits = 4096
)

// KeyPair describes a spawn-managed keypair on disk.
type KeyPair struct {
	Name           string    // EC2 key name, e.g. "spawn-key-alice" or "spawn-key-alice-rsa"
	Algorithm      Algorithm // ed25519 | rsa
	PrivateKeyPath string    // ~/.spawn/keys/<name>
	PublicKeyPath  string    // ~/.spawn/keys/<name>.pub
}

// KeyDir returns spawn's managed key directory (~/.spawn/keys), creating it with
// 0700 perms. homeDir should be the platform home directory.
func KeyDir(homeDir string) (string, error) {
	dir := filepath.Join(homeDir, ".spawn", "keys")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("create spawn key dir: %w", err)
	}
	return dir, nil
}

// keyName returns the EC2/file name for a user+algorithm. ED25519 uses the bare
// "spawn-key-<user>"; RSA appends "-rsa" so both can coexist locally and in EC2.
func keyName(username string, algo Algorithm) string {
	if algo == RSA {
		return fmt.Sprintf("spawn-key-%s-rsa", username)
	}
	return fmt.Sprintf("spawn-key-%s", username)
}

// EnsureKey finds or creates spawn's managed keypair for the given algorithm
// under ~/.spawn/keys. It is idempotent: if the keypair already exists, it is
// returned unchanged. Keys are generated in-process and written with 0600
// (private) / 0644 (public).
func EnsureKey(homeDir, username string, algo Algorithm) (*KeyPair, error) {
	dir, err := KeyDir(homeDir)
	if err != nil {
		return nil, err
	}
	name := keyName(username, algo)
	kp := &KeyPair{
		Name:           name,
		Algorithm:      algo,
		PrivateKeyPath: filepath.Join(dir, name),
		PublicKeyPath:  filepath.Join(dir, name+".pub"),
	}

	// Idempotent: both files present → reuse.
	if fileExists(kp.PrivateKeyPath) && fileExists(kp.PublicKeyPath) {
		return kp, nil
	}

	privPEM, pubAuthorized, err := generate(algo)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(kp.PrivateKeyPath, privPEM, 0600); err != nil {
		return nil, fmt.Errorf("write private key: %w", err)
	}
	if err := os.WriteFile(kp.PublicKeyPath, pubAuthorized, 0644); err != nil { //nolint:gosec // public key
		return nil, fmt.Errorf("write public key: %w", err)
	}
	return kp, nil
}

// generate creates a keypair in-process and returns the PEM-encoded private key
// (OpenSSH-compatible) and the authorized_keys-format public key.
func generate(algo Algorithm) (privPEM, pubAuthorized []byte, err error) {
	switch algo {
	case RSA:
		priv, err := rsa.GenerateKey(rand.Reader, rsaBits)
		if err != nil {
			return nil, nil, fmt.Errorf("generate rsa key: %w", err)
		}
		// PKCS#1 PEM is what `ssh-keygen -t rsa` historically emits and is widely
		// accepted by ssh clients and AWS password decryption.
		privPEM = pem.EncodeToMemory(&pem.Block{
			Type:  "RSA PRIVATE KEY",
			Bytes: x509.MarshalPKCS1PrivateKey(priv),
		})
		sshPub, err := ssh.NewPublicKey(&priv.PublicKey)
		if err != nil {
			return nil, nil, fmt.Errorf("rsa ssh public key: %w", err)
		}
		return privPEM, ssh.MarshalAuthorizedKey(sshPub), nil

	case ED25519:
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, nil, fmt.Errorf("generate ed25519 key: %w", err)
		}
		block, err := ssh.MarshalPrivateKey(priv, "")
		if err != nil {
			return nil, nil, fmt.Errorf("marshal ed25519 private key: %w", err)
		}
		privPEM = pem.EncodeToMemory(block)
		sshPub, err := ssh.NewPublicKey(pub)
		if err != nil {
			return nil, nil, fmt.Errorf("ed25519 ssh public key: %w", err)
		}
		return privPEM, ssh.MarshalAuthorizedKey(sshPub), nil

	default:
		return nil, nil, fmt.Errorf("unsupported key algorithm %q", algo)
	}
}

// Resolve returns the path to a usable private key for the given EC2 key name.
// It is the ONE key resolver used by connect/status/queue (replacing the two
// previously divergent search functions).
//
// Search order:
//  1. spawn-managed keys in ~/.spawn/keys (exact name, then -rsa variant)
//  2. back-compat ~/.ssh patterns: exact name, name.pem, name.key, then the
//     default id_rsa / id_ed25519 / id_ecdsa — preserving prior findSSHKey
//     behavior so existing users keep working.
func Resolve(homeDir, keyName string) (string, error) {
	if keyName == "" {
		return "", fmt.Errorf("key name is required to locate SSH key")
	}

	spawnDir := filepath.Join(homeDir, ".spawn", "keys")
	sshDir := filepath.Join(homeDir, ".ssh")

	candidates := []string{
		// spawn-managed keys first (single source of truth).
		filepath.Join(spawnDir, keyName),
		filepath.Join(spawnDir, keyName+"-rsa"),
		// back-compat ~/.ssh search (matches the old findSSHKey order).
		filepath.Join(sshDir, keyName),
		filepath.Join(sshDir, keyName+".pem"),
		filepath.Join(sshDir, keyName+".key"),
		filepath.Join(sshDir, "id_rsa"),
		filepath.Join(sshDir, "id_ed25519"),
		filepath.Join(sshDir, "id_ecdsa"),
	}
	for _, p := range candidates {
		if fileExists(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("no SSH key found for %q (looked in ~/.spawn/keys and ~/.ssh)", keyName)
}

// Fingerprint returns the AWS-format MD5 fingerprint (colon-separated hex of the
// MD5 of the DER-encoded public key) for the authorized_keys-format public key
// at pubKeyPath. This matches what EC2 DescribeKeyPairs reports for an imported
// key, so callers can dedupe re-launches.
func Fingerprint(pubKeyPath string) (string, error) {
	data, err := os.ReadFile(pubKeyPath)
	if err != nil {
		return "", fmt.Errorf("read public key: %w", err)
	}
	pubKey, _, _, _, err := ssh.ParseAuthorizedKey(data)
	if err != nil {
		return "", fmt.Errorf("parse public key: %w", err)
	}
	cryptoKey, ok := pubKey.(ssh.CryptoPublicKey)
	if !ok {
		return "", fmt.Errorf("public key type does not expose crypto key")
	}
	derBytes, err := x509.MarshalPKIXPublicKey(cryptoKey.CryptoPublicKey())
	if err != nil {
		return "", fmt.Errorf("marshal public key to DER: %w", err)
	}
	// nosemgrep: use-of-md5 -- AWS SSH key fingerprint format requires MD5 (RFC 4716)
	hash := md5.Sum(derBytes)
	out := make([]byte, 0, len(hash)*3)
	const hexdigits = "0123456789abcdef"
	for i, b := range hash {
		if i > 0 {
			out = append(out, ':')
		}
		out = append(out, hexdigits[b>>4], hexdigits[b&0x0f])
	}
	return string(out), nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
