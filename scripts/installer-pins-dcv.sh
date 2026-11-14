#!/usr/bin/env bash
# Verify the Amazon DCV release cmd/app.go installs (dcvVersion) and print its
# pins (#1620): the sha256 of each per-arch tarball and the primary
# fingerprints of the DCV signing key. Run weekly, on ubuntu-latest, by
# .github/workflows/installer-pins.yml; its PR job writes the pins with
# scripts/installerpin.
#
#   scripts/installer-pins-dcv.sh           only when dcvVersion is not pinned yet
#   scripts/installer-pins-dcv.sh --force   re-verify the pinned version too
#
# The bytes are trusted only once every RPM in both tarballs verifies, with
# rpmkeys -K in a throwaway rpm database, against the NICE-GPG-KEY downloaded
# alongside them: the same check the boot makes, done here once so the boot can
# also require the exact tarball and key. The key is fetched from the same
# origin as the tarballs, so a key change is called out in the PR (installerpin
# set) for a person to confirm with AWS before merging.
#
# Writes `changed`, `version`, `sha256_x86_64`, `sha256_aarch64` and
# `key_fingerprints` to $GITHUB_OUTPUT when set. Downloads into a temp
# directory outside the checkout and removes it. Needs curl, gpg, rpmkeys and
# rpmdb (the rpm package), tar, go and sha256sum (or shasum); calls no AWS API.
set -euo pipefail
shopt -s inherit_errexit

readonly ORIGIN=https://d1uj6qtbmh3dt5.cloudfront.net

cd "$(dirname "$0")/.."

force=false
case ${1:-} in
  '') ;;
  --force) force=true ;;
  *) echo "usage: $0 [--force]" >&2; exit 2 ;;
esac

output() { if [ -n "${GITHUB_OUTPUT:-}" ]; then echo "$1=$2" >>"$GITHUB_OUTPUT"; fi; }
output changed false

show=$(go -C scripts run ./installerpin show)
get() { sed -n "s/^$1=//p" <<<"$show"; }
version=$(get dcvVersion)
major_minor=$(get dcvVersionMajorMinor)
pinned=$(get dcvPinVersion)
if [ -z "$version" ] || [ -z "$major_minor" ]; then
  echo "::error::could not read dcvVersion / dcvVersionMajorMinor from cmd/app.go" >&2
  exit 1
fi
if [ "$pinned" = "$version" ] && [ "$force" = false ]; then
  echo "Amazon DCV $version is pinned already"
  exit 0
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
export GNUPGHOME="$work/gnupg"
mkdir -m 700 "$GNUPGHOME"

# 1. The key, and its primary fingerprints, computed exactly as the boot does
#    (cmd/app.go dcvInstallBlockWith; TestDCVPinScriptAgreesWithTheBoot).
curl -fsS --retry 3 -o "$work/NICE-GPG-KEY" "$ORIGIN/NICE-GPG-KEY"
fprs=$(gpg --batch --show-keys --with-colons "$work/NICE-GPG-KEY" 2>/dev/null | awk -F: '$1 == "pub" { p = 1; next } p && $1 == "fpr" { print $10; p = 0 }' | sort | paste -sd ' ' -)
if [ -z "$fprs" ]; then
  echo "::error::$ORIGIN/NICE-GPG-KEY holds no OpenPGP primary key" >&2
  exit 1
fi
echo "NICE-GPG-KEY primary fingerprints: $fprs"
db="$work/rpmdb"
mkdir -p "$db"
rpmdb --dbpath "$db" --initdb
rpmkeys --dbpath "$db" --import "$work/NICE-GPG-KEY"

# 2. Each tarball: every RPM verifies against that key, then the sha256.
sha256() { if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d ' ' -f 1; }
sum_x86_64='' sum_aarch64=''
for arch in x86_64 aarch64; do
  tgz="nice-dcv-$version-amzn2023-$arch.tgz"
  url="$ORIGIN/$major_minor/Servers/$tgz"
  code=$(curl -sS -o /dev/null -w '%{http_code}' -I "$url")
  if [ "$code" = 404 ] || [ "$code" = 403 ]; then
    echo "::error::$url is not published (HTTP $code): is dcvVersion $version right?" >&2
    exit 1
  fi
  curl -fsS --retry 3 --max-time 900 -o "$work/$tgz" "$url"
  mkdir -p "$work/$arch"
  tar -xzf "$work/$tgz" -C "$work/$arch" --strip-components=1
  shopt -s nullglob
  rpms=("$work/$arch"/*.rpm)
  shopt -u nullglob
  if [ "${#rpms[@]}" -eq 0 ]; then
    echo "::error::$tgz holds no RPMs" >&2
    exit 1
  fi
  server=false
  for rpm in "${rpms[@]}"; do
    case $(basename "$rpm") in nice-dcv-server-*) server=true ;; esac
    out=$(rpmkeys --dbpath "$db" -K "$rpm" 2>&1) || {
      echo "$out" >&2
      echo "::error::$tgz: $(basename "$rpm") does not verify against NICE-GPG-KEY ($fprs)" >&2
      exit 1
    }
    if ! grep -q 'digests signatures OK$' <<<"$out"; then
      echo "$out" >&2
      echo "::error::$tgz: $(basename "$rpm") is not signed by NICE-GPG-KEY ($fprs)" >&2
      exit 1
    fi
    echo "$arch: $out"
  done
  if [ "$server" = false ]; then
    echo "::error::$tgz holds no nice-dcv-server RPM: not a DCV server tarball" >&2
    exit 1
  fi
  h=$(sha256 "$work/$tgz")
  case $arch in x86_64) sum_x86_64=$h ;; aarch64) sum_aarch64=$h ;; esac
  echo "$arch: ${#rpms[@]} RPMs verified, $tgz sha256 $h"
done

output changed true
output version "$version"
output sha256_x86_64 "$sum_x86_64"
output sha256_aarch64 "$sum_aarch64"
output key_fingerprints "$fprs"
echo "verified Amazon DCV $version"
