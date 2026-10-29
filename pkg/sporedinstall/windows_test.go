package sporedinstall

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The Windows verified install is EXECUTED, not string-matched (#879).
//
// CI cannot run Windows PowerShell 5.1, but it can run PowerShell 7 — GitHub's
// ubuntu-latest runners ship pwsh — and every API the script uses exists in both.
// The script calls `& $aws s3 cp ...`; in PowerShell $aws may be a script block, so
// the harness substitutes a fake aws that serves objects from a local directory.
// That runs the WHOLE generated script: source order, refusal, fallback, and the
// fail-closed ending — the parts a function-level test would never reach.
//
// Skips only where pwsh is absent AND this is not CI. In CI a missing pwsh FAILS:
// a skip would let the only proof that Windows spores verify spored quietly stop
// running.

func pwshOrSkip(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("pwsh")
	if err == nil {
		return p
	}
	if os.Getenv("CI") != "" {
		t.Fatal("pwsh is not available in CI; the Windows spored verifier cannot be executed, " +
			"and a skip here would silently drop the only test that it works")
	}
	t.Skip("pwsh not installed (install PowerShell 7 to run the Windows verifier test locally)")
	return ""
}

// winFixture is a signing key and a set of S3 objects to serve.
type winFixture struct {
	key     *ecdsa.PrivateKey
	pubPEM  string
	objects map[string][]byte // s3:// URL -> bytes
}

func newWinFixture(t *testing.T) *winFixture {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return &winFixture{
		key:     k,
		pubPEM:  string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})),
		objects: map[string][]byte{},
	}
}

// publish places a binary with a correct .sha256 and a .sig made by signer.
func (f *winFixture) publish(t *testing.T, url string, bin []byte, signer *ecdsa.PrivateKey) {
	t.Helper()
	sum := sha256.Sum256(bin)
	f.objects[url] = bin
	f.objects[url+".sha256"] = []byte(hex.EncodeToString(sum[:]) + "\n")
	if signer != nil {
		sig, err := ecdsa.SignASN1(rand.Reader, signer, sum[:])
		if err != nil {
			t.Fatal(err)
		}
		f.objects[url+".sig"] = []byte(base64.StdEncoding.EncodeToString(sig) + "\n")
	}
}

const (
	regional  = "s3://spawn-binaries-eu-west-1/spawn/spored-windows-amd64.exe"
	rootPath  = "s3://spawn-binaries-eu-west-1/spored-windows-amd64.exe"
	fallback  = "s3://spawn-binaries-us-east-1/spawn/spored-windows-amd64.exe"
	installed = "spored/spored.exe"
)

// run executes the generated install script against the winFixture and returns the
// console output and the bytes installed (nil if nothing was installed).
func (f *winFixture) run(t *testing.T, keyPEM string) (string, []byte) {
	t.Helper()
	pwsh := pwshOrSkip(t)
	script, err := WindowsInstallScript(keyPEM, "")
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	store := filepath.Join(dir, "s3")
	progFiles := filepath.Join(dir, "ProgramFiles")
	tmp := filepath.Join(dir, "Temp")
	for _, d := range []string{store, progFiles, tmp} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Objects are stored under a name derived from their URL, so the fake aws can
	// map an s3:// URL to a file without parsing it.
	name := func(url string) string { return hex.EncodeToString([]byte(url)) }
	for url, b := range f.objects {
		if err := os.WriteFile(filepath.Join(store, name(url)), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	harness := `
$ErrorActionPreference = 'Continue'
$env:ProgramFiles = '` + progFiles + `'
$env:TEMP = '` + tmp + `'
$region = 'eu-west-1'
$store = '` + store + `'
# Fake aws: "s3 cp <src> <dst> --region <r>". Serves a stored object or fails,
# setting $LASTEXITCODE as the real CLI would.
$aws = {
  $src = $args[2]; $dst = $args[3]
  $hex = -join ([Text.Encoding]::UTF8.GetBytes($src) | ForEach-Object { $_.ToString('x2') })
  $p = Join-Path $store $hex
  if (Test-Path $p) { Copy-Item -Force $p $dst; $global:LASTEXITCODE = 0 } else { $global:LASTEXITCODE = 1 }
}
` + script + `
"RESULT installed=$sporedInstalled"
`
	sf := filepath.Join(dir, "run.ps1")
	if err := os.WriteFile(sf, []byte(harness), 0o600); err != nil {
		t.Fatal(err)
	}
	// pwsh comes from LookPath and sf is a script this test wrote to its own temp
	// dir — no outside input reaches the command line.
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	out, err := exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-File", sf).CombinedOutput() //nolint:gosec // the test's own generated script
	if err != nil {
		t.Fatalf("pwsh failed: %v\n%s", err, out)
	}
	got, rerr := os.ReadFile(filepath.Join(progFiles, installed))
	if rerr != nil {
		got = nil
	}
	return string(out), got
}

func wantInstalled(t *testing.T, out string, got, want []byte) {
	t.Helper()
	if !strings.Contains(out, "RESULT installed=True") || got == nil {
		t.Fatalf("spored was not installed, but should have been:\n%s", out)
	}
	if string(got) != string(want) {
		t.Fatalf("the installed binary is not the expected one")
	}
}

func wantRefused(t *testing.T, out string, got []byte) {
	t.Helper()
	if strings.Contains(out, "RESULT installed=True") || got != nil {
		t.Fatalf("spored WAS installed, but every source should have been refused — an "+
			"unverified binary would run as a SYSTEM service:\n%s", out)
	}
	if !strings.Contains(out, "spored was NOT installed") {
		t.Errorf("refused, but the console does not say so; an operator reading "+
			"GetConsoleOutput would not learn this instance has no lifecycle:\n%s", out)
	}
}

var bin = []byte("MZ\x90\x00 genuine spored.exe bytes for the test")

func TestWindowsInstallsAVerifiedBinary(t *testing.T) {
	f := newWinFixture(t)
	f.publish(t, regional, bin, f.key)
	out, got := f.run(t, f.pubPEM)
	wantInstalled(t, out, got, bin)
}

// A source that fails verification is skipped and the next one tried — and the
// binary that ends up installed is the VERIFIED one, not the tampered one.
func TestWindowsSkipsATamperedSourceAndFallsBack(t *testing.T) {
	f := newWinFixture(t)
	f.publish(t, regional, bin, f.key)
	f.objects[regional] = []byte("MZ tampered") // .sha256/.sig still describe the genuine binary
	f.publish(t, fallback, bin, f.key)
	out, got := f.run(t, f.pubPEM)
	wantInstalled(t, out, got, bin)
	if !strings.Contains(out, "REFUSING spored from "+regional) {
		t.Errorf("the tampered source was not reported as refused:\n%s", out)
	}
}

func TestWindowsRefusesWhenEverySourceIsTampered(t *testing.T) {
	f := newWinFixture(t)
	for _, u := range []string{regional, rootPath, fallback} {
		f.publish(t, u, bin, f.key)
		f.objects[u] = []byte("MZ tampered")
	}
	out, got := f.run(t, f.pubPEM)
	wantRefused(t, out, got)
}

// The checksum alone is not enough: an attacker who can write the bucket can write
// a matching .sha256. This is the case signature verification exists for.
func TestWindowsRefusesAMatchingChecksumWithAForeignSignature(t *testing.T) {
	f := newWinFixture(t)
	attacker, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	evil := []byte("MZ attacker binary")
	f.publish(t, regional, evil, attacker) // correct .sha256 for evil, signed by the wrong key
	out, got := f.run(t, f.pubPEM)
	wantRefused(t, out, got)
}

func TestWindowsRefusesAMissingSignature(t *testing.T) {
	f := newWinFixture(t)
	f.publish(t, regional, bin, nil) // binary + .sha256, no .sig
	out, got := f.run(t, f.pubPEM)
	wantRefused(t, out, got)
}

func TestWindowsRefusesAMissingChecksum(t *testing.T) {
	f := newWinFixture(t)
	f.publish(t, regional, bin, f.key)
	delete(f.objects, regional+".sha256")
	out, got := f.run(t, f.pubPEM)
	wantRefused(t, out, got)
}

// A spawn build with no signing key mirrors Linux's SPORED_SIG_VERIFY=0: the
// checksum is still required, the signature is not.
func TestWindowsWithoutAKeyStillRequiresTheChecksum(t *testing.T) {
	f := newWinFixture(t)
	f.publish(t, regional, bin, nil)
	out, got := f.run(t, "")
	wantInstalled(t, out, got, bin)

	g := newWinFixture(t)
	g.publish(t, regional, bin, nil)
	g.objects[regional] = []byte("MZ tampered")
	out, got = g.run(t, "")
	wantRefused(t, out, got)
}

// DER-encoded ECDSA integers vary in length: 33 bytes when the high bit is set
// (a leading 0x00), and fewer than 32 when the value is small. A converter that
// assumes fixed widths verifies most signatures and silently rejects some, which
// on Windows means some launches simply get no spored. Sign until both shapes have
// been exercised.
func TestWindowsAcceptsEveryDERIntegerShape(t *testing.T) {
	f := newWinFixture(t)
	sum := sha256.Sum256(bin)
	var long, short bool
	// A short integer (< 32 bytes) occurs about once per 256 integers, so with two
	// per signature the first version's 400 tries missed it ~4% of the time — and
	// CI drew that 4% (#881). Signing is cheap and pwsh runs only when a NEW shape
	// appears, so the budget can be large: 5000 tries miss with probability ~1e-17.
	for i := 0; i < 5000 && !(long && short); i++ {
		sig, err := ecdsa.SignASN1(rand.Reader, f.key, sum[:])
		if err != nil {
			t.Fatal(err)
		}
		rl, sl := int(sig[3]), int(sig[5+int(sig[3])])
		isLong := rl == 33 || sl == 33
		isShort := rl < 32 || sl < 32
		if (isLong && !long) || (isShort && !short) {
			f.publish(t, regional, bin, nil)
			f.objects[regional+".sig"] = []byte(base64.StdEncoding.EncodeToString(sig))
			out, got := f.run(t, f.pubPEM)
			wantInstalled(t, out, got, bin)
			long = long || isLong
			short = short || isShort
		}
	}
	if !long || !short {
		t.Fatalf("did not exercise both DER shapes (long=%v short=%v)", long, short)
	}
}

// The coordinates embedded for Windows must be the key Linux verifies with.
func TestWindowsKeyIsTheLinuxKey(t *testing.T) {
	if !VerificationEnabled() {
		t.Skip("no signing key compiled into this build")
	}
	x, y, err := p256Coordinates(SigningKeyPEM)
	if err != nil {
		t.Fatalf("SigningKeyPEM does not parse as P-256: %v", err)
	}
	s, err := WindowsInstallScript(SigningKeyPEM, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s, "$sporeKeyX = '"+x+"'") || !strings.Contains(s, "$sporeKeyY = '"+y+"'") {
		t.Error("the Windows script does not embed SigningKeyPEM's coordinates")
	}
	if !strings.Contains(s, "$sporeSigVerify = $true") {
		t.Error("a build with a signing key generated a Windows script with verification off")
	}
}

// scripts/install-spored.ps1 is GENERATED from the same verifier the launch
// bootstrap uses, so the two cannot drift. This fails if the committed file is not
// what the generator produces.
func TestInstallSporedPs1IsGenerated(t *testing.T) {
	want, err := WindowsStandaloneScript(SigningKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("..", "..", "scripts", "install-spored.ps1")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil { //nolint:gosec // a committed script
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != want {
		t.Errorf("%s is not what WindowsStandaloneScript generates. It must be generated, not "+
			"edited: a hand-maintained copy of the verifier is how the Windows path came to skip "+
			"verification in the first place. Regenerate:\n  UPDATE_GOLDEN=1 go test "+
			"./pkg/sporedinstall -run TestInstallSporedPs1IsGenerated", path)
	}
	for _, must := range []string{"Test-SporeBinary $cand", "if (-not $sporedInstalled) { exit 1 }"} {
		if !strings.Contains(want, must) {
			t.Errorf("the standalone installer lost %q", must)
		}
	}
}

// p256Coordinates must return the key's own point, each coordinate exactly 32
// bytes, including when a coordinate's leading byte is zero — the case .NET's
// fixed-width ECPoint rejects if left unpadded. Round-tripped through the
// standard library's own decoder rather than compared with PublicKey.X/.Y, which
// are deprecated (#923).
func TestP256CoordinatesRoundTripFixedWidth(t *testing.T) {
	sawShort := false
	for i := 0; i < 5000 && (i < 50 || !sawShort); i++ {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
		if err != nil {
			t.Fatal(err)
		}
		x, y, err := p256Coordinates(string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})))
		if err != nil {
			t.Fatal(err)
		}
		if len(x) != 64 || len(y) != 64 {
			t.Fatalf("coordinates are %d/%d hex chars, want 64 each", len(x), len(y))
		}
		if strings.HasPrefix(x, "00") || strings.HasPrefix(y, "00") {
			sawShort = true
		}
		pt, _ := hex.DecodeString("04" + x + y)
		back, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), pt)
		if err != nil {
			t.Fatalf("coordinates do not decode as a P-256 point: %v", err)
		}
		if !back.Equal(&key.PublicKey) {
			t.Fatal("coordinates decode to a different key")
		}
	}
	if !sawShort {
		t.Fatal("never exercised a coordinate with a leading zero byte")
	}
}
