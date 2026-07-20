package plugin

import (
	"testing"

	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/testing/ca"
)

// validReleaseSAN matches officialSigSANRegex (the pinned release-workflow
// identity). issuer must equal officialSigIssuer.
const (
	validReleaseSAN = "https://github.com/spore-host/spore-plugins/.github/workflows/release.yml@refs/tags/tailscale-v1.2.0"
	wrongRepoSAN    = "https://github.com/attacker/spore-plugins/.github/workflows/release.yml@refs/tags/tailscale-v1.2.0"
)

func TestVerifySignedEntity(t *testing.T) {
	// The virtual Sigstore doesn't issue Fulcio SCTs; drop only that leg.
	orig := requireSCT
	requireSCT = false
	t.Cleanup(func() { requireSCT = orig })

	vs, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatalf("NewVirtualSigstore: %v", err)
	}
	manifest := []byte(`{"schema_version":1,"plugin":"tailscale","version":"v1.2.0","files":{"plugin.yaml":"abc"}}`)

	t.Run("valid signature + matching identity passes", func(t *testing.T) {
		entity, err := vs.Sign(validReleaseSAN, officialSigIssuer, manifest)
		if err != nil {
			t.Fatalf("Sign: %v", err)
		}
		if err := verifySignedEntity(entity, vs, manifest); err != nil {
			t.Errorf("expected verification to pass, got: %v", err)
		}
	})

	t.Run("wrong repo identity is rejected", func(t *testing.T) {
		entity, err := vs.Sign(wrongRepoSAN, officialSigIssuer, manifest)
		if err != nil {
			t.Fatalf("Sign: %v", err)
		}
		if err := verifySignedEntity(entity, vs, manifest); err == nil {
			t.Error("expected rejection for a signature from the wrong repo, got nil")
		}
	})

	t.Run("wrong issuer is rejected", func(t *testing.T) {
		entity, err := vs.Sign(validReleaseSAN, "https://accounts.google.com", manifest)
		if err != nil {
			t.Fatalf("Sign: %v", err)
		}
		if err := verifySignedEntity(entity, vs, manifest); err == nil {
			t.Error("expected rejection for a non-GitHub-Actions issuer, got nil")
		}
	})

	t.Run("tampered manifest is rejected", func(t *testing.T) {
		entity, err := vs.Sign(validReleaseSAN, officialSigIssuer, manifest)
		if err != nil {
			t.Fatalf("Sign: %v", err)
		}
		tampered := []byte(`{"schema_version":1,"plugin":"tailscale","version":"v1.2.0","files":{"plugin.yaml":"DIFFERENT"}}`)
		if err := verifySignedEntity(entity, vs, tampered); err == nil {
			t.Error("expected rejection when the artifact differs from what was signed, got nil")
		}
	})
}

// TestTrustedRootFetcherOverride confirms the fetcher hook is overridable, so
// verifyManifestSignature never touches the network in tests. (The JSON-bundle
// unmarshal is sigstore-go's own tested code; the verification policy it feeds
// is covered by TestVerifySignedEntity.)
func TestTrustedRootFetcherOverride(t *testing.T) {
	vs, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatalf("NewVirtualSigstore: %v", err)
	}
	orig := trustedRootFetcher
	trustedRootFetcher = func() (root.TrustedMaterial, error) { return vs, nil }
	t.Cleanup(func() { trustedRootFetcher = orig })

	got, err := trustedRootFetcher()
	if err != nil || got == nil {
		t.Fatalf("override fetcher returned (%v, %v)", got, err)
	}
}
