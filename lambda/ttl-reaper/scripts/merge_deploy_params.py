#!/usr/bin/env python3
"""Merge deploy parameters: explicit override > live stack value > template default.

spawn#650. `make deploy` previously asserted all 13 of its own defaults on every
run, so the documented command would have:

  - set DryRun true on a stack running DryRun=false, DISARMING the live production
    reaper — it keeps running and reporting, it just stops terminating anything,
    which is invisible until something outlives its TTL;
  - turned DnsSweep off and blanked DnsZoneId/DnsDomain;
  - detached AlarmTopicArn, so every failure alarm in the stack fires into nothing
    — including the not-invoked alarm that exists to catch a stopped reaper.

The Makefile's own comments explain the OPPOSITE hazard very carefully: an omitted
parameter becomes UsePreviousValue and is therefore unreachable, which is why #438
could not be enabled from `make deploy`. Passing everything explicitly fixed that
and created this. Read-then-write fixes both: an unspecified parameter keeps what
is deployed, and anything passed explicitly still gets through.

Pure: all three inputs arrive as JSON on argv, nothing calls AWS here, so the
merge is unit-testable without a stack.
"""

from __future__ import annotations

import json
import sys


def merge(summary: dict, live: list, explicit: dict) -> tuple[dict, list]:
    """Return (merged params, change notes).

    summary  — `aws cloudformation get-template-summary` output. Authoritative for
               WHICH parameters exist and their defaults; using it rather than
               parsing the YAML avoids hand-rolling a CFN parser (!Sub, !Ref and
               friends make that a bad idea).
    live     — `aws cloudformation describe-stacks --query Stacks[0].Parameters`,
               or [] for a stack that does not exist yet.
    explicit — only the values the caller actually set, as decided by Make's
               $(origin ...). A Makefile default is NOT explicit, which is the
               whole point.
    """
    declared = [p["ParameterKey"] for p in summary.get("Parameters", [])]
    defaults = {
        p["ParameterKey"]: p.get("DefaultValue")
        for p in summary.get("Parameters", [])
    }
    current = {p["ParameterKey"]: p["ParameterValue"] for p in live}

    merged: dict[str, str] = {}
    notes: list[str] = []

    for key in declared:
        if key in explicit:
            value = explicit[key]
            was = current.get(key)
            if was is not None and was != value:
                notes.append(f"  {key}: {was!r} -> {value!r}  (explicit)")
            elif was is None:
                notes.append(f"  {key}: (new) -> {value!r}  (explicit)")
            merged[key] = value
        elif key in current:
            # Keep what is deployed. This is the line that makes the dangerous
            # direction impossible rather than merely discouraged.
            merged[key] = current[key]
        elif defaults.get(key) is not None:
            merged[key] = defaults[key]
        # else: no explicit value, not deployed, no default -> let CFN complain,
        # rather than inventing one here.

    # An explicit key the template does not declare is a typo, and silently
    # dropping it would mean a deploy that looks like it did what you asked.
    for key in explicit:
        if key not in declared:
            raise SystemExit(
                f"merge_deploy_params: {key} is not a parameter of this template.\n"
                f"Declared: {', '.join(sorted(declared))}"
            )

    return merged, notes


def as_yaml(params: dict) -> str:
    """CFN parameter-overrides file. Quoted values, because an empty string is the
    case that broke the inline Key=Value form (see the Makefile's comment)."""
    return "".join(f'{k}: "{v}"\n' for k, v in params.items())


def main(argv: list[str]) -> int:
    if len(argv) != 4:
        print(
            "usage: merge_deploy_params.py <summary.json> <live.json> <explicit.txt>",
            file=sys.stderr,
        )
        return 2

    with open(argv[1]) as fh:
        summary = json.load(fh)
    with open(argv[2]) as fh:
        live = json.load(fh) or []
    # `CfnKey=value` lines, one per explicitly-supplied parameter. Split on the
    # FIRST '=' only: values are ARNs, region lists and cron expressions, and an
    # empty value (NotifyUrl=) must survive as "".
    explicit = {}
    with open(argv[3]) as fh:
        for line in fh:
            line = line.rstrip("\n")
            if not line:
                continue
            key, _, value = line.partition("=")
            explicit[key] = value

    merged, notes = merge(summary, live, explicit)

    if notes:
        print("deploy will CHANGE:", file=sys.stderr)
        for n in notes:
            print(n, file=sys.stderr)
    else:
        print(
            "deploy changes no parameters (code only); "
            f"{len(merged)} inherited from the live stack.",
            file=sys.stderr,
        )

    sys.stdout.write(as_yaml(merged))
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
