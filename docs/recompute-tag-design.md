# Retention by regeneration cost (#1076)

**Status: DESIGN — decided 2026-10-17 (see [Decisions](#decisions)).** Phase 1 in progress.

## The idea

When spawn writes a result to S3, it records what it would cost to make again:
compute used, the hourly rate and its source, output size, and what produced it.
Then retention follows from cost. Cheap-to-regenerate outputs can be tiered or
deleted by rule, without asking the researcher. Expensive ones are kept.

It is the data-side counterpart of the TTL reaper: today nothing ever removes a
result object (see [the survey](#what-exists-today)).

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
| `spawn:retain` | `true` (set by the researcher) | **excludes the object from every rule** |

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
  (live / static-with-date / spot-actual), and a `lower_bound` flag for Windows
  (#884: truffle prices Linux only);
- **output:** key, bytes, ETag.

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

The wrapper can't tag through `aws s3 cp`, which has no `--tagging`. After each
upload it lists what it wrote and calls `s3api put-object-tagging` and
`s3api put-object-annotation` per object.
`PutObjectTagging` replaces the whole tag set. These objects were just written
without tags, so that's safe, but it must never run on an object spawn didn't
just write. The rate and instance type come from the instance's own tags
(`ec2:DescribeTags`, already granted); instance metadata tags aren't enabled.

## Lifecycle on `spawn-results-*` only

`CreateS3BucketIfNotExists` (`pkg/aws/s3.go`) sets no lifecycle today. Phase 2
adds a configuration on spawn's own results bucket only, never on a caller's
bucket:

- `regen-class=cheap` → Standard-IA after *N* days, Glacier IR after *M*;
- `regen-class=moderate` → Standard-IA after *N′*;
- `expensive`, unclassified and `retain=true` → no rule.

Objects under 128 KB are not transitioned by default. Most `completion.json` and
`command.log` files are that small, which is fine: they cost little to keep.

## Report first, arm later

The same discipline as every reaper sweep:

1. **Record.** Tags, annotation and `cost_estimate`. Changes nothing about retention.
2. **Report.** `spawn results retention [--prefix]`, a dry run: per run and
   class, the bytes, monthly cost, what tiering would save, and **which outputs
   the sweeper could prove regenerable today** (and why not, for the rest).
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
   Queue and pipeline results follow later, tags and annotation only, with no
   rules on buckets the user owns.

## What exists today

From the 2026-10-17 survey:

- No result object carries tags, except pipeline COMPLETE/FAILED markers, which
  carry metadata.
- No result bucket has a lifecycle rule. Only the schedules bucket does (7-day
  expiry on `schedules/`, `pkg/staging/staging.go:104-133`).
- `completion.json`'s `cost_estimate` is defined and never written.
- Nothing records inputs for one-shot tasks; the pool spec copy is the only
  persisted record (`pool-specs/`).
- No image digest is recorded anywhere.
