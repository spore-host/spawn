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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// These tests RUN the generated shell rather than matching strings in it.
//
// That is the point. The defect this package exists to fix (GHSA-3x4q-99gh-fxhx,
// #819) was two install paths whose scripts contained plausible download code and
// no verification, and the gates in the repo at the time asserted things like
// `strings.Contains(script, "sha256sum")` — satisfied by a comment, and satisfied
// equally by a fail-OPEN checksum block. A substring cannot distinguish "verifies"
// from "mentions verifying", and cannot distinguish "aborts" from "warns and
// continues". Executing it can.

// fixture serves a signed spored artifact over a local HTTP server, with knobs
// for each way a source can be hostile or broken.
type fixture struct {
	t        *testing.T
	dir      string
	keyPath  string // PEM public key the script verifies against
	binary   string
	bodies   map[string][]byte // path -> content; absent path = 404
	srv      *httptest.Server
	destPath string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("marshal pubkey: %v", err)
	}
	keyPath := filepath.Join(dir, "pub.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write pubkey: %v", err)
	}

	binaryName := "spored-linux-arm64"
	content := []byte("#!/bin/sh\necho I am spored\n")
	sum := sha256.Sum256(content)
	sig, err := ecdsa.SignASN1(rand.Reader, priv, sum[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	f := &fixture{
		t:        t,
		dir:      dir,
		keyPath:  keyPath,
		binary:   binaryName,
		destPath: filepath.Join(dir, "spored"),
		bodies: map[string][]byte{
			"/" + binaryName:             content,
			"/" + binaryName + ".sha256": []byte(hex.EncodeToString(sum[:]) + "  " + binaryName + "\n"),
			"/" + binaryName + ".sig":    []byte(base64.StdEncoding.EncodeToString(sig) + "\n"),
		},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := f.bodies[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// This serves a binary artifact and its detached signature to curl inside a
		// test. HTML escaping would corrupt both, which is the opposite of what the
		// rule wants. The marker has to sit on the line immediately above the finding.
		// nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter
		_, _ = w.Write(body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// run executes the generated installer against the fixture and reports its exit
// status, the combined output, and whether anything was installed.
func (f *fixture) run(sigVerify bool, bases ...string) (int, string, bool) {
	f.t.Helper()
	if len(bases) == 0 {
		bases = []string{f.srv.URL}
	}

	script := VerifiedInstallFunc()
	// Point the generated shell at the fixture instead of the real system paths.
	// Substituting the two constants is honest: everything else about the script —
	// the ordering, the fail-closed branches, the openssl invocation — is exactly
	// what ships.
	script = strings.ReplaceAll(script, Dest, f.destPath)
	script = strings.ReplaceAll(script, SigningKeyPath, f.keyPath)

	// macOS has shasum, not sha256sum. Shim rather than skip, so this gate runs
	// for whoever is actually editing the file.
	prelude := ""
	if _, err := exec.LookPath("sha256sum"); err != nil {
		if _, err := exec.LookPath("shasum"); err != nil {
			f.t.Skip("neither sha256sum nor shasum available")
		}
		prelude = "sha256sum() { shasum -a 256 \"$@\"; }\n"
	}

	verify := "0"
	if sigVerify {
		verify = "1"
	}
	full := fmt.Sprintf("%s%s\nSPORED_SIG_VERIFY=%s\n%s %q %s\n",
		prelude, script, verify, FuncName, f.binary, quoteAll(bases))

	// Executing the generated script IS the test. The input is this package's own
	// output plus a httptest URL; asserting on it as a string instead is precisely
	// the weaker check that let the #819 fail-open ship.
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd := exec.Command("bash", "-c", full) //nolint:gosec // this package's own generated script
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		code = 1
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
	}
	_, statErr := os.Stat(f.destPath)
	return code, string(out), statErr == nil
}

func quoteAll(ss []string) string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, fmt.Sprintf("%q", s))
	}
	return strings.Join(out, " ")
}

func requireBash(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Linux-only: executes the Linux shell installer; the Windows installer is PowerShell, covered by windows_test.go (#1002)")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl not available")
	}
}

// A verified artifact installs.
func TestInstallsAVerifiedArtifact(t *testing.T) {
	requireBash(t)
	f := newFixture(t)

	code, out, installed := f.run(true)
	if code != 0 {
		t.Fatalf("a correctly signed artifact failed to install (exit %d):\n%s", code, out)
	}
	if !installed {
		t.Fatalf("exit 0 but nothing was installed:\n%s", out)
	}
	for _, want := range []string{"Checksum verified", "Signature verified"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not report %q, so a reader cannot tell what was checked:\n%s", want, out)
		}
	}
}

// Every way the source can fail verification must abort WITHOUT installing.
//
// Table-driven over the hostile cases rather than one test each, because the
// property is identical in all of them and the thing that must not vary is
// "nothing lands in Dest".
func TestFailsClosedAndInstallsNothing(t *testing.T) {
	requireBash(t)

	cases := []struct {
		name      string
		sigVerify bool
		mutate    func(f *fixture)
		wantLog   string
	}{
		{
			name:      "no checksum published",
			sigVerify: true,
			mutate:    func(f *fixture) { delete(f.bodies, "/"+f.binary+".sha256") },
			// The fail-open this replaced: a missing checksum used to warn and continue.
			wantLog: "refusing to install an unverified spored",
		},
		{
			name:      "checksum mismatch",
			sigVerify: true,
			mutate:    func(f *fixture) { f.bodies["/"+f.binary+".sha256"] = []byte("deadbeef\n") },
			wantLog:   "Checksum verification FAILED",
		},
		{
			name:      "empty checksum file",
			sigVerify: true,
			mutate:    func(f *fixture) { f.bodies["/"+f.binary+".sha256"] = []byte("\n") },
			wantLog:   "Checksum verification FAILED",
		},
		{
			name:      "no signature, verification on",
			sigVerify: true,
			mutate:    func(f *fixture) { delete(f.bodies, "/"+f.binary+".sig") },
			// Probed before the download, so the source is skipped rather than committed to.
			wantLog: "No signature at",
		},
		{
			name:      "signature does not match the binary",
			sigVerify: true,
			mutate: func(f *fixture) {
				// Keep the signature; swap the binary and restate its checksum, so
				// only the signature can catch it. This is the bucket-compromise
				// case the checksum provably cannot detect.
				evil := []byte("#!/bin/sh\necho EVIL\n")
				sum := sha256.Sum256(evil)
				f.bodies["/"+f.binary] = evil
				f.bodies["/"+f.binary+".sha256"] = []byte(hex.EncodeToString(sum[:]) + "\n")
			},
			wantLog: "Signature verification FAILED",
		},
		{
			name:      "corrupt signature",
			sigVerify: true,
			mutate:    func(f *fixture) { f.bodies["/"+f.binary+".sig"] = []byte("not base64 at all!!\n") },
			wantLog:   "Signature verification FAILED",
		},
		{
			name:      "binary absent",
			sigVerify: false,
			mutate:    func(f *fixture) { delete(f.bodies, "/"+f.binary) },
			wantLog:   "Failed to download",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			c.mutate(f)

			code, out, installed := f.run(c.sigVerify)
			if code == 0 {
				t.Errorf("exited 0 despite %s:\n%s", c.name, out)
			}
			if installed {
				t.Errorf("INSTALLED spored despite %s — this is the whole defect:\n%s", c.name, out)
			}
			if !strings.Contains(out, c.wantLog) {
				t.Errorf("output does not explain the refusal (%q):\n%s", c.wantLog, out)
			}
		})
	}
}

// With no key compiled in, the checksum still runs and the log says what it means.
//
// Asserting the log matters as much as the behaviour: claiming authenticity it
// cannot check is how a downgrade goes unnoticed.
func TestWithoutAKeyItChecksCorruptionAndSaysSo(t *testing.T) {
	requireBash(t)
	f := newFixture(t)
	delete(f.bodies, "/"+f.binary+".sig")

	code, out, installed := f.run(false)
	if code != 0 || !installed {
		t.Fatalf("checksum-only install failed (exit %d, installed=%v):\n%s", code, installed, out)
	}
	if !strings.Contains(out, "corruption, not authenticity") {
		t.Errorf("the no-key path does not say it is checking corruption rather than authenticity:\n%s", out)
	}
	if strings.Contains(out, "Signature verified") {
		t.Errorf("claims a signature was verified with no key compiled in:\n%s", out)
	}
}

// A source that cannot satisfy the trust requirement is skipped, not committed to.
//
// This is the #440 shape: a stale unsigned regional bucket used to satisfy "did
// the binary download?", so the us-east-1 fallback never fired and the later .sig
// lookup hard-failed the install region-wide.
func TestAnUnsignedSourceFallsThroughToASignedOne(t *testing.T) {
	requireBash(t)

	good := newFixture(t)
	unsigned := newFixture(t)
	delete(unsigned.bodies, "/"+unsigned.binary+".sig")

	code, out, installed := good.run(true, unsigned.srv.URL, good.srv.URL)
	if code != 0 || !installed {
		t.Fatalf("did not fall through to the signed source (exit %d, installed=%v):\n%s", code, installed, out)
	}
	if !strings.Contains(out, "trying next source") {
		t.Errorf("did not report skipping the unsigned source:\n%s", out)
	}
	if !strings.Contains(out, "Downloaded spored from "+good.srv.URL) {
		t.Errorf("did not download from the signed source:\n%s", out)
	}
}

// The executable bit must go on only after verification.
//
// Ordering, not presence: a chmod +x before the checksum leaves an executable
// attacker-controlled file in /tmp even on the refusal path.
func TestChmodHappensAfterVerification(t *testing.T) {
	script := VerifiedInstallFunc()
	chmod := strings.Index(script, "chmod +x")
	if chmod < 0 {
		t.Fatal("no chmod +x in the installer — this gate is anchored on a string that no longer exists")
	}
	for _, earlier := range []string{"Checksum verification FAILED", "Signature verification FAILED"} {
		if i := strings.Index(script, earlier); i < 0 || i > chmod {
			t.Errorf("%q appears after chmod +x (or not at all); the binary must not become "+
				"executable until every check has passed", earlier)
		}
	}
}

// The production signing key must be a real P-256 public key.
//
// Emptying SigningKeyPEM is a one-literal, cleanly-compiling edit that turns every
// install into a checksum-only install, reopening the same-bucket gap #440 closed.
// Parsing it rather than checking it is non-empty is deliberate: a truncated or
// placeholder value satisfies a length check and fails openssl on every instance,
// which is the same silent downgrade.
func TestProductionSigningKeyIsRealAndUsable(t *testing.T) {
	if strings.TrimSpace(SigningKeyPEM) == "" {
		t.Fatal("no signing key is compiled in: every install would verify only the checksum, " +
			"served from the same bucket as the binary (#440)")
	}
	if !VerificationEnabled() {
		t.Fatal("VerificationEnabled() is false with a key present")
	}
	block, _ := pem.Decode([]byte(SigningKeyPEM))
	if block == nil {
		t.Fatal("the compiled-in signing key is not valid PEM")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatalf("the compiled-in signing key does not parse as a public key: %v", err)
	}
	ec, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("signing key is %T, want *ecdsa.PublicKey — the KMS key is ECC_NIST_P256", pub)
	}
	if ec.Curve != elliptic.P256() {
		t.Errorf("signing key curve is %s, want P-256 to match the KMS key spec (ECDSA_SHA_256)",
			ec.Curve.Params().Name)
	}
}

// KeySetupScript must enable verification when a key is present and disable it
// when not — and must never emit both assignments.
func TestKeySetupMatchesWhetherAKeyIsCompiledIn(t *testing.T) {
	orig := SigningKeyPEM
	t.Cleanup(func() { SigningKeyPEM = orig })

	SigningKeyPEM = "-----BEGIN PUBLIC KEY-----\nTESTKEY\n-----END PUBLIC KEY-----"
	on := KeySetupScript()
	if !strings.Contains(on, "\nSPORED_SIG_VERIFY=1\n") {
		t.Error("a compiled-in key does not produce SPORED_SIG_VERIFY=1")
	}
	if strings.Contains(on, "SPORED_SIG_VERIFY=0") {
		t.Error("emits SPORED_SIG_VERIFY=0 as well as =1; the later assignment would win")
	}
	if !strings.Contains(on, "TESTKEY") || !strings.Contains(on, SigningKeyPath) {
		t.Error("does not write the key to the path the installer verifies against")
	}

	SigningKeyPEM = ""
	off := KeySetupScript()
	if !strings.Contains(off, "SPORED_SIG_VERIFY=0") {
		t.Error("an absent key does not produce SPORED_SIG_VERIFY=0")
	}
	if strings.Contains(off, "SPORED_SIG_VERIFY=1") {
		t.Error("enables verification with no key compiled in")
	}
	if strings.Contains(off, "EOFSPOREDPUBKEY") {
		t.Error("writes a key heredoc with no key compiled in")
	}
}
