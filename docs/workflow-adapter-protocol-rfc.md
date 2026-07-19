# RFC: Shared Task-Execution Protocol for Workflow Adapters

- **Status:** Draft (for discussion)
- **Tracking issue:** [spawn#386](https://github.com/spore-host/spawn/issues/386)
- **Related:** nf-spawn, miniwdl-spawn, cwl-spawn, snakemake-executor-plugin-spawn, spawn-airflow

## Problem

spore.host has five workflow-engine adapters, each mapping "run this task" from a
different engine onto an ephemeral EC2 instance:

| Adapter | Language | Engine hook |
|---------|----------|-------------|
| nf-spawn | Java/Groovy | Nextflow `TaskHandler` / executor |
| miniwdl-spawn | Python | miniwdl container backend |
| cwl-spawn | Python | cwltool executor |
| snakemake-executor-plugin-spawn | Python | Snakemake executor plugin |
| spawn-airflow | Python | Airflow operator |

Each independently reimplements the **same** machinery:

1. parse the engine's per-task resource request (cpu/mem/gpu/arch)
2. map it to an instance type (via `truffle`)
3. build the S3 input manifest and stage inputs
4. construct user-data / the launch command
5. launch via `spawn` with a TTL + `--on-complete terminate`
6. poll for the durable `.exitcode`-in-S3 completion signal
7. interpret the exit code and classify failures (app error vs capacity vs Spot vs staging)
8. stage outputs back and translate errors to the engine

Exploration confirmed these are **five separate implementations with no shared
library** (a Java repo + four Python repos). The consequences:

- **Drift.** Staging semantics, retry classification, and completion detection
  will diverge subtly across adapters — a bug fixed in one is not fixed in the
  others.
- **Duplicated tests.** Each adapter re-tests the same staging/launch/exit logic.
- **High cost to add an adapter.** A sixth engine (Prefect, Argo, Dagster, …)
  rebuilds all eight steps from scratch.
- **No single source of truth** for "what a spore.host task *is*."

## Goal

Define **one task-execution protocol** — a stable data contract plus a reference
implementation — that every adapter targets. An adapter's only job becomes:
*translate its engine's native task object into a `TaskSpec`, hand it to the
executor, and translate the `CompletionRecord` back.* Everything between is shared.

Non-goals for v1: replacing any engine's DAG/scheduling; a network RPC service
(the contract is a library + S3/tag data format, not a new server); the
capacity-broker and instance-reuse ideas (sketched under "Future").

## Proposed contract

The protocol is a set of language-neutral types (JSON on the wire / in S3) plus a
Go reference implementation in `spawn`. Each adapter either links the Go core (via
CLI/JSON) or implements the same types natively; the **JSON shapes are
authoritative** so cross-language adapters stay compatible.

### Types

```
TaskSpec           one unit of work to run on one ephemeral instance
  task_id          stable id (adapter-assigned; used for logs/idempotency)
  command []string argv (no shell unless the adapter wraps it)
  container        optional image ref (digest preferred over tag)
  resources        ResourceRequest
  inputs  []InputManifest
  outputs []OutputManifest
  lifecycle        { ttl, on_complete }        # always terminate-backed
  env    map                                    # task environment

ResourceRequest    what truffle sizes against
  cpu, memory_gib, gpus
  architecture     x86_64 | arm64 (must match the container/binary)
  families []      allow/deny list (e.g. [c7i,m7i,r7i])
  purchase         spot | on_demand
  fallback         on_demand when spot unavailable
  memory_headroom_percent

InputManifest / OutputManifest
  source           s3://… or local path
  destination      local path or s3://…

LaunchResult       { instance_id, region, az, instance_type, spot }
TaskState          submitted | launching | running | completed | failed | cancelled
CompletionRecord   { exit_code, state, started_at, ended_at, cost_estimate, logs[] }
Cancellation       { task_id, reason }
RetryClassification  app_error | capacity | spot_interruption | staging_error |
                     instance_health | ttl_expired | controller_lost   → retryable?
```

Example `TaskSpec` (JSON) — see issue #386 for the annotated version:

```json
{
  "task_id": "align-sample-42",
  "command": ["bwa", "mem", "ref.fa", "sample.fastq.gz"],
  "container": "quay.io/biocontainers/bwa@sha256:…",
  "resources": { "cpu": 16, "memory_gib": 64, "architecture": "x86_64", "purchase": "spot", "fallback": "on_demand" },
  "inputs":  [{ "source": "s3://bucket/sample.fastq.gz", "destination": "/work/sample.fastq.gz" }],
  "outputs": [{ "source": "/work/result.bam", "destination": "s3://bucket/results/result.bam" }],
  "lifecycle": { "ttl": "4h", "on_complete": "terminate" }
}
```

### Reference implementation

A `spawn` package (proposed `pkg/taskproto`) owns: `TaskSpec`→launch translation
(reusing existing `pkg/params`, truffle sizing, and the launch path), the S3
`.exitcode` completion protocol (already the de-facto signal), and
`RetryClassification` from AWS/launch errors. Adapters call it one of two ways:

- **Go-native** (future adapters): import the package.
- **JSON/CLI** (the four Python adapters + Java): a `spawn task run --spec spec.json`
  entry point that consumes a `TaskSpec` and emits a `CompletionRecord`, so an
  adapter shells out rather than reimplementing. (This composes with `spawn task
  diagnose`, #391.)

## Migration

1. Land the types + `pkg/taskproto` + `spawn task run` in `spawn` (behind the new
   contract; no adapter change yet).
2. Port **one** adapter as the proving ground — cwl-spawn or snakemake (both are
   Python, both already AWS-verified at v0.1.0) — to shell out to `spawn task run`.
   Compare against its current path on a real workflow.
3. Port the remaining Python adapters.
4. nf-spawn (Java) adopts the JSON/CLI path; a native JVM binding is optional
   later.
5. Delete each adapter's now-duplicated staging/launch/exit code.

Each step is independently shippable; adapters not yet ported keep working.

## Future (out of scope for v1)

- **Capacity broker** — fold truffle sizing + quota check + Spot fallback +
  `lagotto` waiting behind one placement policy the executor calls, instead of
  each adapter calling `spawn` directly. Natural once the protocol exists.
- **Short-task instance reuse / pooling** — for sub-minute tasks where a fresh
  EC2 boot dominates runtime, serve several tasks from one warm instance.
- **Execution manifest** — emit a machine-readable provenance record per task
  (spawn+adapter+engine versions, AMI, instance type, arch, region/AZ, container
  digest, command, env, spot/on-demand, timings, exit code, termination reason)
  for reproducibility. Overlaps `CompletionRecord`; could be an extension of it.

## Decisions needed

1. **Hosting:** a new package in `spawn` (`pkg/taskproto`) vs. a standalone
   `spore-host/taskproto` repo with Go + Python packages. (Leaning `spawn` for v1
   to avoid a release-coupling problem; revisit if a Python-native binding is
   wanted.)
2. **Adapter integration surface:** is `spawn task run --spec` (shell-out) the
   committed contract for non-Go adapters, or do we publish a versioned JSON
   schema they each validate against independently?
3. **Versioning:** how the protocol version is carried in `TaskSpec` and how
   adapters negotiate a spawn that predates a field.
4. **First port:** cwl-spawn or snakemake as the proving ground?
5. **Scope of v1 `RetryClassification`:** which error classes are in the first cut
   vs. deferred.
