# Spawn CLI Assessment — Prioritized Findings Report

## 1. Executive Summary

The spawn CLI surface is fundamentally healthy: the command tree is broad (130 nodes) but the core lifecycle verbs (`start`/`stop`/`hibernate`/`terminate`/`cancel`), the `--json`→`--output` deprecation gate, and the destructive-verb `--yes` convention are all consistent and test-enforced. The problems cluster into three themes: **(a) CLI ergonomics drift** — flat-hyphenated command families that should be subgroups (`notify workspace-*`, `autoscale *-schedule`), and inconsistent flag names/types across the launch family (`--subnet`/`--subnet-id`, `--key-pair`/`--key-name`, `--tag`/`--tags`, `--regions` slice-vs-string, `--output`/`-o` shadowing the root format flag); **(b) internal dead code** — several fully-unwired packages and file-level orphans totaling ~1000+ lines; and **(c) oversized/coupled internals** — `cmd/launch.go` (4324 LOC) and its 749-line `launchWithProgress`, the `pkg/aws` god-client, and cmd/ reaching for DynamoDB/STS directly instead of a store layer.

The single most valuable and safest thing to do first is the **one genuine safety bug**: `destructive-gate-misses-compound-verbs` — `workspace-remove` and `remove-schedule` perform irreversible DynamoDB/scheduler deletes with **no confirmation and no `--yes`**, because the gate matches on exact `Name()` and hyphenated compound verbs slip past it. Everything else is consistency polish, non-user-facing cleanup, or structural refactoring that can be batched behind the CLI-grouping decisions.

Note: severities below reflect the **corrected** verdicts, not the original submissions (several dead-code "high"s were downgraded once confirmed to be isolated/test-covered, and several "high" CLI-grouping items were downgraded to cosmetic).

---

## 2. Quick Wins (effort = quick-win)

Ordered by payoff (safety → user-visible consistency → internal hygiene).

| id | title | dimension | severity | evidence (file:line) | recommendation (short) | userFacing / SemVer |
|---|---|---|---|---|---|---|
| `cli-schedule-describe-verb` | `schedule describe` is the lone `describe` verb (rest use status/show/info) | cli-verbs | medium | cmd/schedule.go:100 vs team.go:71, queue.go:132, fsx.go:32 | Rename to `schedule show` (align to newest-group convention); keep `describe` as hidden alias | **Yes** — alias + CHANGELOG + MINOR |
| `cli-flags-tag-vs-tags` | `--tag` (StringArray) everywhere except autoscale launch `--tags` (StringToString) | cli-flags | low | cmd/autoscale.go:221 vs launch.go:373, list.go:49, snapshot.go:112, capacity_block.go:65 | ADD canonical `--tag` StringArray to autoscale launch; keep `--tags` as deprecated alias (different type — not a rename) | **Yes** — alias + CHANGELOG + MINOR |
| `cli-flags-subnet-name` | launch `--subnet` vs `--subnet-id` on autoscale/burst/image import | cli-flags | low | cmd/launch.go:236 vs autoscale.go:217, burst.go:57, image.go:675 | Standardize on `--subnet-id`; add it to launch, deprecate `--subnet` (do not remove) | **Yes** — alias + CHANGELOG + MINOR |
| `cli-flags-ssh-key-name` | launch `--key-pair` vs `--key-name` on autoscale/burst | cli-flags | low | cmd/launch.go:240 vs autoscale.go:216, burst.go:56 | Standardize on `--key-name` (matches AWS KeyName); add to launch, deprecate `--key-pair`. Leave connect/plugin `--key` (private-key path) alone | **Yes** — alias + CHANGELOG + MINOR |
| `cli-flags-detach-vs-detached` | async flag `--detach` (launch/sweep) vs `--detached` (pipeline launch) | cli-flags | low | cmd/launch.go:324, sweepgroup.go:113 vs pipeline.go:155 | Canonical `--detach`; register `--detached` as deprecated alias. NOTE: `flagDetached` is a **no-op** today (never read) — wire it or drop it | **Yes** — alias + CHANGELOG + MINOR |
| `cli-flags-execute-force-vs-confirm` | "execute vs dry-run" is `--force` (cleanup) vs `--confirm` (workspace-destroy), plus `--dry-run` (capacity-block) | cli-flags | low | cmd/cleanup.go:42, bot.go:1074, capacity_block.go:64 | Standardize on `--dry-run` (NOT `--force` — already means "allow downgrade" on upgrade-spored:50); keep `--confirm` as deprecated alias; add real `--yes` to workspace-destroy | **Yes** — alias + CHANGELOG + MINOR + update flag_conventions_test isDestructive() |
| `cli-cost-thin-group` | `cost` group has a single child `breakdown` | cli-grouping | low | cmd/cost.go:17, :31 (grep=2) | Optionally fold to top-level `spawn cost <sweep-id>`; keep `cost breakdown` as hidden alias — or leave as-is | **Yes** — alias + CHANGELOG + MINOR (if flattened) |
| `cli-image-ami-overlap` | `ami` and `image` both say "manage machine images" | cli-grouping | low | cmd/amigroup.go:9, image.go:45, snapshot.go:29 | Disambiguate Short strings only (ami=capture from running; image=build from ISO). Drop the `ami snapshots` vs `snapshot` overlap note | No (help-text only) — CHANGELOG Documentation entry, no bump |
| `dead-pkg-sms` | `pkg/sms` unreferenced within spawn | dead-code | low | pkg/sms/sms.go (6 funcs) | **Do NOT delete blindly** — imported cross-repo by spore-host `lambda/rest-api` (PendingKey/PendingNotification/PendingTable). Coordinate as public-API removal | No CLI surface — but public Go API; SemVer/CHANGELOG note |
| `dead-pkg-instance-patterns` | `pkg/instance` unreferenced anywhere | dead-code | low | pkg/instance/patterns.go:13,48 | Delete patterns.go + patterns_test.go. lambda/sweep-orchestrator has its own private `expandWildcard` | No — no bump |
| `dead-pkg-streaming` | `pkg/streaming` fully dead (~71 funcs) | dead-code | high | pkg/streaming/{tcp,transport,zmq}.go | Delete all 5 files; also remove examples/streaming/README.md + the exemplar in .claude/agents/test-coverage.md | No — CHANGELOG Removed (documented example) |
| `dead-audit-context-file` | `pkg/audit/context.go` fully dead (7 helpers) | dead-code | low | audit/context.go:19-74 | Delete context.go + its 4 orphaned tests in logger_test.go (~154-200). **Do NOT** delete SetUserID/SetCorrelationID/GetCorrelationID — test-covered | No — no bump |
| `dead-dns-encoding-inverse-funcs` | dead DNS helpers DecodeAccountID/GetAccountSubdomain/ParseDNSName + pkg-level GetFQDN | dead-code | medium | encoding.go:22,30,44; client.go:393 | Delete the 4 as one set (DecodeAccountID dies once ParseDNSName goes) + their unit tests. Keep the `(c *Client) GetFQDN` method | No — optional CHANGELOG |
| `dead-scheduler-alerts-alt-constructors` | dead alt-constructors/methods in pkg/scheduler + pkg/alerts | dead-code | low | scheduler.go:55,277,330; alerts.go:98,110,174,291 | Delete the 7 unused constructors/CRUD methods | No — no bump |
| `dead-queue-dependency-funcs` | `DependenciesMet`/`GetReadyJobs` dead (topo-sort path is live) | dead-code | low | pkg/queue/dependency.go:72,82 | Delete both + their tests (dependency_test.go:139,199) in same change or build breaks | No — no bump |
| `dead-cmd-helper-oneoffs` | dead helpers waitForDCV / getTagValue / formatTTLDuration | dead-code | low | app.go:610, completion.go:169, extend.go:307 | Delete all three + their tests (utils_test.go:220, extend_test.go:160). Note lambda/dashboard-api has its own live `getTagValue` — don't touch | No — no bump |
| `dup-format-duration` | `formatDuration` == `formatDurationForTTL` (byte-identical) | duplication | medium | cmd/list.go:480-501, state.go:566-587 | Delete `formatDurationForTTL`, point state.go TTL path at `formatDuration`; update state_test.go:141/143/168/329. Do NOT fold pkg/slurm's variant | No — no bump |
| `dup-truncate` | `truncate` == `truncateValue` | duplication | low | cmd/pipeline.go:530-535, list.go:380-385 | Keep one (cmd/utils.go), delete other. Panic-if-maxLen<3 is unreachable (optional hardening) | No — no bump |
| `dup-tabwriter-init` | `tabwriter.NewWriter(...,2,' ',0)` repeated ~20x | duplication | low | 20 sites; dns.go:88 uses padding 3 | Add `newTableWriter()` in cmd/utils.go; normalize dns.go. Weak/optional — do when touching files | No — no bump |
| `dup-parse-duration` | divergent duration parsers; config path silently drops `d` support | duplication | low | state.go:514-563 vs config/local.go:203-212 | Make `config.ParseDuration` delegate to the complete parser (fixes `ttl: 2d`→0 latent bug). Drop extend.go formatTTLDuration from this item (it's a formatter/dead). 3 lambda copies can't share | No — behavior change on config parse: CHANGELOG Fixed + bump |
| `buildtags-223-line-fn` | `buildTags` is 218 straight-line lines | oversized | low | pkg/aws/client.go:623-840 (not :846) | Low priority; group by concern only if splitting the file | No — no bump |
| `autoscale-go-flat` | cmd/autoscale.go 1335 LOC, ~15 handlers | oversized | low | autoscale.go:747,857,1138,1230,1274 | Same-package split into autoscale_schedule.go / autoscale_policy.go | No — no bump |

---

## 3. Structural Changes (effort = medium / structural)

| id | title | dimension | severity | evidence (file:line) | recommendation (short) | userFacing / SemVer |
|---|---|---|---|---|---|---|
| `destructive-gate-misses-compound-verbs` | **SAFETY**: `workspace-remove`/`remove-schedule` do irreversible deletes with no `--yes` and no prompt | cli-flags | medium | flag_conventions_test.go:45-53; bot.go:583→599-604; autoscale.go:172→1230 | Fix isDestructive() to match last hyphen-segment (or subgroup so Name()=`remove`/`destroy`), then ADD `--yes`+prompt to both. workspace-destroy is already gated (dry-run/--confirm) — not a bug | **Yes** (if regrouping) — alias + CHANGELOG + MINOR |
| `notify-workspace-flat-verbs` / `cli-notify-workspace-group` | `notify workspace-*` should be a `workspace` subgroup | cli-grouping | low/medium | bot.go:505,583,614,678; init 1006-1009 | `notify workspace add\|remove\|list\|destroy`; keep flat names as hidden aliases; preserve workspace-destroy `--confirm` (test-whitelisted) | **Yes** — alias + CHANGELOG + MINOR |
| `autoscale-schedule-flat-verbs` / `cli-autoscale-schedule-group` | `autoscale *-schedule` flat-hyphenated + shadows top-level `schedule` | cli-grouping | medium/low | autoscale.go:165,172,179; schedule.go:31 | `autoscale schedule add\|remove\|list` (use `list`, not `create`); flat names as deprecated aliases. Decide whether to also regroup set-policy/activity | **Yes** — alias + CHANGELOG + MINOR |
| `cli-autoscale-policy-activity-pairs` / `autoscale-set-policy-pair` | `set-policy`/`set-metric-policy`, `scaling-activity`/`metric-activity` flat pairs | cli-verbs | low | autoscale.go:137,144,151,158 | Scope to the one real asymmetry (`set-metric-policy` mid-token qualifier); full `policy`/`activity` regroup not worth 4-cmd churn | **Yes** — alias + CHANGELOG + MINOR |
| `read-verb-detail-fragmentation` | 4 verbs for single-resource detail: info/describe/show/(status) | cli-verbs | medium | fsx.go:32, schedule.go:100, team.go:71, queue.go:132, autoscale.go:102 | Standardize `show`=one static, `status`=one live, `list`=many; rename `fsx info`→`fsx show`, `schedule describe`→`schedule show` | **Yes** — aliases + CHANGELOG + MINOR |
| `delete-vs-remove-inconsistency` | `delete` vs `remove` (and `destroy`) inconsistent | cli-verbs | medium | alerts.go:114, fsx.go:40, team.go:85/92, plugin.go:190, bot.go:583/678 | Doc-only convention (remove=detach-from-parent, delete=destroy-standalone); also address delete-vs-destroy. Do NOT churn team | No (doc-only) — no bump |
| `autoscale-status-doubles-as-list` | autoscale has no `list`; `status [group]` overloads both | cli-verbs | low | autoscale.go:102, 187-201 | ADD `autoscale list` (purely additive), keep bare `status` working | **Yes** — additive only: CHANGELOG Added + MINOR, no alias |
| `cli-flags-local-output-shadows-root` | local `--output`/`-o` shadow root format flag | cli-flags | medium | root.go:78 vs queue.go:168/171/175, slurm.go:110, pipeline.go:160 | Rename path flags to `--output-file`/`-f` or `--output-dir` (precedent sweepgroup.go:116); keep `--output`/`-o` deprecated. Extend flag_conventions_test | **Yes** — deprecated alias + CHANGELOG + MINOR |
| `cli-flags-validate-snapshot-output-duplicate` | validate/snapshot reinvent root format flag; `validate -o json` errors | duplication | medium | validate.go:57, snapshot.go:113 vs root.go:78 | Delete local flags, read getOutputFormat() (root.go:113), branch on `=="json"`. Fixes live `spawn validate -o json` "unknown shorthand" bug | **Yes** — contract preserved: CHANGELOG Fixed/Changed + MINOR, no alias |
| `cli-flags-security-group-name-type` | SG flag has 3 names + 3 types; launch only accepts one SG | cli-flags | medium | launch.go:237, autoscale.go:218, burst.go:58, image.go:676 | Standardize `--security-group-ids` StringSlice across launch/autoscale/burst (fixes launch's single-SG gap); each rename needs alias | **Yes** — aliases + CHANGELOG + MINOR |
| `cli-flags-regions-type-mismatch` | `--regions` StringSlice on list, raw comma-string elsewhere | cli-flags | low | list.go:44 vs availability.go:36, collect.go:77, stage.go:118/130, sweepgroup.go:121 | StringSliceVarP everywhere + `-r` shorthand. Backward-compatible (no alias needed). Also fixes collect.go:125 missing TrimSpace | **Yes** — additive: CHANGELOG Changed + MINOR, no alias |
| `cli-flags-wait-type-mismatch` | `--wait` bool (launch/ami/pipeline) vs int minutes (image import) | cli-flags | low | launch.go:368, amigroup.go:50, pipeline.go:156 vs image.go:663 | Optional: bool `--wait` + `--wait-timeout` on image import (mirror launch.go:369). Current int is deliberate/tested — reasonable to defer | **Yes** (if changed) — CHANGELOG + MINOR + update TestWaitFlagParsing |
| `cli-flags-idle-action-boolean-vs-enum` | `--hibernate-on-idle` bool vs `--on-complete` enum | cli-flags | low | launch.go:254 vs launch.go:260 | Optional `--on-idle=stop\|hibernate` (NOT terminate — daemon only stops/hibernates); keep boolean as alias | **Yes** — alias + CHANGELOG + MINOR |
| `cli-flags-instance-flag-vs-positional` | instance is positional on lifecycle, `-i` flag on plugin/notify register | cli-flags | low | connect/status/stop/etc. positional vs plugin.go:457, bot.go:1045 | Mostly WONTFIX — plugin subcommands already use their positional for plugin ref; register is flag-defensible | **Yes** (if changed) — alias + CHANGELOG + MINOR |
| `dup-ssh-invocation` | SSH option block copy-pasted across 4 spored-exec sites | duplication | medium | status.go:98-108, config.go:78-88, extend.go:347-357, queue.go:395-406 | Extract `buildSporedSSHArgs`/`runSpored` for the 4 identical sites. Do NOT merge connect.go/launch.go (ControlMaster for #56) or plugin paths (accept-new/BatchMode) | No — no bump |
| `dup-account-id-sts` / `cmd-sts-caller-identity-bypasses-existing-helper` | 17 inline sts.NewFromConfig+GetCallerIdentity vs existing GetAccountID | duplication | low/medium | client.go:1806/1828; 17 sites across 8 cmd files | Add `accountIDFromConfig(ctx,cfg)` or use `aws.NewClientFromConfig(cfg).GetAccountID`; respect per-site config (devCfg vs infra). Adds IMDS fallback | No — CHANGELOG Changed |
| `dup-regional-ec2-client` | per-region EC2 client boilerplate ~43+ sites | duplication | low | 47 ec2.NewFromConfig, 43 cfg.Copy(); getRegionalConfig client.go:1924 | Add `c.regionalEC2(region)`; retire getRegionalConfig (its err is always nil — kills dead branches) | No — no bump |
| `dup-list-then-filter-by-name` | list-then-filter loops (does NOT dup resolveInstance) | duplication | low | connect.go:94/592/614, app.go:387, state.go:391/206-226/425-438 | Poll loops should reuse existing DescribeInstanceWithRetry/GetInstanceState; job-array filter can share a predicate. resolveInstance untouched | No — no bump |
| `launch-go-god-file` | cmd/launch.go 4324 LOC mixes 5 concerns | oversized | high | launch.go:697-1232, 3014-3496, 3497-3996, 3997-4173, 2509-2727 | Same-package split: launch_sweep.go / launch_jobarray.go / launch_regions.go / launch_userdata.go. Residual ~2000-2100 LOC | No — no bump |
| `launch-with-progress-750-line-fn` | `launchWithProgress` 749 lines, 11+ inline steps | oversized | high | launch.go:1233-1976 | Extract per-step helpers (ensureAMI/ensureSSHKey/ensureIAMProfile/ensureSecurityGroup/ensureFSx/buildAndLaunch/waitAndRegisterDNS); mind the job-array early-return branch | No — no bump |
| `aws-client-grab-bag` | pkg/aws/client.go 2379 LOC / 60 funcs grab-bag | oversized | medium | client.go:1572-1805 (IAM), 1932-2286 (SG), 623-845 (buildTags), 2287/2319 | Same-package moves: IAM→iam.go, SG→securitygroups.go, buildTags→tags.go, EFS/pricing to domain files | No — no bump |
| `setup-spored-iam-role-209-line-fn` | SetupSporedIAMRole (actually ~105 lines) | oversized | low | client.go:1572-1676 (const/waitForInstanceProfile follow) | Extract inline trust-policy to const; consolidate with iam.go create-or-get. Propagation-wait already extracted | No — no bump |
| `sweep-launch-fns-duplication` | shared parallel-launch+tally idiom (2 fns, not 5) | duplication | low | launch.go:1017 + 3239 (dup `launchResult` struct); 3323 vs no-cleanup | Extract `runLaunchBatch` for launchAllAtOnce + launchJobArray only (closes #220-class cleanup divergence). Not cohort/detached paths | No — optional CHANGELOG |
| `cmd-direct-dynamodb-no-store-layer` | cmd/ talks to DynamoDB directly (27 sites); no store layer | coupling | medium | bot/team/pipeline/list-sweeps/collect (raw ops); alerts/cost/autoscale/availability already delegate | Extract per-domain repos for the ~5 leaking files (mirror pkg/alerts.Client). Leave the 4 injecting ones. pkg/team does NOT exist | No — no bump |
| `aws-god-client-split` | pkg/aws god package (~7.2K non-test lines, 32 *Client methods) | coupling | medium | client.go 2379; siblings iam.go 768, cleanup.go 563… | No cycle — split opportunistically along file domains (imaging/fsx/iam/thin core) as files are touched | No — no bump |
| `cmd-flat-package-33-imports` | cmd imports 33 internal pkgs; 19/50 files hit AWS SDK directly | coupling | low | go list fan-out; sts=9/dynamodb=9 files | Add guardrail test forbidding new aws-sdk service imports in cmd/ (allowlist). Gated on store-layer + STS-helper landing | No — internal test |

---

## 4. User-Facing Changes & SemVer

Every `userFacing=true` finding. Per repo conventions (SemVer + Keep-a-Changelog, spawn/CLAUDE.md), any command/flag rename requires a **back-compat alias + CHANGELOG entry + SemVer bump**.

### Group A — Requires deprecated alias + CHANGELOG Deprecated/Changed (breaking-if-removed renames)
- `cli-schedule-describe-verb` — `schedule describe` → `schedule show`
- `read-verb-detail-fragmentation` — `fsx info`→`fsx show`, `schedule describe`→`schedule show`
- `cli-notify-workspace-group` / `notify-workspace-flat-verbs` — `workspace-*` → `notify workspace *` (preserve `--confirm`)
- `cli-autoscale-schedule-group` / `autoscale-schedule-flat-verbs` — `*-schedule` → `autoscale schedule *`
- `cli-autoscale-policy-activity-pairs` / `autoscale-set-policy-pair` — `set-metric-policy` mid-token fix
- `cli-cost-thin-group` — `cost breakdown` → `cost <id>` (if flattened)
- `cli-flags-local-output-shadows-root` — path `--output`/`-o` → `--output-file`/`--output-dir`
- `cli-flags-tag-vs-tags` — add `--tag`, deprecate `--tags` (autoscale launch)
- `cli-flags-subnet-name` — add `--subnet-id`, deprecate `--subnet` (launch)
- `cli-flags-security-group-name-type` — standardize `--security-group-ids`
- `cli-flags-ssh-key-name` — add `--key-name`, deprecate `--key-pair` (launch)
- `cli-flags-detach-vs-detached` — deprecate `--detached` → `--detach`
- `cli-flags-execute-force-vs-confirm` — standardize `--dry-run`, deprecate `--confirm` (also updates isDestructive())
- `cli-flags-idle-action-boolean-vs-enum` — add `--on-idle`, deprecate `--hibernate-on-idle`
- `cli-flags-instance-flag-vs-positional` — (only if pursued; recommended WONTFIX)
- `destructive-gate-misses-compound-verbs` — (only the subgroup-rename option is a rename; the isDestructive() fix + adding `--yes` is not)

### Group B — Additive / behavior-preserving: CHANGELOG only, NO alias needed
- `autoscale-status-doubles-as-list` — ADD `autoscale list` (additive; bare `status` still works)
- `cli-flags-regions-type-mismatch` — StringVar→StringSlice is backward-compatible; add `-r`
- `cli-flags-validate-snapshot-output-duplicate` — observable contract preserved (Fixed: `validate -o json`)
- `cli-flags-wait-type-mismatch` — (if changed) needs deprecation window since `--wait=N` breaks under bool

### Group C — Docs/help-text only (no command/flag rename, no bump)
- `cli-image-ami-overlap` — Short-string clarification (CHANGELOG Documentation)
- `delete-vs-remove-inconsistency` — convention documentation only
- `resume-verb-two-meanings` — verb-glossary doc only
- `lifecycle-verbs-consistent` — negative finding, glossary doc only

**Recommended SemVer if all Group A+B shipped in one release:** a single **MINOR** bump (pre-1.0 breaking is still MINOR per the note in the findings). All renames ship as deprecated aliases (nothing hard-removed), so no MAJOR is warranted. Group C adds no bump beyond whatever MINOR is already cut.

---

## 5. Investigated & Ruled Out

- **`fp-launcher-provision`** — `launcher.Provision` + `validateEphemeralFSx`/`createAndAttachEphemeralFSx` (provision.go:59/171/205) flagged dead by `deadcode`, but it is the **exported headless launch entrypoint** (the #220 hotpath / Lambda no-SSH path) called by `test/e2e/tier2_launcher_test.go:34` (build-tagged, invisible to deadcode). **KEEP.**
- **`fp-testutil-and-e2e-helpers`** — `pkg/testutil` (~55 funcs across helpers.go/substrate.go/integration_helpers.go) and `test/e2e/helpers.go` flagged dead, but they are **test infrastructure** used only by `_test.go` and build-tagged e2e files, which `deadcode` ignores. `grep -rl pkg/testutil` shows ~30 `_test.go` importers. **KEEP.**
- **`dead-pkg-sms`** — NOT deletable as first assumed: `pkg/sms` is a **published cross-repo public API** imported by the sibling `spore-host/spore-host/lambda/rest-api` (uses `PendingKey`/`PendingNotification`/`PendingTable`, pinned to spawn v0.36.1). The whole-repo grep missed the sibling repo. Treat as coordinated public-API removal, not a quick win.
- **`dead-pkg-streaming-sms-instance` (combined item)** — the "zero importers anywhere" headline was **partly wrong** (pkg/sms is live cross-repo per above). pkg/streaming and pkg/instance ARE confirmed dead within the whole `~/src` tree; the corrected dead-line tally is ~1058, not ~1150.
- **`dup-list-then-filter-by-name`** — the central claim that these loops "duplicate resolveInstance's name/ID matching" was **refuted**: none of the flagged loops reproduce resolveInstance's i-prefix/case-insensitive/running-over-stopped logic. They are unrelated tag-filter, job-array-set, and exact-ID poll patterns. Downgraded and re-scoped.
- **`cli-flags-json-gate-confirmed`** — verified the existing guardrail HOLDS: all 9 `--json` flags are properly `MarkDeprecated`; `go test ./cmd -run TestFlagConventions` passes. No action; used as the model to extend for `--output`/`--tag`.
- **`lifecycle-verbs-consistent`** — verified as a **negative finding**: start/stop/hibernate/terminate and orchestration `cancel` vs instance `terminate` are a clean, consistent split. No change.
- **`resume-verb-two-meanings`** — two senses (continue-sweep vs un-pause) disambiguated by noun context; top-level `resume` is already correctly deprecated toward `sweep resume` (sweepgroup.go:126, verified live). Doc-only.
- **General cross-module caveat honored throughout:** `deadcode` over the main module cannot see the 11 lambda go.mod modules. Every pkg-level "dead" claim (streaming, sms, instance, alerting, compliance, audit, dns, scheduler, alerts, queue) was re-grepped across all lambda modules + cmd/spored + cmd/orchestrator before confirming. This is how the sms cross-repo live-use and the alerting/compliance isolation were correctly resolved.

---

## 6. Recommended Execution Order

### Phase 0 — Do now, no design discussion (safety + isolated cleanup)
1. **`destructive-gate-misses-compound-verbs`** (the isDestructive() fix + add `--yes`/prompt to `workspace-remove` and `remove-schedule`). This is the one real safety gap — irreversible deletes with zero confirmation. Do the gate fix + prompt **without** waiting on the subgroup renames.
2. **Dead-code deletions** that are fully isolated and test-safe, batchable in one PR each or together: `dead-pkg-streaming` (+ its docs), `dead-pkg-instance-patterns`, `dead-audit-context-file` (+ orphaned tests), `dead-dns-encoding-inverse-funcs` (+ tests), `dead-scheduler-alerts-alt-constructors`, `dead-queue-dependency-funcs` (+ tests), `dead-cmd-helper-oneoffs` (+ tests). Each deletes its own tests in the same change.
3. **`dead-pkg-sms`** — file/track as a coordinated cross-repo public-API removal; **do not** bundle with Phase 0.2.
4. **Same-package duplication cleanups** (behavior-preserving, no user surface): `dup-format-duration`, `dup-truncate`, `dup-tabwriter-init`, `dup-regional-ec2-client`. `dup-parse-duration` fixes a real latent bug (`ttl: 2d`→0) so give it its own PR with a CHANGELOG Fixed entry.

### Phase 1 — Do now, additive/backward-compatible user-facing (single MINOR release)
5. **Additive flag fixes** (no alias needed): `autoscale-status-doubles-as-list` (add `list`), `cli-flags-regions-type-mismatch` (StringSlice + `-r`), `cli-flags-validate-snapshot-output-duplicate` (fixes `validate -o json`).
6. **`dup-account-id-sts` / `cmd-sts-caller-identity-bypasses-existing-helper`** and **`dup-ssh-invocation`** — internal refactors, no user surface; can land alongside Phase 1 or defer.

### Phase 2 — Needs a design decision, then one coordinated MINOR release
These are user-facing renames requiring aliases + CHANGELOG + a SemVer call. **Decide the conventions first**, then batch the renames so aliases and the CHANGELOG are written once:
- **Detail-verb convention** (`read-verb-detail-fragmentation` + `cli-schedule-describe-verb`): pick `show`/`status`/`list` rule, then rename `fsx info`/`schedule describe`.
- **CLI subgroup regrouping** (`notify-workspace-*`, `autoscale-*-schedule`): decide whether to also regroup `set-policy`/`activity`, then execute. This unlocks the cleaner form of the `destructive-gate` fix (leaf `Name()` becomes `remove`/`destroy`).
- **Flag-name/type standardization batch** (`cli-flags-subnet-name`, `cli-flags-ssh-key-name`, `cli-flags-security-group-name-type`, `cli-flags-tag-vs-tags`, `cli-flags-local-output-shadows-root`, `cli-flags-detach-vs-detached`, `cli-flags-execute-force-vs-confirm`): agree the canonical names (`--subnet-id`, `--key-name`, `--security-group-ids`, `--tag`, `--output-file`/`--output-dir`, `--detach`, `--dry-run`) and extend `flag_conventions_test.go` with the new locks in the same PR.
- Lower priority / arguably WONTFIX: `cli-cost-thin-group`, `cli-flags-wait-type-mismatch`, `cli-flags-idle-action-boolean-vs-enum`, `cli-flags-instance-flag-vs-positional`, `cli-autoscale-policy-activity-pairs`.
- Doc-only, fold into the same release notes: `cli-image-ami-overlap`, `delete-vs-remove-inconsistency`, `resume-verb-two-meanings`, `lifecycle-verbs-consistent` (verb glossary).

### Phase 3 — Structural refactors (largest blast radius; do after Phase 2 grouping decisions)
7. **`launch-go-god-file`** first (pure same-package file split), then **`launch-with-progress-750-line-fn`** (step extraction) — the file split makes the function extraction reviewable. **`sweep-launch-fns-duplication`** (`runLaunchBatch`) is best done immediately after, since it also closes the #220-class cleanup divergence.
8. **`autoscale-go-flat`** split — prerequisite that makes the `autoscale schedule`/`policy` regroupings (Phase 2) cleaner; sequence it before them if doing both.
9. **`aws-client-grab-bag`** → **`setup-spored-iam-role-209-line-fn`** / **`buildtags-223-line-fn`** — same-package moves; do opportunistically. Watch `TestBuildInlinePolicy` statement-count assertion (helpers_test.go) per project memory.
10. **`aws-god-client-split`** and **`cmd-direct-dynamodb-no-store-layer`** — the two big **needs-design-discussion** items. No cycle exists, so both are opportunistic; the store layer (repos for bot/team/pipeline/sweep, mirroring pkg/alerts.Client) is the higher-value of the two and is a prerequisite for the **`cmd-flat-package-33-imports`** guardrail test. Defer until there's appetite for a structural pass.

**Defer entirely / needs discussion:** the store-layer extraction (`cmd-direct-dynamodb-no-store-layer`), the `pkg/aws` domain split (`aws-god-client-split`), the cmd SDK-import guardrail (`cmd-flat-package-33-imports`), and `dual-ec2-access-layers-provider-vs-aws` (confirmed a *defensible* split — document the boundary rather than merge).