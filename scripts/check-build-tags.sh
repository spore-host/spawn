#!/usr/bin/env bash
#
# Compile (go vet) every package under every custom build tag the repo uses,
# in every module, so code behind `//go:build <tag>` cannot rot unseen.
#
# Why (#913): the root integration_test.go is `//go:build integration` and did
# not compile from the initial release onward (`undefined: config`, then
# loadAWSConfig returning the awsconfig package's Config interface where
# aws.Config was needed). `make check` and CI only ever vetted the default
# build, so nothing noticed until someone tried `make test-integration`. #907
# was the same blind spot (a tagged test importing a deleted package); the tidy
# gate catches missing imports under any tag, but not type errors.
#
# Nothing here executes a test. The integration and e2e tier 1-3 tests launch
# real, billable instances; `go vet` type-checks them (test files included) and
# runs the vet analyzers, and that is all.
#
# The tag set is DISCOVERED from `//go:build` lines, never listed (CLAUDE.md
# "Writing a gate", rule 4). What is dropped from those lines is the toolchain's
# own vocabulary, not a list of "tags we know are fine": GOOS/GOARCH names come
# from `go tool dist list`, plus the fixed names the go command itself defines
# (unix, cgo, gc, gccgo, goN.M, goexperiment.X) and `ignore`, the convention for
# files that are never built.
#
# Each tag is vetted on its own. A file that only builds under a COMBINATION
# (`a && b`), or under a tag plus another GOOS, would be skipped by every
# single-tag run — so the script also checks that every custom-tagged file was
# compiled by at least one run, and fails naming any that was not.
#
# Usage: scripts/check-build-tags.sh
set -euo pipefail

root=$(pwd)

mods=$(find . -name go.mod -not -path './.git/*' | sort)
if [ -z "$mods" ] || ! printf '%s\n' "$mods" | grep -qx './go.mod'; then
  echo "module discovery did not find ./go.mod — the discovery is broken, not the repo" >&2
  exit 1
fi
mod_dirs=$(printf '%s\n' "$mods" | xargs -n1 dirname)

# Toolchain-defined constraint names; everything else on a //go:build line is
# a custom tag.
toolchain_names=$( { go tool dist list | tr '/' '\n'; printf '%s\n' unix cgo gc gccgo ignore; } | sort -u)

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fail=0
total_tags=0
total_files=0

for dir in $mod_dirs; do
  # The module's own .go files: under $dir but not under a nested module.
  prune=()
  for other in $mod_dirs; do
    case "$other" in
      "$dir"/*) prune+=( -path "$other" -prune -o ) ;;
    esac
  done
  find "$dir" -path ./.git -prune -o ${prune[@]+"${prune[@]}"} -name '*.go' -type f -print | sort > "$tmp/files"

  # Files carrying a custom tag, and the set of custom tags.
  : > "$tmp/tagged"; : > "$tmp/tags"
  while IFS= read -r f; do
    line=$(grep -m1 -E '^//go:build ' "$f" || true)
    [ -n "$line" ] || continue
    custom=$(printf '%s\n' "${line#//go:build }" | tr -c 'A-Za-z0-9_.\n' '\n' | grep -v '^$' |
      grep -vE '^go1(\.[0-9]+)*$|^goexperiment\.' | sort -u | comm -23 - <(printf '%s\n' "$toolchain_names") || true)
    if [ -n "$custom" ]; then
      (cd "$(dirname "$f")" && printf '%s/%s\n' "$(pwd -P)" "$(basename "$f")") >> "$tmp/tagged"
      printf '%s\n' "$custom" >> "$tmp/tags"
    fi
  done < "$tmp/files"
  tags=$(sort -u "$tmp/tags")
  [ -n "$tags" ] || continue

  : > "$tmp/compiled"
  for tag in $tags; do
    total_tags=$((total_tags + 1))
    echo "-- $dir: go vet -tags $tag ./..."
    if ! out=$(cd "$dir" && go vet -tags "$tag" ./... 2>&1); then
      echo "::error::$dir does not vet under -tags $tag:" >&2
      printf '%s\n' "$out" | sed 's/^/  /' >&2
      fail=1
    fi
    # Which files that run actually compiled (for the coverage check below).
    (cd "$dir" && go list -tags "$tag" -e \
      -f '{{$d := .Dir}}{{range .GoFiles}}{{$d}}/{{.}}{{"\n"}}{{end}}{{range .TestGoFiles}}{{$d}}/{{.}}{{"\n"}}{{end}}{{range .XTestGoFiles}}{{$d}}/{{.}}{{"\n"}}{{end}}' \
      ./... 2>/dev/null) >> "$tmp/compiled" || true
  done

  sort -u "$tmp/tagged" > "$tmp/tagged.s"
  # go list reports Dir with symlinks resolved, like `pwd -P` above.
  sort -u "$tmp/compiled" > "$tmp/compiled.s"
  missed=$(comm -23 "$tmp/tagged.s" "$tmp/compiled.s")
  if [ -n "$missed" ]; then
    echo "::error::$dir: these tagged files were compiled by no single-tag run (a tag combination, or a tag plus another GOOS?) — extend scripts/check-build-tags.sh to cover them:" >&2
    printf '%s\n' "$missed" | sed "s|^$root/|  |" >&2
    fail=1
  fi
  total_files=$((total_files + $(grep -c . "$tmp/tagged.s")))
done

if [ "$total_tags" -eq 0 ]; then
  # Refusing to pass vacuously: the repo has integration and e2e tags, so
  # finding none means the discovery broke.
  echo "found no custom build tags in any module — the discovery is broken, not the repo" >&2
  exit 1
fi

if [ "$fail" -eq 0 ]; then
  echo "✓ $total_files tagged files vet clean across $total_tags (module, tag) pairs"
fi
exit $fail
