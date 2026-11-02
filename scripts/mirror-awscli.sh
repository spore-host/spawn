#!/usr/bin/env bash
# Mirror the AWS CLI v2 zips the Linux bootstrap installs on an image with no
# aws CLI into every spawn-binaries-<region> bucket (#1494). Run by
# .github/workflows/release.yaml before GoReleaser, from the tagged checkout:
#
#   scripts/mirror-awscli.sh publish   download, verify, upload, prune
#   scripts/mirror-awscli.sh verify    read every mirrored zip back and check it,
#                                      then remove the local copies
#
# Why: stock Ubuntu has no aws CLI, so spawn_need_aws installs the pinned one,
# and an instance in a private subnet with only VPC endpoints (what
# --no-public-ip is for) cannot reach awscli.amazonaws.com. It can reach its
# own region's spawn-binaries bucket through the S3 endpoint --no-public-ip
# already requires, so the bootstrap tries <bucket>/<mirror key> first.
#
# What is mirrored is exactly what this build pins: `spawn awscli-pin` prints
# <arch> <version> <sha256> <mirror key> per architecture. Each zip is
# downloaded from awscli.amazonaws.com with its detached .sig, the signature
# must verify against the AWS CLI team key committed in
# scripts/awsclipin/aws-cli-team.asc, made by exactly the fingerprint below,
# and the zip must also have the pinned sha256. Anything else fails the step
# before a byte is uploaded. The instance checks the same sha256 whichever
# source it uses, so the mirror adds a source, not a trust root.
#
# The deleter: publish keeps, per bucket, the pinned version and the newest
# KEEP-1 others under spawn/awscli/ (a spawn release a few pins old still finds
# its zip), and deletes the older ones. It touches only keys of the mirror's
# own name form.
#
# Needs: go, curl, gpg, aws, sha256sum (or shasum), cmp. verify needs no AWS
# credentials: it reads over anonymous HTTPS, as an instance does. Writes only under
# $AWSCLI_MIRROR_DIR (outside the checkout: GoReleaser refuses a dirty tree)
# and a throwaway GNUPGHOME. Env: SPAWN_BINARY_BUCKETS, AWSCLI_MIRROR_DIR.
set -euo pipefail

readonly AWSCLI_KEY_FPR=FB5DB77FD5C118B80511ADA8A6310ACC4672475C
readonly KEY_FILE=scripts/awsclipin/aws-cli-team.asc
readonly ORIGIN=https://awscli.amazonaws.com
readonly MIRROR_DIR=spawn/awscli
readonly KEEP=3

mode=${1:-}
case $mode in publish | verify) ;; *)
  echo "usage: $0 publish|verify" >&2
  exit 2
  ;;
esac
out=${AWSCLI_MIRROR_DIR:?AWSCLI_MIRROR_DIR must name a directory outside the checkout}
read -ra BUCKETS <<<"${SPAWN_BINARY_BUCKETS:-}"
if [ "${#BUCKETS[@]}" -eq 0 ]; then
  echo "::error::SPAWN_BINARY_BUCKETS is empty" >&2
  exit 1
fi
mkdir -p "$out"

sha256() { if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d ' ' -f 1; }
region_of() { echo "${1#spawn-binaries-}"; }

# The pins, from this build. A malformed line fails here, not as a wrong key.
pins=$(SPORE_NO_UPDATE_CHECK=1 CGO_ENABLED=0 go run . awscli-pin)
n=0
while read -r arch version sum key; do
  if ! [[ $arch =~ ^(x86_64|aarch64)$ && $version =~ ^2\.[0-9]+\.[0-9]+$ && $sum =~ ^[0-9a-f]{64}$ ]] ||
    [ "$key" != "$MIRROR_DIR/awscli-exe-linux-$arch-$version.zip" ]; then
    echo "::error::spawn awscli-pin printed an unexpected line: $arch $version $sum $key" >&2
    exit 1
  fi
  n=$((n + 1))
done <<<"$pins"
if [ "$n" -eq 0 ]; then
  echo "::error::spawn awscli-pin printed no pins" >&2
  exit 1
fi

if [ "$mode" = verify ]; then
  # Every bucket, every pinned zip, fetched as an instance fetches it:
  # anonymous HTTPS at the regional S3 name (#1436). It must have the pinned
  # sha256 and be the bytes publish verified.
  back="$out/readback"
  bad=0
  for bucket in "${BUCKETS[@]}"; do
    region=$(region_of "$bucket")
    while read -r arch version sum key; do
      rm -f "$back"
      if ! curl -fsS --retry 3 --max-time 900 -o "$back" "https://$bucket.s3.$region.amazonaws.com/$key"; then
        echo "::error::s3://$bucket/$key cannot be fetched anonymously, as instances fetch it (#1494)"
        bad=1
        continue
      fi
      if [ "$(sha256 "$back")" != "$sum" ] || ! cmp -s "$back" "$out/${key##*/}"; then
        echo "::error::s3://$bucket/$key is not the pinned AWS CLI $version $arch zip (#1494)"
        bad=1
      fi
    done <<<"$pins"
  done
  rm -f "$back"
  [ "$bad" = 0 ] || exit 1
  rm -rf "$out"
  echo "✅ Verified the AWS CLI mirror ($n zips) in ${#BUCKETS[@]} regions"
  exit 0
fi

# publish. 1. The key: exactly one primary key, the pinned one.
export GNUPGHOME
GNUPGHOME=$(mktemp -d)
trap 'gpgconf --kill all >/dev/null 2>&1 || true; rm -rf "$GNUPGHOME"' EXIT
chmod 700 "$GNUPGHOME"
gpg --batch --quiet --import "$KEY_FILE"
fprs=$(gpg --batch --with-colons --fixed-list-mode --list-keys |
  awk -F: '$1 == "pub" { want = 1; next } want && $1 == "fpr" { print $10; want = 0 }')
if [ "$fprs" != "$AWSCLI_KEY_FPR" ]; then
  echo "::error::$KEY_FILE holds primary key(s) [$fprs], not exactly $AWSCLI_KEY_FPR" >&2
  exit 1
fi

# 2. Each zip: signature by that key, then the pinned sha256.
while read -r arch version sum key; do
  name=${key##*/}
  rm -f "$out/$name" "$out/$name.sig"
  if ! curl -fsS --retry 3 --max-time 900 -o "$out/$name" "$ORIGIN/$name" ||
    ! curl -fsS --retry 3 --max-time 60 -o "$out/$name.sig" "$ORIGIN/$name.sig"; then
    echo "::error::could not download $ORIGIN/$name and its .sig (#1494)" >&2
    exit 1
  fi
  # A good signature is not enough: it must be by the pinned key, whose
  # primary fingerprint is VALIDSIG's last field.
  status=$(gpg --batch --status-fd 1 --verify "$out/$name.sig" "$out/$name" 2>"$out/gpg.err") || {
    cat "$out/gpg.err" >&2
    echo "::error::$ORIGIN/$name: signature does not verify against $AWSCLI_KEY_FPR (#1494)" >&2
    exit 1
  }
  if ! awk -v fpr="$AWSCLI_KEY_FPR" '$1 == "[GNUPG:]" && $2 == "VALIDSIG" && $NF == fpr { ok = 1 } END { exit !ok }' <<<"$status"; then
    echo "::error::$ORIGIN/$name: no VALIDSIG by $AWSCLI_KEY_FPR in gpg's status output (#1494)" >&2
    exit 1
  fi
  if [ "$(sha256 "$out/$name")" != "$sum" ]; then
    echo "::error::$ORIGIN/$name has a valid signature but not the sha256 this build pins ($sum); the bootstrap would refuse it (#1494)" >&2
    exit 1
  fi
  echo "$arch: AWS CLI $version signature verified, sha256 $sum"
done <<<"$pins"

# 3. Upload every verified zip to every bucket. Any failed copy fails xargs.
# shellcheck disable=SC2016 # $1/$2/$3 expand in xargs' bash, not here
for bucket in "${BUCKETS[@]}"; do
  while read -r _ _ _ key; do
    printf '%s\0%s\0%s\0' "$out/${key##*/}" "s3://$bucket/$key" "$(region_of "$bucket")"
  done <<<"$pins"
done | xargs -0 -P 8 -n 3 bash -c 'aws s3 cp "$1" "$2" --region "$3" --only-show-errors' _
echo "✅ Mirrored the AWS CLI ($n zips) to ${#BUCKETS[@]} regions"

# 4. The deleter: keep the pinned version and the newest KEEP-1 others.
# mirror_version KEY prints KEY's AWS CLI version when KEY has the mirror's own
# name form, and nothing otherwise: a key of any other form is not ours.
mirror_version() { sed -nE "s#^$MIRROR_DIR/awscli-exe-linux-(x86_64|aarch64)-(2\.[0-9]+\.[0-9]+)\.zip\$#\2#p" <<<"$1"; }
pinned=$(awk '{ print $2; exit }' <<<"$pins")
for bucket in "${BUCKETS[@]}"; do
  region=$(region_of "$bucket")
  keys=$(aws s3api list-objects-v2 --bucket "$bucket" --prefix "$MIRROR_DIR/" --region "$region" --query 'Contents[].Key' --output text)
  [ "$keys" = None ] && keys=""
  versions=$(for k in $keys; do mirror_version "$k"; done | sort -uV)
  keep=$({ echo "$pinned"; grep -vxF "$pinned" <<<"$versions" | tail -n $((KEEP - 1)) || true; } | sort -uV)
  for k in $keys; do
    v=$(mirror_version "$k")
    [ -n "$v" ] || continue
    if ! grep -qxF "$v" <<<"$keep"; then
      aws s3 rm --only-show-errors --region "$region" "s3://$bucket/$k"
      echo "pruned s3://$bucket/$k (keeping $(tr '\n' ' ' <<<"$keep"))"
    fi
  done
done
