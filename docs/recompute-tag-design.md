# Retention by regeneration cost (#1076)

**Status: decided 2026-10-17 (see [Decisions](#decisions)).** Phase 1
(record) shipped in v0.132.1 and was verified on a real instance. v0.132.0 lost
task outputs for about an hour (#1094; see [Shipping notes](#shipping-notes)).
Phase 2 (report: `spawn results retention`, read only) shipped in v0.133.0.
Phase 3 (tiering: `spawn results lifecycle`, `spawn results retain`) is in
progress, decided 2026-10-18 (Decisions 5 to 7). Phase 4 (the verifying
sweeper) is not started.

## The idea

When spawn writes a result to S3, it records what it would cost to make again:
compute used, the hourly rate and its source, output size, and what produced it.
Then retention follows from cost. Cheap-to-regenerate outputs can be tiered or
deleted by rule, without asking the researcher. Expensive ones are kept.

It is the data-side counterpart of the TTL reaper: today nothing ever removes a
result object (see [the survey](#what-existed-before-phase-1)).

## The constraint that shapes everything

**A lifecycle rule cannot verify anything at the moment it acts.** S3 lifecycle
filters match prefix, object size and tags by **exact key=value** only
([AWS](https://docs.aws.amazon.com/AmazonS3/latest/userguide/intro-lifecycle-filters.html)).
A tag written at upload saying "regenerable" is a claim about the world at upload
time. Whether the output can still be regenerated when the rule fires, 90 days
later, depends on things that change:

- **Inputs live in buckets spawn does not own.** Task `inputs[]` are arbitrary
  caller URIs (`pkg/taskproto/wrapper.go:215`). If one is deleted or overwritten,
  the output becomes irreplaceable, and its tag doesn't change.
- **The recipe is not kept.** For a one-shot task the spec exists only in the
  instance's user data. `completion.json` records no command, image, inputs or
  outputs (`pkg/taskproto/completion.go:40-62`).
- **The image is not pinned.** No digest is recorded anywhere in taskproto, and
  an image tag such as `:latest` can move.

So the design splits the two actions by how reversible they are:

| action | reversible? | who may do it |
|---|---|---|
| **tier** (Standard → IA / Glacier IR) | yes; retrieval cost only | an S3 lifecycle rule, matching a class tag |
| **delete** | **no** | a **verifying sweeper** that re-checks, *at deletion time*, that every input still exists unchanged (ETag/version match) and the pinned recipe is readable, then deletes |

Deleting by lifecycle rule alone would be deleting on a promise nobody checked.

## What is recorded, and where

**Tags** (≤10 per object; lifecycle and IAM can use them):

| tag | example | used by |
|---|---|---|
| `spawn:regen-class` | `cheap` / `moderate` / `expensive` | lifecycle tier rules |
| `spawn:regen-usd` | `0.42` | humans, the sweeper |
| `spawn:regen-hours` | `1.5` | humans, the sweeper |
| `spawn:run-id` | `b3f…` | joins the object to its provenance |
| `spawn:regenerable` | `true`, only when the spec opted in | the sweeper |
| `spawn:retain` | `false` at upload; the researcher sets `true` | **every rule requires `false`** |

**Why `spawn:retain=false` is written at upload.** A lifecycle filter can AND a
prefix, tags and size bounds, but it can't negate anything. So "a rule skips
objects tagged `retain=true`" can't be written. Instead every rule requires
`spawn:retain=false`, and setting `true` takes the object out of every rule.
Objects written before spawn tagged `retain=false` match no rule and are kept.
That is the safe direction. `PutObjectTagging` replaces the whole tag set, so
setting `retain` needs a command that merges (`spawn results retain`, phase 3),
not a bare `put-object-tagging`.

**Provenance**, too large for tags, goes in an **S3 object annotation**
(`PutObjectAnnotation`, name `spawn.provenance`, JSON, ≤1 MiB). Annotations are
encrypted like the object and deleted with it, and are readable on Glacier
objects without a restore
([AWS](https://docs.aws.amazon.com/AmazonS3/latest/userguide/annotations-overview.html)).
The Go SDK in use (s3 v1.114.3) and CLI v2 both support them. They can't be set
at upload, can't be used by lifecycle or IAM, and are unavailable in UAE and
Bahrain (none of spawn's regions). The record holds:

- **recipe:** command argv, env (names only, never values), container image **plus
  the digest that actually ran** (`docker inspect` after the pull, on the
  instance), AMI id, instance type, spawn and spored versions;
- **inputs:** each URI with the **ETag and version id read at stage-in**. This
  is what lets the sweeper prove "unchanged" later;
- **cost:** start and end time, compute seconds, rate, **rate source**
  (`spot`, `on-demand:live`, `on-demand:static`), and a `lower_bound` flag for
  Windows (#884: truffle prices Linux only). A static rate has no collection
  date to record: truffle doesn't expose one (truffle#184). The report says so
  rather than implying a date.

There is one record per run, annotated on every object the run wrote. The
object's own key, size and ETag aren't repeated in it. A prefix input
(`s3://b/refs/`) is recorded object by object with its ETag but no version id,
because `ListObjectsV2` returns none.

The same cost block fills `completion.json`'s `cost_estimate`. That field has
existed all along and no writer sets it.

## Cost: honest or not at all

`regen-usd` = compute seconds × the rate that actually applied:

- the **spot price paid** for spot, otherwise `spawn:price-per-hour`;
- labelled with its source;
- Windows flagged as a lower bound.

An unknown rate gives **no class at all**. Unclassified objects match no rule, so
"we don't know what it cost" means "kept", never "cheap".

Cost is per **run**, so a run that writes many objects charges each of them the
whole run. That's deliberate: regenerating any one of them means rerunning the
whole job.

## Write paths

Surveyed 2026-10-17. Only the first is spawn's to govern.

| path | writer | bucket | plan |
|---|---|---|---|
| **task wrapper** (`spawn task run`, pooled workers) | generated bash, `aws s3 cp` | `spawn-results-<acct>-<region>` (spawn creates it) or caller `results_prefix` / `outputs[].destination` | **Phase 1** |
| queue jobs | spored Go `transfermanager` | user's `result_s3_bucket` | later; tag only, no rules on a user bucket |
| pipeline stages | spored Go `UploadObject` | user's `p.S3Bucket` | later; the orchestrator already computes `StageCostUSD` in DynamoDB |

`aws s3 cp` has no `--tagging`, so the wrapper uploads outputs through
`spored provenance upload` instead. It writes each object exactly where
`aws s3 cp` would (pinned against the real CLI), **with the tags in the same
request**. It then annotates the object with `ObjectIfMatch` on the ETag it just
got back, so a concurrent overwrite is refused rather than annotated. Tagging at
write is the point: spawn never runs `PutObjectTagging` on an object, which
would replace the tag set of something it may not have written.

Two fallbacks keep provenance from ever costing an output:

- the wrapper probes `spored provenance --help` first, and an older spored gets
  plain `aws s3 cp`;
- if `spored provenance upload` fails for **any** reason, the output is uploaded
  untagged with `aws s3 cp`. Untagged means kept.

The wrapper is generated by the CLI that launched the task, but an instance
installs the **latest** spored at boot. Skew between them is normal, which is
why the probe exists and why the fallback isn't optional.

The rate and instance type come from the instance's own tags
(`ec2:DescribeTags`, already granted); instance metadata tags aren't enabled.

## Lifecycle on `spawn-results-*` only

`CreateS3BucketIfNotExists` (`pkg/aws/s3.go`) sets no lifecycle today. Phase 2
adds a configuration on spawn's own results bucket only, never on a caller's
bucket:

- `regen-class=cheap` AND `retain=false` → Standard-IA at 30 days, Glacier IR at 90;
- `regen-class=moderate` AND `retain=false` → Standard-IA at 60;
- `expensive`, unclassified, `retain=true`, and objects with no `retain` tag → no rule.

Objects under 128 KB are not transitioned by default. Most `completion.json` and
`command.log` files are that small, which is fine: they cost little to keep.

## Report first, arm later

The same discipline as every reaper sweep:

1. **Record.** Tags, annotation and `cost_estimate`. Changes nothing about retention.
2. **Report.** `spawn results retention [--bucket] [--prefix]`, read only: per
   run and class, the bytes, monthly storage cost, what tiering would save, and
   **which runs the sweeper could prove regenerable today**, with the reason for
   each that can't be. "Prove" means the sweeper's check run now, without
   deleting: the spec opted in, the provenance annotation is readable, and every
   recorded input still exists with the same ETag (and version id, where
   recorded). An input the caller can't read counts as unproven, not as
   unchanged. Storage prices are a static us-east-1 table taken from the AWS
   Price List API, and every dollar figure is labelled with that source, why
   it's static and the date: truffle, the suite's pricing authority (#533),
   prices EC2 only.
3. **Tier.** The lifecycle configuration on `spawn-results-*`.
4. **Delete.** The verifying sweeper, opt-in, after report runs have been
   read, as with `SpotRequests=reap`.

## Decisions

Made by the owner on 2026-10-17.

1. **Class thresholds:** `cheap` is < \$1 **and** < 1 h. `expensive` is ≥ \$25
   **or** ≥ 12 h. `moderate` is everything between. An unknown rate gives no class.
2. **Deletion is opt-in per spec.** The verifying sweeper may delete only outputs
   whose task spec says `regenerable: true`, and only after re-checking the inputs
   and recipe at deletion time. **Tiering is automatic** by class.
   `spawn:retain=true` excludes an object from both.
3. **Tiering ages:** `cheap` goes to Standard-IA at 30 days and Glacier IR at 90.
   `moderate` goes to Standard-IA at 60. `expensive`, unclassified and
   `retain=true` objects are never tiered.
4. **Phase 1 scope:** task results only (`spawn task run` and pooled workers).
   Queue and pipeline results follow later, tags and annotation only. spawn
   never puts rules on a bucket the user owns on its own initiative; see 6.

Made by the owner on 2026-10-18, for phase 3:

5. **Rules are applied only on request.** `spawn results lifecycle --apply`
   installs them on a bucket, per account. Creating a `spawn-results-*` bucket
   doesn't. That may change to automatic once real reports have been read.
6. **A researcher may apply the same rules to their own bucket**, explicitly:
   `spawn results lifecycle --bucket <theirs> --apply`. This amends Decision 4,
   which kept rules off user buckets entirely. The user runs it; spawn never
   does it for them. This needs documentation, beyond the command's own help.
7. **Outputs written before `spawn:retain=false` existed** (v0.132.x) are not
   tagged retroactively by any automatic step. They are kept. A researcher who
   wants them tiered runs `spawn results retain --off <prefix>`, the same
   merging command that sets `retain=true`, so no separate backfill exists.

## What existed before phase 1

From the 2026-10-17 survey, kept as the baseline. Phase 1 changed the first,
third, fourth and fifth points for task outputs.

- No result object carries tags, except pipeline COMPLETE/FAILED markers, which
  carry metadata.
- No result bucket has a lifecycle rule. Only the schedules bucket does (7-day
  expiry on `schedules/`, `pkg/staging/staging.go:104-133`).
- `completion.json`'s `cost_estimate` is defined and never written.
- Nothing records inputs for one-shot tasks; the pool spec copy is the only
  persisted record (`pool-specs/`).
- No image digest is recorded anywhere.

## Shipping notes

- **Test on an instance, not a workstation.** Phase 1's pre-release check ran
  `spored provenance upload` against real S3 from a laptop with a region
  configured. That proved S3's behaviour, not the instance's environment. On an
  instance, the Go SDK had no region: unlike the AWS CLI, it reads the region
  from IMDS only when asked (`config.WithEC2IMDSRegion`). Every upload failed,
  and with no fallback each output was lost (#1094). The fix added the region
  and the fallback. Exposure was about 69 minutes, the time v0.132.0's spored
  was the latest.
