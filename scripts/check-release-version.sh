#!/usr/bin/env bash
#
# Release guard: prove the binaries about to be published report the tag they
# were built from.
#
# Why this exists. The version a binary reports used to be a hardcoded constant
# in the source (spawn: "0.38.1", spored: "0.1.0"). Nothing verified it, and
# nothing needed to: the release pipeline injects the real version with an -X
# ldflag, so the constant only ever surfaced in non-release builds. It drifted 59
# releases behind, and the drift was invisible precisely because releases were
# fine. Then a hand-built spawn was used to validate a feature and confidently
# announced itself as 0.38.1.
#
# The constants are gone (see pkg/buildinfo). This script closes the other half:
# it fails the tag if the ldflag wiring ever breaks — a renamed variable, a
# renamed module path, a mistyped ldflag, a `builds:` entry that lost its
# ldflags. Every one of those is silent today: `go build` succeeds, GoReleaser
# succeeds, the release publishes, and the binary reports "dev".
#
# Usage: check-release-version.sh <tag>     e.g. check-release-version.sh v0.98.0
set -euo pipefail

tag="${1:-}"
if [ -z "$tag" ]; then
  echo "usage: $0 <tag>   (e.g. v0.98.0)" >&2
  exit 2
fi

# The tag is vX.Y.Z; the version the binaries report is X.Y.Z. GoReleaser's
# {{.Version}} is the tag without the leading "v", and pkg/buildinfo trims a "v"
# from anything injected, so both sides agree on the un-prefixed form.
want="${tag#v}"

# Read the ldflags out of .goreleaser.yaml rather than restating them, so this
# check exercises the real wiring instead of a copy that could drift alongside
# it. A `builds:` entry that loses its -X line makes the grep come back empty and
# fails below.
spawn_ldflag=$(grep -oE '\-X [^ ]*spawn/cmd\.Version=\{\{\.Version\}\}' .goreleaser.yaml || true)
spored_ldflag=$(grep -oE '\-X main\.Version=\{\{\.Version\}\}' .goreleaser.yaml || true)

fail=0
note() { echo "::error::$*" >&2; fail=1; }

if [ -z "$spawn_ldflag" ]; then
  note ".goreleaser.yaml has no '-X .../spawn/cmd.Version={{.Version}}' ldflag — released spawn binaries would report 'dev'"
fi
if [ -z "$spored_ldflag" ]; then
  note ".goreleaser.yaml has no '-X main.Version={{.Version}}' ldflag — released spored binaries would report 'dev'"
fi
[ "$fail" -eq 0 ] || exit 1

# Build with exactly the ldflags the release will use, substituting the real tag
# for {{.Version}}, and ask each binary what it thinks it is. This is the part a
# static check can't do: it catches a ldflag whose variable name no longer
# resolves, which the Go linker accepts silently (it does not error on an -X for
# a symbol that doesn't exist).
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "Building spawn and spored with the release ldflags for ${tag}..."
go build -ldflags "${spawn_ldflag//\{\{.Version\}\}/$want}" -o "$tmp/spawn" .
go build -ldflags "${spored_ldflag//\{\{.Version\}\}/$want}" -o "$tmp/spored" ./cmd/spored

# `spawn version` prints "Version:    X.Y.Z"; take that field.
got_spawn=$("$tmp/spawn" version | awk '/^Version:/ {print $2}')
# `spored version` prints "spored version X.Y.Z".
got_spored=$("$tmp/spored" version | awk '{print $3}')

check() { # $1=binary name  $2=reported
  if [ "$2" != "$want" ]; then
    note "$1 reports version '$2' but the tag is '$tag' (expected '$want') — the -X ldflag is not reaching the version the binary prints"
  else
    echo "✅ $1 reports ${2} for tag ${tag}"
  fi
}
check spawn "$got_spawn"
check spored "$got_spored"

[ "$fail" -eq 0 ] || exit 1

# A release must not carry an unreleased-version placeholder in the changelog
# either: the tag is the moment [Unreleased] becomes [X.Y.Z] (see CLAUDE.md), and
# publishing with the section unpromoted means the release notes for X.Y.Z say
# "Unreleased" forever.
if [ -f CHANGELOG.md ] && ! grep -qF "## [${want}]" CHANGELOG.md; then
  note "CHANGELOG.md has no '## [${want}]' section — promote [Unreleased] to [${want}] before tagging"
fi

[ "$fail" -eq 0 ] || exit 1
echo "✅ Release version check passed for ${tag}"
