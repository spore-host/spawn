#!/usr/bin/env bash
# The EFA installer that --efa runs as root (#1505): pinned in
# pkg/userdata/efa.go (efaInstallerVersion, efaInstallerSHA256) and checked
# against AWS's signature here, at pin time and at release, never on the
# instance. The instance checks only the pinned sha256.
#
#   scripts/efa-pin.sh bump [--version V]   move the pin to the newest release
#                                           (or V), if newer, by editing efa.go
#                                           and CHANGELOG.md (the weekly
#                                           .github/workflows/efa-pin.yml)
#   scripts/efa-pin.sh verify               require that the pinned tarball's
#                                           signature verifies and that its
#                                           sha256 is the pinned one (the
#                                           release workflow, before any build)
#
# Either way a tarball's bytes are trusted only once its detached .sig verifies
# with gpg as made by AWS's EFA key, committed as
# scripts/efapin/aws-efa-installer.key, with exactly the primary fingerprint
# below. That key and fingerprint are the ones the EC2 User Guide gives
# (https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/efa-start.html; key at
# https://efa-installer.amazonaws.com/aws-efa-installer.key). The key has no
# expiry today; if AWS ever sets one, this fails within 60 days of it.
#
# Version source (bump): the Version column of AWS's EFA release notes,
# https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/efa-changelog.html. A
# version listed before its tarball is published is reported and skipped; the
# next run picks it up.
#
# Writes `bumped`, `from`, `version` and `sha256` to $GITHUB_OUTPUT when set.
# Downloads into a throwaway directory outside the checkout (the tarball is
# about 600 MB) and removes it. Needs curl, gpg, go and sha256sum (or shasum);
# calls no AWS API.
set -euo pipefail
shopt -s inherit_errexit

readonly EFA_KEY_FPR=4E9091BCBB97A96B26B15E59A05480B1DD2D3CCC
readonly KEY_FILE=scripts/efapin/aws-efa-installer.key
readonly ORIGIN=https://efa-installer.amazonaws.com
readonly RELEASE_NOTES=https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/efa-changelog.html
readonly WARN_DAYS=60

cd "$(dirname "$0")/.."

mode=${1:-}
version=
case $mode in
  bump)
    if [ "${2:-}" = --version ]; then
      version=${3:?--version needs a value}
    elif [ $# -gt 1 ]; then
      echo "usage: $0 bump [--version 1.N.N]" >&2
      exit 2
    fi
    ;;
  verify) [ $# -eq 1 ] || { echo "usage: $0 verify" >&2; exit 2; } ;;
  *)
    echo "usage: $0 bump [--version 1.N.N] | verify" >&2
    exit 2
    ;;
esac

output() { if [ -n "${GITHUB_OUTPUT:-}" ]; then echo "$1=$2" >>"$GITHUB_OUTPUT"; fi; }
[ "$mode" = verify ] || output bumped false

work=$(mktemp -d)
cleanup() {
  gpgconf --kill all 2>/dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT
export GNUPGHOME="$work/gnupg"
mkdir -m 700 "$GNUPGHOME"

# 1. The key: exactly one primary key, the pinned one, not near any expiry.
gpg --batch --quiet --import "$KEY_FILE"
colons=$(gpg --batch --with-colons --fixed-list-mode --list-keys)
fprs=$(awk -F: '$1 == "pub" { want = 1; next } want && $1 == "fpr" { print $10; want = 0 }' <<<"$colons")
if [ "$fprs" != "$EFA_KEY_FPR" ]; then
  echo "::error::$KEY_FILE holds primary key(s) [$fprs], not exactly $EFA_KEY_FPR" >&2
  exit 1
fi
expires=$(awk -F: '$1 == "pub" { print $7; exit }' <<<"$colons")
if [ -n "$expires" ]; then
  left=$(((expires - $(date +%s)) / 86400))
  if [ "$left" -lt "$WARN_DAYS" ]; then
    echo "::error::The EFA installer signing key $EFA_KEY_FPR expires in $left days (or has). Replace $KEY_FILE from https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/efa-start.html and check its fingerprint against that page." >&2
    exit 1
  fi
fi

# 2. The pin, from the Go source of truth.
pins=$(go -C scripts run ./efapin show --source ../pkg/userdata/efa.go)
current=$(sed -n 's/^efaInstallerVersion=//p' <<<"$pins")
pinned_sum=$(sed -n 's/^efaInstallerSHA256=//p' <<<"$pins")
if ! [[ $current =~ ^1\.[0-9]+\.[0-9]+$ && $pinned_sum =~ ^[0-9a-f]{64}$ ]]; then
  echo "::error::could not read the EFA installer pin from pkg/userdata/efa.go: [$pins]" >&2
  exit 1
fi

sha256() { if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d ' ' -f 1; }

# verified_sum V: download aws-efa-installer-V.tar.gz and its .sig, require a
# VALIDSIG by the pinned key, print the tarball's sha256.
verified_sum() {
  local url="$ORIGIN/aws-efa-installer-$1.tar.gz" tgz="$work/aws-efa-installer-$1.tar.gz" status
  curl -fsS --retry 3 --max-time 1800 -o "$tgz" "$url"
  curl -fsS --retry 3 -o "$tgz.sig" "$url.sig"
  # A good signature is not enough: it must be by the pinned key, whose primary
  # fingerprint is VALIDSIG's last field.
  status=$(gpg --batch --status-fd 1 --verify "$tgz.sig" "$tgz" 2>"$work/gpg.err") || {
    cat "$work/gpg.err" >&2
    echo "::error::$url: signature does not verify against $EFA_KEY_FPR" >&2
    return 1
  }
  if ! awk -v fpr="$EFA_KEY_FPR" '$1 == "[GNUPG:]" && $2 == "VALIDSIG" && $NF == fpr { ok = 1 } END { exit !ok }' <<<"$status"; then
    echo "::error::$url: no VALIDSIG by $EFA_KEY_FPR in gpg's status output" >&2
    return 1
  fi
  sha256 "$tgz"
  rm -f "$tgz" "$tgz.sig"
}

if [ "$mode" = verify ]; then
  sum=$(verified_sum "$current")
  if [ "$sum" != "$pinned_sum" ]; then
    echo "::error::aws-efa-installer-$current.tar.gz is validly signed but its sha256 is $sum, not the pinned $pinned_sum" >&2
    exit 1
  fi
  echo "EFA installer $current: signature by $EFA_KEY_FPR verified, sha256 $sum is the pin"
  exit 0
fi

# 3. bump: the candidate.
if [ -z "$version" ]; then
  version=$(curl -fsS --retry 3 "$RELEASE_NOTES" | grep -oE '<td[^>]*>[[:space:]]*1\.[0-9]+\.[0-9]+[[:space:]]*</td>' |
    grep -oE '1\.[0-9]+\.[0-9]+' | sort -V | tail -1 || true)
  if [ -z "$version" ]; then
    echo "::error::found no 1.N.N version in $RELEASE_NOTES: the version source has broken" >&2
    exit 1
  fi
fi
if ! [[ $version =~ ^1\.[0-9]+\.[0-9]+$ ]]; then
  echo "::error::$version is not an EFA installer release" >&2
  exit 1
fi
if [ "$(printf '%s\n%s\n' "$current" "$version" | sort -V | tail -1)" = "$current" ]; then
  echo "EFA installer pin $current is current (newest release: $version)"
  exit 0
fi
echo "EFA installer pin $current; newer release $version"
code=$(curl -sS -o /dev/null -w '%{http_code}' -I "$ORIGIN/aws-efa-installer-$version.tar.gz")
if [ "$code" = 404 ] || [ "$code" = 403 ]; then
  echo "::warning::aws-efa-installer-$version.tar.gz is not published yet (HTTP $code); the next run will retry"
  exit 0
fi
sum=$(verified_sum "$version")
echo "aws-efa-installer-$version.tar.gz: signature verified, sha256 $sum"

go -C scripts run ./efapin set --source ../pkg/userdata/efa.go --changelog ../CHANGELOG.md --version "$version" --sha256 "$sum"
output bumped true
output from "$current"
output version "$version"
output sha256 "$sum"
echo "moved the EFA installer pin $current -> $version"
