#!/usr/bin/env bash
# Verify, before a release publishes anything, that the AWS CLI v2 zips this
# build pins are what the AWS CLI team signed. Run by
# .github/workflows/release.yaml before GoReleaser, from the tagged checkout:
#
#   scripts/verify-awscli-pin.sh
#
# Why: stock Ubuntu has no aws CLI, so the Linux bootstrap's spawn_need_aws
# downloads the pinned zip from awscli.amazonaws.com and installs it only if
# it has the pinned sha256. A pin that is not the sha256 of a validly signed
# zip would make every such boot fail closed, or install bytes nobody signed.
# The weekly bump (.github/workflows/awscli-pin.yml, #1495) checks the same
# when it moves the pin; this re-checks the pin the tag actually carries.
#
# What is checked is exactly what this build pins: `spawn awscli-pin` prints
# <arch> <version> <sha256> per architecture. Each zip is downloaded from
# awscli.amazonaws.com with its detached .sig; the signature must verify
# against the AWS CLI team key committed in scripts/awsclipin/aws-cli-team.asc,
# made by exactly the fingerprint below, and the zip must have the pinned
# sha256. Anything else fails the step.
#
# Nothing is uploaded: spawn does not mirror third-party artifacts (#1515;
# the #1494 regional mirror is retired). Instances fetch the zip from
# awscli.amazonaws.com, directly or through the spawn egress proxy.
#
# Needs: go, curl, gpg, sha256sum (or shasum). No AWS credentials. Writes only
# under $AWSCLI_VERIFY_DIR (outside the checkout: GoReleaser refuses a dirty
# tree), which it removes, and a throwaway GNUPGHOME.
set -euo pipefail

readonly AWSCLI_KEY_FPR=FB5DB77FD5C118B80511ADA8A6310ACC4672475C
readonly KEY_FILE=scripts/awsclipin/aws-cli-team.asc
readonly ORIGIN=https://awscli.amazonaws.com

out=${AWSCLI_VERIFY_DIR:?AWSCLI_VERIFY_DIR must name a directory outside the checkout}
mkdir -p "$out"

sha256() { if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d ' ' -f 1; }

# The pins, from this build. A malformed line fails here.
pins=$(SPORE_NO_UPDATE_CHECK=1 CGO_ENABLED=0 go run . awscli-pin)
n=0
while read -r arch version sum extra; do
  if ! [[ $arch =~ ^(x86_64|aarch64)$ && $version =~ ^2\.[0-9]+\.[0-9]+$ && $sum =~ ^[0-9a-f]{64}$ ]] || [ -n "$extra" ]; then
    echo "::error::spawn awscli-pin printed an unexpected line: $arch $version $sum $extra" >&2
    exit 1
  fi
  n=$((n + 1))
done <<<"$pins"
if [ "$n" -eq 0 ]; then
  echo "::error::spawn awscli-pin printed no pins" >&2
  exit 1
fi

# 1. The key: exactly one primary key, the pinned one.
export GNUPGHOME
GNUPGHOME=$(mktemp -d)
trap 'gpgconf --kill all >/dev/null 2>&1 || true; rm -rf "$GNUPGHOME" "$out"' EXIT
chmod 700 "$GNUPGHOME"
gpg --batch --quiet --import "$KEY_FILE"
fprs=$(gpg --batch --with-colons --fixed-list-mode --list-keys |
  awk -F: '$1 == "pub" { want = 1; next } want && $1 == "fpr" { print $10; want = 0 }')
if [ "$fprs" != "$AWSCLI_KEY_FPR" ]; then
  echo "::error::$KEY_FILE holds primary key(s) [$fprs], not exactly $AWSCLI_KEY_FPR" >&2
  exit 1
fi

# 2. Each zip: signature by that key, then the pinned sha256.
while read -r arch version sum; do
  name=awscli-exe-linux-$arch-$version.zip
  rm -f "$out/$name" "$out/$name.sig"
  if ! curl -fsS --retry 3 --max-time 900 -o "$out/$name" "$ORIGIN/$name" ||
    ! curl -fsS --retry 3 --max-time 60 -o "$out/$name.sig" "$ORIGIN/$name.sig"; then
    echo "::error::could not download $ORIGIN/$name and its .sig" >&2
    exit 1
  fi
  # A good signature is not enough: it must be by the pinned key, whose
  # primary fingerprint is VALIDSIG's last field.
  status=$(gpg --batch --status-fd 1 --verify "$out/$name.sig" "$out/$name" 2>"$out/gpg.err") || {
    cat "$out/gpg.err" >&2
    echo "::error::$ORIGIN/$name: signature does not verify against $AWSCLI_KEY_FPR" >&2
    exit 1
  }
  if ! awk -v fpr="$AWSCLI_KEY_FPR" '$1 == "[GNUPG:]" && $2 == "VALIDSIG" && $NF == fpr { ok = 1 } END { exit !ok }' <<<"$status"; then
    echo "::error::$ORIGIN/$name: no VALIDSIG by $AWSCLI_KEY_FPR in gpg's status output" >&2
    exit 1
  fi
  if [ "$(sha256 "$out/$name")" != "$sum" ]; then
    echo "::error::$ORIGIN/$name has a valid signature but not the sha256 this build pins ($sum); the bootstrap would refuse it" >&2
    exit 1
  fi
  echo "$arch: AWS CLI $version signature verified, sha256 $sum"
done <<<"$pins"
echo "✅ Verified the pinned AWS CLI ($n zips) at $ORIGIN"
