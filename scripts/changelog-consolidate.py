#!/usr/bin/env python3
"""Merge duplicate group headings in CHANGELOG.md's [Unreleased] section.

`changelog_test.go` rejects a duplicate `### Fixed` (etc.) because the release
ritual promotes [Unreleased] verbatim and Keep-a-Changelog wants one heading per
group. Two PRs each adding their own group merge cleanly for git and badly for the
format, so this is a routine, mechanical conflict rather than a mistake — it came
up four times in a single session before this script existed.

Entries keep their order within a group; groups are emitted in Keep-a-Changelog
order. Only [Unreleased] is touched: released sections are historical record.
"""

from __future__ import annotations

import pathlib
import re
import sys

CANON = [
    "### Added",
    "### Changed",
    "### Deprecated",
    "### Removed",
    "### Fixed",
    "### Security",
    "### Documentation",
]


def consolidate(text: str) -> str:
    start = text.index("## [Unreleased]")
    # The next release heading bounds the section; a file with only [Unreleased]
    # runs to the link block.
    m = re.search(r"(?m)^## \[(?!Unreleased)", text[start + 1 :])
    end = start + 1 + m.start() if m else len(text)

    head, block, tail = text[:start], text[start:end], text[end:]
    parts = re.split(r"(?m)^(### .+)$", block)
    intro = parts[0].rstrip("\n")

    groups: dict[str, list[str]] = {}
    seen: list[str] = []
    for i in range(1, len(parts), 2):
        name, body = parts[i].strip(), parts[i + 1].strip("\n")
        if name not in groups:
            groups[name] = []
            seen.append(name)
        if body:
            groups[name].append(body)

    order = [g for g in CANON if g in groups] + [g for g in seen if g not in CANON]

    out = [intro, ""]
    for g in order:
        out += [g, "", "\n\n".join(groups[g]), ""]
    return head + "\n".join(out).rstrip("\n") + "\n\n" + tail


def main() -> int:
    p = pathlib.Path(sys.argv[1] if len(sys.argv) > 1 else "CHANGELOG.md")
    before = p.read_text()
    after = consolidate(before)
    if before == after:
        print(f"{p}: already consolidated")
        return 0
    p.write_text(after)
    print(f"{p}: consolidated [Unreleased] groups")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
