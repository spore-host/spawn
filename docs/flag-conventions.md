# CLI flag conventions

These conventions apply across the spore.host CLIs (`spawn`, `truffle`,
`lagotto`, `spored`). They exist so users and wrapper scripts can rely on one
predictable flag surface instead of per-command quirks. New commands MUST follow
them; `spawn`'s `TestFlagConventions` gate (in `cmd/flag_conventions_test.go`)
enforces the machine-checkable parts.

Origin: spawn#40 (suite-wide flag audit).

## Structured output: root `-o` / `--output`

Structured output is selected by the **root persistent flag**:

```
-o, --output string   Output format (table, json[, yaml, csv])   (default "table")
```

- A command that can emit structured data reads the format via the shared
  `getOutputFormat()` helper (spawn) / the root `outputFormat` var (truffle,
  lagotto) — it does **not** define its own output-format flag.
- **Do not** add a per-command `--json` bool. Several historical ones remain as
  **deprecated aliases** (`MarkDeprecated("json", "use --output json instead")`)
  and are wired as `if localJSON || getOutputFormat() == "json"`. They will be
  removed a release after deprecation. New code must not add more.
- **Do not** shadow `-o`/`--output` with a different meaning. A command that
  needs an output *directory* or *file* uses a distinct name+shorthand, e.g.
  `--output-file`/`-f` or `--output-dir` — never `--output`/`-o`.

## Confirmation on destructive commands: `--yes` / `-y`

Any command that destroys or irreversibly mutates state — verbs
`cancel`, `terminate`, `delete`, `remove`, `destroy` — MUST:

1. Register `--yes`/`-y` (`BoolVarP(&fooYes, "yes", "y", false, "Skip the confirmation prompt")`), and
2. Prompt for confirmation when it is absent, via the shared `confirmYes(skip, prompt)` helper (spawn `cmd/utils.go`).

`confirmYes` returns true immediately when `--yes` is set; otherwise it prompts
on stderr and returns true only on an explicit `y`/`yes`. A read error or
**non-interactive/piped stdin (EOF) reads as "no"**, so an unattended invocation
without `--yes` aborts rather than performing the destructive action silently —
this is what lets tools like nf-spawn drive these commands safely.

The confirmation is applied uniformly to these verbs **regardless of
reversibility** (e.g. lagotto's `cancel` only flips a watch's status), for
predictability across the suite.

Exception: `spawn notify ... destroy` uses `--confirm` with dry-run-by-default
semantics; the gate allows a command that has `--confirm` instead of `--yes`.

## Shorthand registry

Keep single-letter shorthands consistent across tools:

| Short | Long          | Meaning                          |
|-------|---------------|----------------------------------|
| `-o`  | `--output`    | output **format** (table/json/…) |
| `-r`  | `--regions`   | multi-region filter (slice)      |
| `-v`  | `--verbose`   | verbose output                   |
| `-y`  | `--yes`       | skip confirmation prompt         |
| `-f`  | `--output-file` | output file path               |
| `-i`  | `--instance`  | instance ID/hostname             |

Don't reuse a shorthand for a different concept on another command.

## region vs regions

- A single-target/filter flag is `--region` (string).
- A multi-region flag is `--regions`/`-r` (string slice). truffle aliases
  `--region` → `--regions` for convenience.
