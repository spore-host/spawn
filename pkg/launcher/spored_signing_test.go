package launcher

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/pem"
	"regexp"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/sporedinstall"
)

// sigVerifyOn matches the ASSIGNMENT that turns verification on, anchored at line
// start — not the bare identifier, which also appears in two shell comments
// inside the unconditionally-appended static body.
var sigVerifyOn = regexp.MustCompile(`(?m)^SPORED_SIG_VERIFY=1$`)

// The compiled-in signing key must be a real P-256 public key.
//
// This is the assertion whose absence made #813 possible. Emptying
// sporedinstall.SigningKeyPEM is a one-literal, cleanly-compiling edit that makes
// every launch run an UNVERIFIED spored — reopening the same-bucket-checksum gap
// #440 closed — and before this, both signature gates still passed: one matched a
// shell comment, the other injects its own test key and never reads production's.
//
// Parsing it, rather than just checking it is non-empty, is deliberate: a
// truncated or placeholder value would satisfy a length check while failing
// openssl at boot on every instance, which is the same silent-downgrade shape.
func TestProductionSigningKeyIsRealAndUsable(t *testing.T) {
	if strings.TrimSpace(sporedinstall.SigningKeyPEM) == "" {
		t.Fatal("no signing key is compiled in: every launch would download and run " +
			"spored unverified from the same bucket that serves its checksum (#440)")
	}
	if !sporedinstall.VerificationEnabled() {
		t.Fatal("sporedinstall.VerificationEnabled() is false with a key present")
	}

	block, _ := pem.Decode([]byte(sporedinstall.SigningKeyPEM))
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

// With no signing key compiled in, the bootstrap must not CLAIM to verify a
// signature: it sets SPORED_SIG_VERIFY=0 and keeps the sha256 corruption check.
//
// This used to open with `if sporedinstall.VerificationEnabled() { t.Skip(...) }`, and
// since a key IS compiled in it skipped on every run — permanently dead, while
// counting as coverage for the disabled path. sporedinstall.SigningKeyPEM is a var
// precisely so a test can exercise this path, so now it does.
func TestBootstrap_SignatureDisabledWhenNoKeyIsCompiledIn(t *testing.T) {
	restore := sporedinstall.SigningKeyPEM
	t.Cleanup(func() { sporedinstall.SigningKeyPEM = restore })
	sporedinstall.SigningKeyPEM = ""

	if sporedinstall.VerificationEnabled() {
		t.Fatal("sporedinstall.VerificationEnabled() is true with an empty key")
	}
	script, err := BuildLinuxBootstrap(BootstrapConfig{Username: "ec2-user", PublicKey: []byte("ssh-ed25519 AAAA test")})
	if err != nil {
		t.Fatalf("BuildLinuxBootstrap: %v", err)
	}
	if !strings.Contains(script, "SPORED_SIG_VERIFY=0") {
		t.Error("expected SPORED_SIG_VERIFY=0 when no key is compiled in")
	}
	// And the enabling assignment must be absent. Asserting only the presence of
	// "=0" would pass on a script that somehow emitted both.
	if sigVerifyOn.MatchString(script) {
		t.Error("SPORED_SIG_VERIFY=1 is assigned even though no key is compiled in")
	}
	// The key must not be WRITTEN (no heredoc) when none is compiled in. The
	// runtime-gated verify block in the static body may still reference the path,
	// but SPORED_SIG_VERIFY=0 means it never runs.
	if strings.Contains(script, "EOFSPOREDPUBKEY") {
		t.Error("must not write a signing key heredoc when no key is compiled in")
	}
	// The verify block is present (static body) but gated; the sha256 check stays.
	if !strings.Contains(script, "Checksum verified") {
		t.Error("sha256 corruption check must remain")
	}
}

// TestBootstrap_SignatureEnabled: with a key set, the bootstrap embeds it and the
// verify block downloads the .sig and runs openssl, failing closed on mismatch.
func TestBootstrap_SignatureEnabled(t *testing.T) {
	orig := sporedinstall.SigningKeyPEM
	sporedinstall.SigningKeyPEM = "-----BEGIN PUBLIC KEY-----\nTESTKEY\n-----END PUBLIC KEY-----"
	t.Cleanup(func() { sporedinstall.SigningKeyPEM = orig })

	script, err := BuildLinuxBootstrap(BootstrapConfig{Username: "ec2-user", PublicKey: []byte("ssh-ed25519 AAAA test")})
	if err != nil {
		t.Fatalf("BuildLinuxBootstrap: %v", err)
	}
	for _, want := range []string{
		"SPORED_SIG_VERIFY=1",
		"/etc/spawn/spored-signing-key.pem",
		"TESTKEY",                       // the embedded pubkey
		"openssl dgst -sha256 -verify",  // the verification call
		"Signature verification FAILED", // fail-closed message
		"refusing to run an unsigned binary",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("enabled bootstrap missing %q", want)
		}
	}
}
