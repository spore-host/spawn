package aws

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	//lint:ignore SA1019 mimics EC2, which encrypts the Windows password with PKCS #1 v1.5 (#923)
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
	//lint:ignore SA1019 mimics EC2, which encrypts the Windows password with PKCS #1 v1.5 (#923)
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

// TestWindowsSetAdminPasswordScript (#201, #1587): the script sets the
// Administrator account up for use (set password, never-expire, enable), and
// carries no password: only the sealing key's public half. SSM keeps a
// command's parameters for about 30 days.
func TestWindowsSetAdminPasswordScript(t *testing.T) {
	priv := testSealKey(t)
	script := windowsSetAdminPasswordScript(&priv.PublicKey)
	for _, want := range []string{
		"ConvertTo-SecureString $pw -AsPlainText -Force",
		"Set-LocalUser -Password $p",
		"-PasswordNeverExpires $true",
		"Enable-LocalUser -Name 'Administrator'",
		"$ErrorActionPreference = 'Stop'",
		"$k.Modulus = [Convert]::FromBase64String('" + base64.StdEncoding.EncodeToString(priv.N.Bytes()) + "')",
		"$rsa.Encrypt(",
		windowsSealedPasswordMarker,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q\n--- script ---\n%s", want, script)
		}
	}
	if strings.Contains(script, `ConvertTo-SecureString "`) {
		t.Error("the script embeds a password literal: it would be kept in SSM command history (#1587)")
	}
	if a, b := windowsSetAdminPasswordScript(&priv.PublicKey), windowsSetAdminPasswordScript(&priv.PublicKey); a != b {
		t.Error("the script varies for one key: something other than the public key is per call")
	}
}

func testSealKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048) // the size does not matter to the format; keeps the test fast
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

// The instance's answer is opened with the key, as the script seals it
// (RSA.Encrypt with RSAEncryptionPadding.OaepSHA256).
func TestOpenSealedWindowsPassword(t *testing.T) {
	priv := testSealKey(t)
	ct, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, &priv.PublicKey, []byte("Xy7!pQ"), nil)
	if err != nil {
		t.Fatal(err)
	}
	out := "noise\r\n" + windowsSealedPasswordMarker + base64.StdEncoding.EncodeToString(ct) + "\r\n"
	if pw, err := openSealedWindowsPassword(out, priv); err != nil || pw != "Xy7!pQ" {
		t.Errorf("openSealedWindowsPassword = %q, %v; want the sealed password", pw, err)
	}
	if _, err := openSealedWindowsPassword("Set the password\n", priv); err == nil {
		t.Error("output with no sealed password was accepted")
	}
	other := testSealKey(t)
	if _, err := openSealedWindowsPassword(out, other); err == nil {
		t.Error("a password sealed to another key was opened")
	}
}

// TestWindowsPasswordScriptRunsInPowerShell runs the script's generation and
// sealing in a real PowerShell when one is installed (Windows PowerShell on
// the Windows CI runner, pwsh elsewhere), with the account lines stubbed out,
// and opens what it prints: the password meets the complexity rule the
// account needs and decrypts with the key.
func TestWindowsPasswordScriptRunsInPowerShell(t *testing.T) {
	shell := "pwsh"
	if runtime.GOOS == "windows" {
		shell = "powershell"
	}
	path, err := exec.LookPath(shell)
	if err != nil {
		t.Skipf("%s not installed", shell)
	}
	priv := testSealKey(t)
	script := windowsSetAdminPasswordScript(&priv.PublicKey)
	for _, line := range []string{
		"Get-LocalUser -Name 'Administrator' | Set-LocalUser -Password $p -PasswordNeverExpires $true",
		"Enable-LocalUser -Name 'Administrator'",
	} {
		script = strings.Replace(script, line, "$null = $p", 1)
	}
	// As a file, as AWS-RunPowerShellScript runs its commands.
	file := filepath.Join(t.TempDir(), "set-password.ps1")
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(path, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", file).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", shell, err, out)
	}
	pw, err := openSealedWindowsPassword(string(out), priv)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(pw) != windowsPasswordLength {
		t.Errorf("password is %d characters, want %d", len(pw), windowsPasswordLength)
	}
	for _, set := range windowsPasswordCharsets {
		if !strings.ContainsAny(pw, set) {
			t.Errorf("password has no character from %q: Windows' complexity rule may refuse it", set)
		}
	}
	all := strings.Join(windowsPasswordCharsets, "")
	for _, r := range pw {
		if !strings.ContainsRune(all, r) {
			t.Errorf("password has %q, outside the character sets", r)
		}
	}
	if strings.Contains(string(out), pw) {
		t.Error("the script printed the password in clear")
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
