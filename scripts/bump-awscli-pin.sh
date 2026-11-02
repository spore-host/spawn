#!/usr/bin/env bash
# Move the AWS CLI v2 release the Linux bootstrap installs on an image with no
# aws CLI (#1490) to the newest one, if it is newer (#1495). Run weekly by
# .github/workflows/awscli-pin.yml; it can be run by hand the same way.
#
#   scripts/bump-awscli-pin.sh              newest 2.x release, if newer than the pin
#   scripts/bump-awscli-pin.sh --version V  release V, if newer than the pin
#
# It never trusts the version source for the bytes: whatever version it picks,
# it pins a sha256 only for a zip whose detached signature gpg verifies against
# the AWS CLI team key committed in scripts/awsclipin/aws-cli-team.asc, made by
# exactly the fingerprint below. That key expires; this script fails while the
# key is expired or within 60 days of expiring, so the weekly run turns red in
# time to replace it (from the AWS CLI install guide), whether or not a new
# release exists.
#
# Version source: the 2.N.N tags of github.com/aws/aws-cli, read with
# `git ls-remote`. AWS tags every v2 release there as it publishes it, the read
# needs no token and no API rate limit, and the tags are the same list the
# public CHANGELOG.rst is built from. A tag that is newer than the published
# zip (the minutes between tagging and upload) is reported and skipped; the next
# run picks it up.
#
# Edits pkg/launcher/bootstrap.go and CHANGELOG.md through scripts/awsclipin
# (go/parser, so a renamed constant fails rather than moving nothing). Writes
# `bumped`, `from` and `version` to $GITHUB_OUTPUT when set. Needs git, curl,
# gpg, go and sha256sum (or shasum); calls no AWS API.
set -euo pipefail

readonly AWSCLI_KEY_FPR=FB5DB77FD5C118B80511ADA8A6310ACC4672475C
readonly KEY_FILE=scripts/awsclipin/aws-cli-team.asc
readonly WARN_DAYS=60

cd "$(dirname "$0")/.."

version=
if [ "${1:-}" = --version ]; then
  version=${2:?--version needs a value}
elif [ $# -gt 0 ]; then
  echo "usage: $0 [--version 2.N.N]" >&2
  exit 2
fi

output() { if [ -n "${GITHUB_OUTPUT:-}" ]; then echo "$1=$2" >>"$GITHUB_OUTPUT"; fi; }
output bumped false

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
export GNUPGHOME="$work/gnupg"
mkdir -m 700 "$GNUPGHOME"

# 1. The key: exactly one primary key, the pinned one, not near expiry.
gpg --batch --quiet --import "$KEY_FILE"
colons=$(gpg --batch --with-colons --fixed-list-mode --list-keys)
fprs=$(awk -F: '$1 == "pub" { want = 1; next } want && $1 == "fpr" { print $10; want = 0 }' <<<"$colons")
if [ "$fprs" != "$AWSCLI_KEY_FPR" ]; then
  echo "::error::$KEY_FILE holds primary key(s) [$fprs], not exactly $AWSCLI_KEY_FPR" >&2
  exit 1
fi
expires=$(awk -F: '$1 == "pub" { print $7; exit }' <<<"$colons")
now=$(date +%s)
if [ -n "$expires" ]; then
  left=$(( (expires - now) / 86400 ))
  if [ "$expires" -le "$now" ]; then
    echo "::error::The AWS CLI signing key $AWSCLI_KEY_FPR expired $(date -u -d "@$expires" +%F 2>/dev/null || echo "at $expires"). Replace $KEY_FILE with the current key from https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html, check its fingerprint against that page, and update the pinned fingerprint here and in scripts/awsclipin." >&2
    exit 1
  fi
  if [ "$left" -lt "$WARN_DAYS" ]; then
    echo "::error::The AWS CLI signing key $AWSCLI_KEY_FPR expires in $left days. Replace $KEY_FILE with the renewed key from https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html before then (a renewal keeps the fingerprint; a new key changes it here and in scripts/awsclipin)." >&2
    exit 1
  fi
  echo "AWS CLI signing key $AWSCLI_KEY_FPR: expires in $left days"
fi

# 2. The pin and the candidate.
current=$(go -C scripts run ./awsclipin show --bootstrap ../pkg/launcher/bootstrap.go | sed -n 's/^awsCLIVersion=//p')
if [ -z "$current" ]; then
  echo "::error::could not read awsCLIVersion from pkg/launcher/bootstrap.go" >&2
  exit 1
fi
if [ -z "$version" ]; then
  version=$(git ls-remote --tags --refs https://github.com/aws/aws-cli 'refs/tags/2.*' |
    awk '{ sub("refs/tags/", "", $2); print $2 }' | grep -E '^2\.[0-9]+\.[0-9]+$' | sort -V | tail -1 || true)
  if [ -z "$version" ]; then
    echo "::error::found no 2.N.N tag in github.com/aws/aws-cli: the version source has broken" >&2
    exit 1
  fi
fi
if ! [[ $version =~ ^2\.[0-9]+\.[0-9]+$ ]]; then
  echo "::error::$version is not an AWS CLI v2 release" >&2
  exit 1
fi
if [ "$(printf '%s\n%s\n' "$current" "$version" | sort -V | tail -1)" = "$current" ]; then
  echo "AWS CLI pin $current is current (newest release: $version)"
  exit 0
fi
echo "AWS CLI pin $current; newer release $version"

# 3. Both zips, each verified against the key, then hashed.
sha256() { if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d ' ' -f 1; }
sum_x86_64='' sum_aarch64=''
for arch in x86_64 aarch64; do
  zip="$work/awscli-exe-linux-$arch-$version.zip"
  url="https://awscli.amazonaws.com/awscli-exe-linux-$arch-$version.zip"
  code=$(curl -sS -o /dev/null -w '%{http_code}' -I "$url")
  if [ "$code" = 404 ] || [ "$code" = 403 ]; then
    echo "::warning::$url is not published yet (HTTP $code); the next run will retry"
    exit 0
  fi
  curl -fsS --retry 3 --max-time 900 -o "$zip" "$url"
  curl -fsS --retry 3 -o "$zip.sig" "$url.sig"
  # A good signature is not enough: it must be by the pinned key, whose primary
  # fingerprint is VALIDSIG's last field.
  status=$(gpg --batch --status-fd 1 --verify "$zip.sig" "$zip" 2>"$work/gpg.err") || {
    cat "$work/gpg.err" >&2
    echo "::error::$url: signature does not verify against $AWSCLI_KEY_FPR" >&2
    exit 1
  }
  if ! awk -v fpr="$AWSCLI_KEY_FPR" '$1 == "[GNUPG:]" && $2 == "VALIDSIG" && $NF == fpr { ok = 1 } END { exit !ok }' <<<"$status"; then
    echo "::error::$url: no VALIDSIG by $AWSCLI_KEY_FPR in gpg's status output" >&2
    exit 1
  fi
  h=$(sha256 "$zip")
  case $arch in x86_64) sum_x86_64=$h ;; aarch64) sum_aarch64=$h ;; esac
  echo "$arch: signature verified, sha256 $h"
done

go -C scripts run ./awsclipin set --bootstrap ../pkg/launcher/bootstrap.go --changelog ../CHANGELOG.md \
  --version "$version" --sha256-x86_64 "$sum_x86_64" --sha256-aarch64 "$sum_aarch64"
output bumped true
output from "$current"
output version "$version"
echo "moved the AWS CLI pin $current -> $version"
