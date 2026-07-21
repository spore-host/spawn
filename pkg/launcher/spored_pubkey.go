package launcher

// sporedSigningPublicKeyPEM is the spore.host public key used to verify the
// authenticity of spored binaries at instance boot. It is the public half of a
// KMS asymmetric signing key (ECDSA_SHA_256) held in the spore.host infra
// account; the release pipeline signs each spored binary with the private key
// (via kms:Sign, which never exports it), and the generated bootstrap verifies
// the downloaded binary's signature against THIS key with openssl before running
// it. Because the key is compiled into spawn — which is itself trusted (Homebrew
// tap / signed GitHub release) — the trust root lives in the spawn binary, not in
// the S3 bucket the binary is served from. That's what closes the
// same-bucket-checksum gap (spore-host#440): an attacker who rewrites the bucket
// cannot forge a signature without this key.
//
// Rotation: publish a new key, ship a spawn release embedding it (this const),
// and re-sign binaries. Old spawn releases keep verifying against the old key
// until upgraded; keep signing with both during a rotation window.
//
// NOTE: until the KMS key is provisioned and this is populated with the real
// public key, signatureVerificationEnabled() returns false and the bootstrap
// falls back to sha256-only verification with a logged warning (no false
// "signature verified" claim). Populating this constant + the release signing
// step flips verification on.
// A var (not const) so tests can exercise the enabled path and so a rotation can
// be applied in one place. Production value is set here at build time.
var sporedSigningPublicKeyPEM = ``

// signatureVerificationEnabled reports whether a real signing public key is
// compiled in. When false, the bootstrap uses sha256-only verification (honestly
// logged as corruption-only) rather than asserting authenticity it can't check.
func signatureVerificationEnabled() bool {
	return len(sporedSigningPublicKeyPEM) > 0
}
