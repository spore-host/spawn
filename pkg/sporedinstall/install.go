// Package sporedinstall emits the one shell implementation of "download spored
// and install it", so that every path which puts a root daemon on an instance
// verifies it the same way.
//
// Why a package rather than three similar snippets. spored runs as root and is
// installed by curl-to-root from an S3 bucket. The boot path
// (pkg/launcher/bootstrap.go) got this right: it probes for a signature before
// committing to a source, requires a sha256, verifies a detached signature
// against a key compiled into spawn rather than one served from the same bucket,
// and fails closed at every step (spore-host#440). Two other paths did not, and
// nobody noticed because each was written by copying the shape of the download
// rather than the shape of the verification (GHSA-3x4q-99gh-fxhx, #819):
//
//   - cmd/app.go, twice — no checksum, no signature, and `mv … || true`, so even
//     the install failing was swallowed. This is the user-data of every
//     `spawn app launch`.
//   - cmd/upgrade_spored.go — sha256 only, and fail-OPEN: a missing .sha256
//     downgraded to no verification with a warning on stderr, which on a
//     non-interactive upgrade nobody reads.
//
// A missing checksum is not a reason to skip verification; it is a reason to
// stop. An attacker who can write the bucket can equally delete a .sha256, so
// "verify if a checksum happens to be published" is verification an attacker
// opts you out of.
package sporedinstall

import (
	"fmt"
	"strings"
)

// Dest is where spored is installed on the instance. Shared so the three call
// sites cannot disagree about it.
const Dest = "/usr/local/bin/spored"

// SigningKeyPath is where the bootstrap writes the compiled-in public key.
const SigningKeyPath = "/etc/spawn/spored-signing-key.pem"

// SigningKeyPEM is the spore.host public key used to verify the authenticity of
// spored binaries. It is the public half of a KMS asymmetric signing key
// (ECDSA_SHA_256, ECC_NIST_P256) held in the spore.host infra account
// (alias/spored-signing,
// arn:aws:kms:us-east-1:966362334030:key/7b28df72-c92f-4d0c-97e3-7696e04bafd3);
// the release pipeline signs each spored binary with the private half via
// kms:Sign, which never exports it.
//
// Because the key is compiled into spawn — itself trusted, via the Homebrew tap
// or a signed GitHub release — the trust root lives in the spawn binary and not
// in the bucket the binary is served from. That is what closes the
// same-bucket-checksum gap (spore-host#440): an attacker who rewrites the bucket
// cannot forge a signature without this key, and a sha256 served from the
// rewritten bucket proves only that the download was not corrupted in transit.
//
// Rotation: publish a new key, ship a spawn release embedding it here, and
// re-sign. Older spawn releases keep verifying against the old key until
// upgraded, so keep signing with both during a rotation window.
//
// A var rather than a const so tests can exercise both the enabled and disabled
// paths, and so a rotation is a one-line change. Empty disables signature
// verification; the generated script then says so rather than implying an
// authenticity check it cannot perform.
var SigningKeyPEM = `-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEOZo4nbX52lIw/vbPQdawhyXrhG+c
AtY5UKek+Y9PGbBHyqAukxdMoE5hc28ZvbcDIRiSussHivyki5gr8/QlVA==
-----END PUBLIC KEY-----`

// VerificationEnabled reports whether a real signing public key is compiled in.
// When false the generated script verifies the sha256 only, and logs that it is
// checking corruption rather than authenticity.
func VerificationEnabled() bool { return len(SigningKeyPEM) > 0 }

// KeySetupScript emits the shell that makes the compiled-in key available to
// VerifiedInstallFunc, and sets SPORED_SIG_VERIFY accordingly. Every caller of
// VerifiedInstallFunc must emit this first; with SPORED_SIG_VERIFY unset the
// function treats verification as off, which is the honest default but not the
// intended one.
func KeySetupScript() string {
	if !VerificationEnabled() {
		return "\n# No spored signing key compiled into this spawn build.\nSPORED_SIG_VERIFY=0\n"
	}
	return fmt.Sprintf(`
# spored signing public key (spore.host). The installer verifies the downloaded
# binary's detached .sig against this before it is ever made executable.
SPORED_SIG_VERIFY=1
mkdir -p %s
cat > %s <<'EOFSPOREDPUBKEY'
%s
EOFSPOREDPUBKEY
`, dirOf(SigningKeyPath), SigningKeyPath, SigningKeyPEM)
}

func dirOf(p string) string {
	if i := strings.LastIndex(p, "/"); i > 0 {
		return p[:i]
	}
	return "/"
}

// FuncName is the shell function VerifiedInstallFunc defines. Exported so gates
// can assert that a script which downloads spored actually calls it.
const FuncName = "spore_install_verified_spored"

// VerifiedInstallFunc returns the definition of a shell function that downloads
// and installs spored, failing closed at every step.
//
//	spore_install_verified_spored <binary-name> <base-url> [base-url ...]
//
// Bases are tried in order. A base is only usable if it carries the binary AND,
// when SPORED_SIG_VERIFY=1, a matching .sig — probed with a HEAD before the
// large download, so a stale unsigned regional bucket falls through to the next
// candidate instead of committing the install to it and hard-failing later
// (#440). A sha256 is always required. The binary is made executable only after
// every check has passed, and installed by atomic rename so an already-running
// spored cannot cause ETXTBSY (#27).
//
// It returns non-zero on any failure and leaves the existing binary untouched,
// so a caller may keep running whatever is already installed — but must not
// treat the failure as success. The old `|| true` in cmd/app.go is exactly the
// shape to avoid.
func VerifiedInstallFunc() string {
	return fmt.Sprintf(`
# --- spored verified install (generated by pkg/sporedinstall) ---------------
# Usage: %[1]s <binary-name> <base-url> [base-url ...]
# Fails closed: a missing checksum, a missing signature (when verification is
# on), or any mismatch aborts without installing anything. Do NOT append
# "|| true" to a call site — that is the defect this function replaced.
%[1]s() {
    local binary="$1"; shift
    if [ -z "$binary" ] || [ $# -eq 0 ]; then
        echo "%[1]s: need a binary name and at least one base URL" >&2
        return 1
    fi

    local tmp base bin_url sig_url chosen=""
    tmp="$(mktemp /tmp/spored.XXXXXX)" || return 1

    for base in "$@"; do
        bin_url="${base}/${binary}"
        sig_url="${base}/${binary}.sig"
        if [ "${SPORED_SIG_VERIFY:-0}" = "1" ]; then
            if ! curl -fsI --retry 3 --retry-delay 1 --retry-all-errors "$sig_url" >/dev/null 2>&1; then
                echo "No signature at ${base} — trying next source"
                continue
            fi
        fi
        if curl -f --retry 5 --retry-delay 2 --retry-all-errors -o "$tmp" "$bin_url" 2>/dev/null; then
            chosen="$base"
            echo "Downloaded spored from ${base}"
            break
        fi
    done

    if [ -z "$chosen" ]; then
        echo "Failed to download ${binary} from any source" >&2
        if [ "${SPORED_SIG_VERIFY:-0}" = "1" ]; then
            echo "  (signature verification is on — a source without a .sig is skipped)" >&2
        fi
        rm -f "$tmp"
        return 1
    fi

    # Checksum is REQUIRED, not best-effort. It only proves the download wasn't
    # corrupted — it is served from the same bucket as the binary — but a missing
    # one means something is wrong with the source, and treating "no checksum" as
    # "no check needed" lets an attacker who can delete objects opt us out of
    # verification entirely. That was the fail-open in cmd/upgrade_spored.go.
    echo "Verifying checksum..."
    if ! curl -f --retry 5 --retry-delay 2 --retry-all-errors -o "${tmp}.sha256" "${chosen}/${binary}.sha256" 2>/dev/null; then
        echo "No checksum published at ${chosen}/${binary}.sha256 — refusing to install an unverified spored" >&2
        rm -f "$tmp" "${tmp}.sha256"
        return 1
    fi
    local expected actual
    expected="$(awk '{print $1}' "${tmp}.sha256")"
    actual="$(sha256sum "$tmp" | awk '{print $1}')"
    if [ -z "$expected" ] || [ "$expected" != "$actual" ]; then
        echo "Checksum verification FAILED (expected ${expected:-<empty>}, got ${actual})" >&2
        rm -f "$tmp" "${tmp}.sha256"
        return 1
    fi
    rm -f "${tmp}.sha256"
    echo "Checksum verified: $actual"

    # Signature proves AUTHENTICITY, which the checksum cannot: the key comes
    # from spawn, not from the bucket, so a bucket compromise can't forge it.
    if [ "${SPORED_SIG_VERIFY:-0}" = "1" ]; then
        echo "Verifying signature..."
        if ! curl -f --retry 5 --retry-delay 2 --retry-all-errors -o "${tmp}.sig" "${chosen}/${binary}.sig" 2>/dev/null; then
            echo "Signature not found at ${chosen}/${binary}.sig — refusing to run an unsigned binary" >&2
            rm -f "$tmp" "${tmp}.sig"
            return 1
        fi
        # The .sig is base64-encoded DER (ECDSA_SHA_256) as emitted by the release
        # signer; decode to raw DER for openssl, tolerating an already-binary sig.
        # Read from stdin rather than as a positional argument: GNU coreutils
        # accepts "base64 -d FILE", BSD/macOS base64 does not, and redirection
        # works on both. Not academic — it is how this was first tested.
        if ! base64 -d < "${tmp}.sig" > "${tmp}.sig.der" 2>/dev/null; then
            cp "${tmp}.sig" "${tmp}.sig.der"
        fi
        if ! openssl dgst -sha256 -verify %[2]s -signature "${tmp}.sig.der" "$tmp" >/dev/null 2>&1; then
            echo "Signature verification FAILED — refusing to run spored" >&2
            rm -f "$tmp" "${tmp}.sig" "${tmp}.sig.der"
            return 1
        fi
        rm -f "${tmp}.sig" "${tmp}.sig.der"
        echo "Signature verified (spore.host)"
    else
        echo "Signature verification not enabled in this spawn build; checksum only (corruption, not authenticity)"
    fi

    # Executable bit goes on only after every check has passed.
    chmod +x "$tmp" || { rm -f "$tmp"; return 1; }

    # Stop any running spored so we don't leave a stale daemon, then install by
    # atomic rename — permitted over a busy binary, because the running process
    # keeps the old unlinked inode, so this never hits ETXTBSY (#27).
    systemctl stop spored 2>/dev/null || true
    if ! mv -f "$tmp" %[3]s; then
        echo "Failed to install spored to %[3]s" >&2
        rm -f "$tmp"
        return 1
    fi
    return 0
}
# --- end spored verified install -------------------------------------------
`, FuncName, SigningKeyPath, Dest)
}
