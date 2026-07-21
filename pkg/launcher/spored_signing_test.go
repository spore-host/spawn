package launcher

import (
	"strings"
	"testing"
)

// TestBootstrap_SignatureDisabledByDefault: with no signing key compiled in, the
// bootstrap must NOT claim to verify a signature — it sets SPORED_SIG_VERIFY=0
// and keeps the sha256 (corruption) check. This is the honest interim state.
func TestBootstrap_SignatureDisabledByDefault(t *testing.T) {
	if signatureVerificationEnabled() {
		t.Skip("a signing key is compiled in; the disabled-path test doesn't apply")
	}
	script, err := BuildLinuxBootstrap(BootstrapConfig{Username: "ec2-user", PublicKey: []byte("ssh-ed25519 AAAA test")})
	if err != nil {
		t.Fatalf("BuildLinuxBootstrap: %v", err)
	}
	if !strings.Contains(script, "SPORED_SIG_VERIFY=0") {
		t.Error("expected SPORED_SIG_VERIFY=0 when no key is compiled in")
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
	orig := sporedSigningPublicKeyPEM
	sporedSigningPublicKeyPEM = "-----BEGIN PUBLIC KEY-----\nTESTKEY\n-----END PUBLIC KEY-----"
	t.Cleanup(func() { sporedSigningPublicKeyPEM = orig })

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
