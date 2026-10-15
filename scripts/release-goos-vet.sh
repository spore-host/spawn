#!/usr/bin/env bash
#
# `go vet ./...` for every GOOS the release builds (#925).
#
# Why: CI and `make check` vet only the host GOOS. `go build` type-checks the
# Windows production code, because GoReleaser ships windows/amd64, but nothing
# type-checked the TESTS for any other GOOS. Two test files used Unix-only
# syscall APIs (syscall.Stat_t, SysProcAttr.Setpgid, syscall.Kill) with no
# build constraint, and `go test ./...` could not compile on Windows. Nobody
# noticed, because nothing ever tried.
#
# The GOOS set is DISCOVERED from .goreleaser.yaml rather than listed (CLAUDE.md
# rule 4), so a target added to the release is vetted from that day on. A
# discovery that finds nothing has failed: this exits non-zero instead of
# passing vacuously.
#
# The parse is awk, not a YAML library, so this needs nothing beyond the Go
# toolchain. That makes it a hand-rolled parser, so
# TestReleaseGOOSDiscoveryAgreesWithYAML (release_builds_test.go) checks its
# --list output against a real gopkg.in/yaml.v3 decode of the same file.
#
# Usage:
#   scripts/release-goos-vet.sh          vet ./... once per discovered GOOS
#   scripts/release-goos-vet.sh --list   print the discovered GOOS set and exit
#
# Only GOOS varies; GOARCH stays the host's. Every build-constrained file in
# this repo is constrained by OS, not architecture.
set -euo pipefail

cd "$(dirname "$0")/.."
cfg=${RELEASE_GOOS_CONFIG:-.goreleaser.yaml}
if [ ! -f "$cfg" ]; then
  echo "release-goos-vet: $cfg not found" >&2
  exit 1
fi

# Inside the top-level `builds:` section, collect each build's `goos:` list in
# either block form (`goos:` then `- linux` lines) or flow form
# (`goos: [linux, darwin]`). `ignore:` entries carry `- goos: windows` with a
# scalar value. That is an exclusion, not a target, and the scalar form is
# never read.
#
# Each build item (a `- ` line at the section's first item indent) must carry a
# goos list. A build without one gets GoReleaser's default set, which this
# script will not guess at. It fails and says to list the set explicitly, so
# the discovered set cannot be silently partial.
discover() {
  awk -v sq="'" '
    function indent(s) { match(s, /^ */); return RLENGTH }
    BEGIN { itemind = -1; builds = 0; q = "[\"" sq "]?"; itemre = "^ *- *" q "[A-Za-z0-9_]+" q " *(#.*)?$" }
    /^[^ #][^:]*:/ { insec = ($0 ~ /^builds:[ \t]*(#.*)?$/); inlist = 0; next }
    !insec { next }
    /^[ \t]*(#.*)?$/ { next }
    {
      ind = indent($0)
      # A block-list item belongs to the open goos list if it sits at or right
      # of the goos KEY (YAML allows `- linux` level with `goos:`).
      if (inlist && ind >= listind && $0 ~ itemre) {
        v = $0; sub(/^ *- */, "", v); sub(/ *(#.*)?$/, "", v); gsub("[\"" sq "]", "", v)
        print "GOOS " builds " " v; next
      }
      inlist = 0
      if (itemind < 0 && $0 ~ /^ *- /) itemind = ind
      if (ind == itemind && $0 ~ /^ *- /) { builds++; print "BUILD " builds }
      if ($0 ~ /^ *(- *)?goos: *(#.*)?$/) { inlist = 1; match($0, /goos:/); listind = RSTART - 1; next }
      if ($0 ~ /^ *(- *)?goos: *\[/) {
        v = $0; sub(/^[^\[]*\[/, "", v); sub(/\].*$/, "", v)
        n = split(v, parts, ",")
        for (i = 1; i <= n; i++) { p = parts[i]; gsub("[ \t\"" sq "]", "", p); if (p != "") print "GOOS " builds " " p }
      }
    }
  ' "$cfg"
}

raw=$(discover)
nbuilds=$(printf '%s\n' "$raw" | grep -c '^BUILD ' || true)
withlist=$(printf '%s\n' "$raw" | awk '$1 == "GOOS" { print $2 }' | sort -u | grep -c . || true)
goos=$(printf '%s\n' "$raw" | awk '$1 == "GOOS" { print $3 }' | sort -u)

if [ "$nbuilds" -eq 0 ] || [ -z "$goos" ]; then
  echo "release-goos-vet: discovered no builds/GOOS in $cfg; the parse is broken, not the release" >&2
  exit 1
fi
if [ "$withlist" -ne "$nbuilds" ]; then
  echo "release-goos-vet: $cfg has $nbuilds build(s) but only $withlist carry a non-empty goos list." >&2
  echo "  A build without one gets GoReleaser's default GOOS set, which this gate will not" >&2
  echo "  guess at. List the targets explicitly." >&2
  exit 1
fi

if [ "${1:-}" = "--list" ]; then
  printf '%s\n' "$goos"
  exit 0
fi

group_open() { [ -n "${GITHUB_ACTIONS:-}" ] && echo "::group::$1" || echo "  $1"; return 0; }
group_close() { [ -n "${GITHUB_ACTIONS:-}" ] && echo "::endgroup::"; return 0; }

fail=0
for os in $goos; do
  group_open "GOOS=$os go vet ./..."
  if ! GOOS="$os" go vet ./...; then
    echo "FAIL GOOS=$os" >&2
    fail=1
  fi
  group_close
done

if [ "$fail" -eq 0 ]; then
  echo "✅ go vet clean for every release GOOS: $(printf '%s\n' "$goos" | paste -sd, -)"
fi
exit $fail
