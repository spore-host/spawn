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
	"strings"
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

// TestGenerateWindowsPassword_Complexity verifies the generated password meets
// Windows' default complexity policy: ≥1 upper, lower, digit, and symbol, and is
// at least the floor length (#201).
func TestGenerateWindowsPassword_Complexity(t *testing.T) {
	for i := 0; i < 200; i++ {
		pw, err := GenerateWindowsPassword(20)
		if err != nil {
			t.Fatalf("GenerateWindowsPassword: %v", err)
		}
		if len(pw) != 20 {
			t.Fatalf("len = %d, want 20", len(pw))
		}
		var hasUpper, hasLower, hasDigit, hasSym bool
		for _, r := range pw {
			switch {
			case strings.ContainsRune(windowsPasswordCharsets[0], r):
				hasUpper = true
			case strings.ContainsRune(windowsPasswordCharsets[1], r):
				hasLower = true
			case strings.ContainsRune(windowsPasswordCharsets[2], r):
				hasDigit = true
			case strings.ContainsRune(windowsPasswordCharsets[3], r):
				hasSym = true
			default:
				t.Fatalf("password %q contains char %q outside the allowed sets", pw, r)
			}
		}
		if !(hasUpper && hasLower && hasDigit && hasSym) {
			t.Fatalf("password %q missing a required class (upper=%v lower=%v digit=%v sym=%v)",
				pw, hasUpper, hasLower, hasDigit, hasSym)
		}
	}
}

// TestGenerateWindowsPassword_FloorLength enforces the 14-char minimum even when
// a smaller length is requested.
func TestGenerateWindowsPassword_FloorLength(t *testing.T) {
	pw, err := GenerateWindowsPassword(4)
	if err != nil {
		t.Fatal(err)
	}
	if len(pw) < 14 {
		t.Errorf("len = %d, want ≥ 14 (floor)", len(pw))
	}
}

// TestGenerateWindowsPassword_PowerShellSafe ensures the generated password never
// contains characters that would break the double-quoted PowerShell string it's
// embedded in (" ` $ \) — the property that lets SetWindowsAdminPasswordViaSSM
// interpolate it without escaping (#201).
func TestGenerateWindowsPassword_PowerShellSafe(t *testing.T) {
	const unsafe = "\"`$\\"
	for i := 0; i < 200; i++ {
		pw, err := GenerateWindowsPassword(20)
		if err != nil {
			t.Fatal(err)
		}
		if strings.ContainsAny(pw, unsafe) {
			t.Fatalf("password %q contains a PowerShell-unsafe char from %q", pw, unsafe)
		}
	}
}

// TestGenerateWindowsPassword_Random checks two successive passwords differ (a
// crude but sufficient guard against a constant/degenerate generator).
func TestGenerateWindowsPassword_Random(t *testing.T) {
	a, err := GenerateWindowsPassword(20)
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateWindowsPassword(20)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Errorf("two generated passwords were identical: %q", a)
	}
}

// TestWindowsSetAdminPasswordScript embeds the password and sets the account up
// for use (set password, never-expire, enable) (#201).
func TestWindowsSetAdminPasswordScript(t *testing.T) {
	script := windowsSetAdminPasswordScript("Abc123!def456GH")
	for _, want := range []string{
		`ConvertTo-SecureString "Abc123!def456GH" -AsPlainText -Force`,
		"Set-LocalUser -Password $p",
		"-PasswordNeverExpires $true",
		"Enable-LocalUser -Name 'Administrator'",
		"$ErrorActionPreference = 'Stop'",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q\n--- script ---\n%s", want, script)
		}
	}
}

// TestSSMErrTail trims and prefixes stderr, and is empty when there's nothing.
func TestSSMErrTail(t *testing.T) {
	if got := ssmErrTail("  "); got != "" {
		t.Errorf("blank stderr → %q, want empty", got)
	}
	if got := ssmErrTail("boom"); got != ": boom" {
		t.Errorf("got %q, want %q", got, ": boom")
	}
	long := strings.Repeat("x", 400)
	got := ssmErrTail(long)
	if !strings.HasPrefix(got, ": ") || !strings.HasSuffix(got, "…") || len(got) > 310 {
		t.Errorf("long stderr not truncated as expected: len=%d", len(got))
	}
}
