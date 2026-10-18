## `spawn results`

Inspect task results in S3

```
spawn results
```

### `spawn results lifecycle`

Install spawn's tiering rules on a bucket (#1076, phase 3).

The rules move task outputs to cheaper storage by what they would cost to make
again, using the tags spawn writes at upload:

  spawn:regen-class=cheap      Standard-IA at 30 days, Glacier IR at 90
  spawn:regen-class=moderate   Standard-IA at 60 days

Each rule also requires spawn:retain=false and a size of at least 128 KiB. So
expensive and unclassified outputs, outputs you have marked with
'spawn results retain', outputs written before v0.133.0 and small files like
logs are never moved. The rules only tier: nothing is ever deleted, and Glacier
Instant Retrieval objects can still be read immediately.

Without --apply this is a dry run: it prints the rules, the bucket's other
lifecycle rules (kept exactly as they are) and whether anything would change.
With --apply it writes the configuration and reads it back to confirm. --remove
takes spawn's rules out again and leaves everything else.

By default it works on this account's results bucket in the configured region.
--bucket names any bucket you own, including your own output buckets. Run
'spawn results retention' first to see what the rules would move.

Tiering has costs, shown by the dry run: a request charge per object moved, a
per-GB charge to read a tiered object back, and AWS's minimum storage duration
(30 days in Standard-IA, 90 in Glacier IR).

Needs s3:GetLifecycleConfiguration and s3:PutLifecycleConfiguration on the
bucket. Every call names your account as the expected bucket owner, so a
mistyped bucket name that belongs to someone else fails instead of being
changed.

```
spawn results lifecycle [flags]
```

**Examples:**

```sh
# What would change on this account's results bucket
  spawn results lifecycle

  # Install the rules there
  spawn results lifecycle --apply

  # Your own output bucket
  spawn results lifecycle --bucket my-lab-results --apply

  # Take spawn's rules out again
  spawn results lifecycle --remove --apply
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--apply` |  | bool |  | Write the change (default: dry run) |
| `--bucket` |  | string |  | Bucket to work on (default: spawn-results-&lt;account&gt;-&lt;region&gt;) |
| `--region` |  | string |  | Region for the default bucket (default: configured region) |
| `--remove` |  | bool |  | Take spawn's rules out instead of installing them |

### `spawn results retain`

Set spawn:retain on task outputs (#1076).

spawn:retain=true takes an output out of every tiering rule, and out of any
future deletion sweep. --off sets spawn:retain=false, which lets the rules apply.
A URI ending in "/" covers every object under that prefix.

--off is also how outputs written before spawn:retain existed (v0.132.x) are
opted in to tiering. They carry no spawn:retain tag, so no rule matches them, and
they are kept until you run this.

S3 replaces an object's whole tag set on every write, so this command reads the
existing tags and writes them back with only spawn:retain changed. Don't use a
bare 'aws s3api put-object-tagging' for this: it would wipe the regeneration
tags the rules match on. Only spawn outputs (objects tagged spawn:run-id) are
changed. Anything else is listed and left alone.

Needs s3:GetObjectTagging and s3:PutObjectTagging on the objects, and
s3:ListBucket for a prefix. On a versioned bucket it also needs
s3:PutObjectVersionTagging, because it writes the tags of the exact version it
read.

```
spawn results retain <s3-uri>... [flags]
```

**Examples:**

```sh
# Keep one output
  spawn results retain s3://my-lab-results/project-x/final.bam

  # Keep a whole run's outputs
  spawn results retain s3://my-lab-results/project-x/run-7/

  # Let older (v0.132.x) outputs be tiered
  spawn results retain --off s3://spawn-results-123456789012-us-east-1/outputs/
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--off` |  | bool |  | Set spawn:retain=false, letting the tiering rules apply |

### `spawn results retention`

Report on task results by what they would cost to make again (#1076).

Since v0.132.1, task outputs are tagged when they're uploaded. The tags give the
run's regeneration class: cheap is under $1 and under 1 hour, expensive is $25
or more, or 12 hours or more, and moderate is in between. Each output also gets
a provenance record of the recipe, the inputs and their ETags, and the cost.
This command reads a bucket and reports, run by run:

  - what is stored, and its monthly storage cost now;
  - what it would cost once the decided tiering rules had moved it. Cheap goes
    to Standard-IA at 30 days and Glacier IR at 90, and moderate goes to
    Standard-IA at 60. Expensive and unclassified outputs, and anything without
    spawn:retain=false, are never moved;
  - whether the deletion sweeper could prove the run regenerable today. That
    needs three things: the spec opted in (regenerable: true), the provenance
    record is readable, and every recorded input still exists with the same
    ETag and version. Each reason a run can't be proven is listed. An input you
    can't read counts as unproven, never as unchanged.

This command NEVER tiers, tags or deletes anything; it is a report. The
tiering rules are installed separately, and only on request, by
'spawn results lifecycle --apply'. No deletion sweeper exists yet.

Storage prices are a static us-east-1 table, and every dollar figure says so.
Objects with no spawn:run-id tag are listed as untracked and kept: logs,
completion records, outputs written before v0.132.1, and outputs uploaded
through the untagged fallback.

```
spawn results retention [flags]
```

**Examples:**

```sh
# This account's default results bucket in the configured region
  spawn results retention

  # A caller bucket, one prefix, with every object listed
  spawn results retention --bucket my-lab-results --prefix project-x/ --objects

  # Machine-readable
  spawn results retention -o json
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--bucket` |  | string |  | Bucket to report on (default: spawn-results-&lt;account&gt;-&lt;region&gt;) |
| `--objects` |  | bool |  | List every object with where tiering would put it |
| `--prefix` |  | string |  | Only objects under this key prefix |
| `--region` |  | string |  | Region for the default bucket (default: configured region) |

