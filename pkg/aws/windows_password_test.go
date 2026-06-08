package aws

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

// TestDecryptWindowsPassword_RoundTrip mimics what EC2 does: encrypt a password
// with the keypair's PUBLIC key (RSA PKCS#1 v1.5), then confirm we decrypt it
// with the private key.
func TestDecryptWindowsPassword_RoundTrip(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	const want = "Sup3r$ecretAdminPass!"
	ciphertext, err := rsa.EncryptPKCS1v15(rand.Reader, &priv.PublicKey, []byte(want))
	if err != nil {
		t.Fatal(err)
	}
	blob := base64.StdEncoding.EncodeToString(ciphertext)

	got, err := decryptWindowsPassword(blob, priv)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if got != want {
		t.Errorf("decrypted = %q, want %q", got, want)
	}
}

// TestDecryptWindowsPassword_FromPKCS1File exercises the full file path with a
// PKCS#1 PEM (what our generator and ssh-keygen -t rsa emit).
func TestDecryptWindowsPassword_FromPKCS1File(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(priv),
	})
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "spawn-key-test-rsa")
	if err := os.WriteFile(keyPath, pemBytes, 0600); err != nil {
		t.Fatal(err)
	}

	const want = "Another!Pass123"
	ct, err := rsa.EncryptPKCS1v15(rand.Reader, &priv.PublicKey, []byte(want))
	if err != nil {
		t.Fatal(err)
	}
	blob := base64.StdEncoding.EncodeToString(ct)

	got, err := DecryptWindowsPassword(blob, keyPath)
	if err != nil {
		t.Fatalf("DecryptWindowsPassword: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestDecryptWindowsPassword_Empty errors clearly when the blob isn't ready.
func TestDecryptWindowsPassword_Empty(t *testing.T) {
	if _, err := DecryptWindowsPassword("", "/nonexistent"); err == nil {
		t.Error("empty password data must error")
	}
}

// TestParseRSAPrivateKey_RejectsED25519 ensures ED25519 keys are rejected with a
// clear message — they cannot decrypt EC2 Windows passwords.
func TestParseRSAPrivateKey_RejectsED25519(t *testing.T) {
	_, edPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(edPriv)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	_, err = parseRSAPrivateKey(pemBytes)
	if err == nil {
		t.Fatal("ED25519 key must be rejected for Windows password decryption")
	}
}

// TestParseRSAPrivateKey_AcceptsPKCS8RSA accepts an RSA key in PKCS#8 form too.
func TestParseRSAPrivateKey_AcceptsPKCS8RSA(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if _, err := parseRSAPrivateKey(pemBytes); err != nil {
		t.Errorf("PKCS#8 RSA key should be accepted: %v", err)
	}
}
