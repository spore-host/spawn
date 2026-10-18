## `spawn results`

Inspect task results in S3

```
spawn results
```

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

This command NEVER tiers, tags or deletes anything; it is a report. No tiering
rule or sweeper exists yet. They come in later phases, after reports like this
one have been read.

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

