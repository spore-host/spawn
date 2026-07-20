package plugin

import (
	"bytes"
	"fmt"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// SignatureFileName is the cosign/sigstore bundle asset's filename in a plugin
// release. cosign sign-blob --bundle writes a JSON Sigstore bundle here.
const SignatureFileName = "manifest.json.sigstore.json"

// Signing identity for official plugin releases. The manifest is signed keyless
// in the spore-plugins release workflow via GitHub Actions OIDC, so the Fulcio
// certificate's issuer is GitHub's OIDC provider and its SAN is the release
// workflow's identity. We pin BOTH so a signature is only trusted if it was
// produced by that exact workflow in that exact repo — a signature from any
// other repo or workflow (even a valid Sigstore signature) is rejected.
const (
	// officialSigIssuer is the OIDC issuer for GitHub Actions tokens.
	officialSigIssuer = "https://token.actions.githubusercontent.com"
	// officialSigSANRegex matches the release workflow's SAN identity across any
	// tag ref: https://github.com/spore-host/spore-plugins/.github/workflows/release.yml@refs/tags/<tag>
	// Anchored to the spore-host/spore-plugins repo + release.yml workflow.
	officialSigSANRegex = `^https://github\.com/spore-host/spore-plugins/\.github/workflows/release\.yml@refs/tags/.+$`
)

// trustedRootFetcher fetches the Sigstore public-good trusted root (Fulcio CAs,
// Rekor logs, CT logs). Overridable in tests to inject a virtual Sigstore. It is
// a variable, not a direct call, so tests never touch the network.
var trustedRootFetcher = func() (root.TrustedMaterial, error) {
	tr, err := root.FetchTrustedRoot()
	if err != nil {
		return nil, err
	}
	return tr, nil
}

// verifyManifestSignature verifies a Sigstore bundle (sigBundleJSON) over the
// manifest bytes, requiring keyless provenance from the official spore-plugins
// release workflow: a Fulcio-issued certificate whose OIDC issuer is GitHub
// Actions and whose SAN matches the release workflow, plus Rekor transparency-log
// inclusion and a trusted timestamp. Any failure — bad signature, wrong identity,
// missing log entry, digest mismatch against manifestData — is an error.
func verifyManifestSignature(sigBundleJSON, manifestData []byte) error {
	var b bundle.Bundle
	if err := b.UnmarshalJSON(sigBundleJSON); err != nil {
		return fmt.Errorf("parse signature bundle: %w", err)
	}
	trustedMaterial, err := trustedRootFetcher()
	if err != nil {
		return fmt.Errorf("fetch sigstore trusted root: %w", err)
	}
	return verifySignedEntity(&b, trustedMaterial, manifestData)
}

// requireSCT gates the Certificate-Transparency-log (SCT) requirement. Real
// Fulcio certificates embed an SCT, so production verification requires one;
// the test virtual Sigstore doesn't issue SCTs, so tests set this false. It's
// the ONLY verification leg relaxed for tests — Rekor inclusion, a trusted
// timestamp, the signature itself, and the identity policy always apply.
var requireSCT = true

// verifySignedEntity runs the keyless verification policy against an already
// loaded entity + trusted material. Split from verifyManifestSignature so tests
// can drive it with a virtual Sigstore (which provides both the SignedEntity and
// the matching TrustedMaterial) without touching the network or bundle JSON.
func verifySignedEntity(entity verify.SignedEntity, trustedMaterial root.TrustedMaterial, manifestData []byte) error {
	opts := []verify.VerifierOption{
		verify.WithTransparencyLog(1),    // Rekor inclusion
		verify.WithObserverTimestamps(1), // a trusted timestamp
	}
	if requireSCT {
		opts = append(opts, verify.WithSignedCertificateTimestamps(1)) // CT-log proof the cert was logged
	}
	sev, err := verify.NewVerifier(trustedMaterial, opts...)
	if err != nil {
		return fmt.Errorf("build signature verifier: %w", err)
	}

	certID, err := verify.NewShortCertificateIdentity(officialSigIssuer, "", "", officialSigSANRegex)
	if err != nil {
		return fmt.Errorf("build certificate identity policy: %w", err)
	}

	if _, err := sev.Verify(entity, verify.NewPolicy(
		verify.WithArtifact(bytes.NewReader(manifestData)),
		verify.WithCertificateIdentity(certID),
	)); err != nil {
		return fmt.Errorf("signature verification failed: %w", err)
	}
	return nil
}
