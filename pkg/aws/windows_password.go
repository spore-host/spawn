package aws

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
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
	ec2Client := c.regionalEC2(region)

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
	// EC2 GetPasswordData encrypts with PKCS #1 v1.5; the format is AWS's, not a
	// choice we can make. The padding-oracle concern does not apply: this runs
	// once, locally, on the user's own key, and its result is not exposed to a
	// remote party.
	//lint:ignore SA1019 the EC2 Windows password format is PKCS #1 v1.5 by AWS's definition (#923)
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

// windowsPasswordCharsets are the four character classes a Windows password must
// draw from to satisfy the default complexity policy (≥3 of the 4). We guarantee
// one of each, then fill the rest from the union — so the generated password
// always passes `net user` regardless of the local policy. The sets exclude
// characters that are awkward to paste or that PowerShell would treat specially
// inside a double-quoted string (", `, $, \) — see SetWindowsAdminPasswordViaSSM.
var windowsPasswordCharsets = []string{
	"ABCDEFGHJKLMNPQRSTUVWXYZ", // upper (no I/O — avoid look-alikes)
	"abcdefghijkmnpqrstuvwxyz", // lower (no l/o)
	"23456789",                 // digits (no 0/1)
	"!@#%^&*()-_=+[]{}:?.",     // symbols (no quote/backtick/$/\\ for PS safety)
}

// GenerateWindowsPassword returns a cryptographically-random password of length
// n (minimum 14) that satisfies Windows' default complexity policy: at least one
// character from each of the upper/lower/digit/symbol classes. It uses crypto/rand
// for every choice. The character set deliberately omits quote/backtick/$/\ so the
// value can be embedded safely in the PowerShell command SetWindowsAdminPassword‐
// ViaSSM sends over SSM.
func GenerateWindowsPassword(n int) (string, error) {
	if n < 14 {
		n = 14
	}
	all := strings.Join(windowsPasswordCharsets, "")
	out := make([]byte, 0, n)

	// One guaranteed character from each class first (complexity).
	for _, set := range windowsPasswordCharsets {
		ch, err := randChar(set)
		if err != nil {
			return "", err
		}
		out = append(out, ch)
	}
	// Fill the remainder from the union of all classes.
	for len(out) < n {
		ch, err := randChar(all)
		if err != nil {
			return "", err
		}
		out = append(out, ch)
	}
	// Shuffle so the guaranteed leading class characters aren't positionally
	// predictable (Fisher–Yates with crypto/rand).
	for i := len(out) - 1; i > 0; i-- {
		jBig, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			return "", err
		}
		j := jBig.Int64()
		out[i], out[j] = out[j], out[i]
	}
	return string(out), nil
}

// randChar returns one cryptographically-random character from set.
func randChar(set string) (byte, error) {
	idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(set))))
	if err != nil {
		return 0, err
	}
	return set[idx.Int64()], nil
}

// SetWindowsAdminPasswordViaSSM sets the local Administrator account's password
// on a Windows instance over SSM RunCommand, and ensures the account is enabled
// and not expired, then returns the password it set (#201).
//
// This is spawn's SSM-first answer to EC2Launch's once-per-Sysprep password
// generation: `setAdminAccount` runs only on the first boot after a Sysprep and
// then disables the retrievable password, so an instance launched from a warm
// (re-imaged, never re-Sysprepped) AMI never produces a GetPasswordData-retriev‐
// able password (#201, #98). Rather than depend on that, spawn owns the
// credential, uniformly on warm and base AMIs. Requires the SSM agent Online
// and ssm:SendCommand on the caller (the same dependency `connect` already has).
//
// The password never appears in SSM (#1587). SSM keeps a command's parameters
// and output for about 30 days, readable with ssm:ListCommands and
// GetCommandInvocation, so a password interpolated into the command, as it was
// until #1587, was readable by anyone with SSM read for a month. Instead, as EC2
// GetPasswordData does, the instance generates the password itself and returns
// it encrypted to a key only this process holds: spawn makes a single-use RSA
// key pair in memory, the command carries only its public half, the script
// sets a random password and prints it RSA-OAEP-encrypted to that key, and
// spawn decrypts it here. The history holds a public key and ciphertext. A
// Windows whose .NET cannot run the script (older than 4.6.2) fails the
// command, and connect falls back to GetPasswordData as before.
func (c *Client) SetWindowsAdminPasswordViaSSM(ctx context.Context, region, instanceID string, timeout time.Duration) (string, error) {
	priv, err := rsa.GenerateKey(rand.Reader, windowsPasswordSealBits)
	if err != nil {
		return "", fmt.Errorf("generate the password's sealing key: %w", err)
	}
	ps := windowsSetAdminPasswordScript(&priv.PublicKey)
	res, err := c.RunPowerShell(ctx, region, instanceID, ps, timeout)
	if err != nil {
		return "", fmt.Errorf("set Administrator password over SSM: %w", err)
	}
	if res.Status != "Success" {
		return "", fmt.Errorf("set Administrator password over SSM: command status %s%s", res.Status, ssmErrTail(res.Stderr))
	}
	return openSealedWindowsPassword(res.Stdout, priv)
}

// windowsPasswordSealBits is the size of the single-use RSA key the password
// is returned under.
const windowsPasswordSealBits = 3072

// windowsSealedPasswordMarker prefixes the one output line carrying the sealed
// password.
const windowsSealedPasswordMarker = "SPAWN-SEALED-PASSWORD:"

// windowsSetAdminPasswordScript is the PowerShell that generates a random
// Administrator password on the instance, sets it, makes sure the account can
// be used for RDP (enabled, password not expiring), and prints it encrypted to
// pub (RSA-OAEP with SHA-256) after windowsSealedPasswordMarker. Pure/testable
// — no AWS calls.
// The only per-call value in it is pub, so nothing secret reaches SSM.
//
// The password is 20 characters with one from each of windowsPasswordCharsets
// and the rest from their union, drawn without modulo bias from
// RandomNumberGenerator and shuffled, as GenerateWindowsPassword draws them.
func windowsSetAdminPasswordScript(pub *rsa.PublicKey) string {
	modulus := base64.StdEncoding.EncodeToString(pub.N.Bytes())
	exponent := base64.StdEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes())
	quoted := make([]string, len(windowsPasswordCharsets))
	for i, set := range windowsPasswordCharsets {
		quoted[i] = "'" + set + "'" // no set contains a single quote
	}
	return strings.Join([]string{
		`$ErrorActionPreference = 'Stop'`,
		`$rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()`,
		`function Get-SpawnIndex([int]$n) {`,
		`  $span = [uint64]4294967296`,
		`  $limit = $span - ($span % [uint64]$n)`,
		`  $b = New-Object byte[] 4`,
		`  do { $rng.GetBytes($b); $v = [uint64][BitConverter]::ToUInt32($b, 0) } while ($v -ge $limit)`,
		`  return [int]($v % [uint64]$n)`,
		`}`,
		`$sets = @(` + strings.Join(quoted, ", ") + `)`,
		`$all = -join $sets`,
		`$chars = New-Object System.Collections.Generic.List[char]`,
		`foreach ($s in $sets) { $chars.Add($s[(Get-SpawnIndex $s.Length)]) }`,
		fmt.Sprintf(`while ($chars.Count -lt %d) { $chars.Add($all[(Get-SpawnIndex $all.Length)]) }`, windowsPasswordLength),
		`for ($i = $chars.Count - 1; $i -gt 0; $i--) { $j = Get-SpawnIndex ($i + 1); $t = $chars[$i]; $chars[$i] = $chars[$j]; $chars[$j] = $t }`,
		`$pw = -join $chars`,
		`$p = ConvertTo-SecureString $pw -AsPlainText -Force`,
		`Get-LocalUser -Name 'Administrator' | Set-LocalUser -Password $p -PasswordNeverExpires $true`,
		`Enable-LocalUser -Name 'Administrator'`,
		// RSACng (.NET Framework 4.6.2+, every supported Windows Server) does
		// OAEP-SHA256; RSACryptoServiceProvider, which RSA.Create() returns
		// there, does not. RSA.Create() is the fallback for .NET on other
		// platforms, where RSACng does not exist (the test runs it in pwsh).
		`try { $rsa = New-Object System.Security.Cryptography.RSACng } catch { $rsa = [System.Security.Cryptography.RSA]::Create() }`,
		`$k = New-Object System.Security.Cryptography.RSAParameters`,
		fmt.Sprintf(`$k.Modulus = [Convert]::FromBase64String('%s')`, modulus),
		fmt.Sprintf(`$k.Exponent = [Convert]::FromBase64String('%s')`, exponent),
		`$rsa.ImportParameters($k)`,
		`$sealed = [Convert]::ToBase64String($rsa.Encrypt([System.Text.Encoding]::UTF8.GetBytes($pw), [System.Security.Cryptography.RSAEncryptionPadding]::OaepSHA256))`,
		`$pw = $null`,
		`Write-Output ('` + windowsSealedPasswordMarker + `' + $sealed)`,
	}, "\n")
}

// windowsPasswordLength is the length of the password the instance generates.
const windowsPasswordLength = 20

// openSealedWindowsPassword finds the sealed password in the script's output
// and decrypts it with priv.
func openSealedWindowsPassword(stdout string, priv *rsa.PrivateKey) (string, error) {
	for _, line := range strings.Split(stdout, "\n") {
		i := strings.Index(line, windowsSealedPasswordMarker)
		if i < 0 {
			continue
		}
		rest := line[i+len(windowsSealedPasswordMarker):]
		ct, err := base64.StdEncoding.DecodeString(strings.TrimSpace(rest))
		if err != nil {
			return "", fmt.Errorf("the instance returned a malformed sealed password: %w", err)
		}
		pt, err := rsa.DecryptOAEP(sha256.New(), nil, priv, ct, nil)
		if err != nil {
			return "", fmt.Errorf("decrypt the password the instance set: %w", err)
		}
		return string(pt), nil
	}
	return "", fmt.Errorf("the instance set the Administrator password but did not return it sealed (no %s line in its output)", windowsSealedPasswordMarker)
}

// ssmErrTail returns a trimmed, prefixed tail of SSM stderr for error messages,
// or "" when there's nothing useful — keeps a failed-command error readable.
func ssmErrTail(stderr string) string {
	s := strings.TrimSpace(stderr)
	if s == "" {
		return ""
	}
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return ": " + s
}
