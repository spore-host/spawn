# Pooled task execution — design

**Status:** Draft / design (no code). Proposed.
**Tracks:** nf-spawn#70 (dispatch-bound fan-out). Related: nf-spawn#69 (per-run
ephemeral FSx), spawn#386 (TaskSpec adapter contract), spawn `pkg/mpicohort`
(cohort adapter precedent), `pkg/queue` (single-instance job runner).
**Scope decision:** reusable **spawn** infrastructure shared by all workflow
adapters (nf-spawn, miniwdl-spawn, snakemake, airflow-spawn), *not* an
nf-spawn-local hack. nf-spawn is the first consumer and the validation case.

---

## 1. Problem (measured, not inferred)

On a real N=108 fan-out (chr20 bcftools variant calling, one `CALL_VARIANTS`
task per genome, nf-spawn 0.8.0, ample quota: queueSize 584 / ~1460 vCPUs free):

- fan-out peaked at **~60 concurrent instances, never 108**;
- it took **5m44s just to dispatch** all 108 tasks (≈ one launch every ~3s);
- per-task `realtime` ≈ **0.6s** (the bcftools work is trivial on chr20).

The launcher — not the workers, not the quota — is the bottleneck.

### Root cause is two-layered

The one-instance-per-task model taxes throughput in two independent ways. Bulk
launch fixes only the first; only worker reuse fixes the second.

**(a) Serial, blocking dispatch.** `SpawnTaskHandler.submit()`
(`nf-spawn/.../SpawnTaskHandler.groovy:186-190`) runs `spawn task run` via
`ProcessBuilder` and **blocks** on `launchProcess.waitFor()`. Nextflow's
`TaskPollingMonitor` calls `submit()` from its submit path, so N submits
serialize. Each `spawn task run` additionally calls `taskproto.Size()` →
truffle `SearchInstanceTypes` + per-candidate pricing lookups
(`spawn/cmd/task.go:367`), **even though nf-spawn always pins `instance_type`**
(`buildTaskSpec`) — pure per-task overhead. Together this is the ~3s/task.

**(b) Boot-per-task tax (the deeper problem).** `Client.Launch` issues
`RunInstances` with `MinCount/MaxCount: 1` (`spawn/pkg/aws/client.go:379`), and
every task is a fresh ephemeral instance paying full boot + container pull +
S3 stage. By Little's Law `concurrency ≈ arrival_rate × time_in_system`: with a
~90s instance lifetime and short tasks, **instances self-terminate faster than
new ones launch**, so steady-state concurrency sits ~30–60 and *can never reach
N no matter how fast dispatch is*. Bulk launch alone would not fix this.

At N=1000 the dispatch cost alone (~50 min) breaks any "finish during a talk"
budget, and the boot tax compounds it.

---

## 2. Design constraints (from the issue)

1. **Best-effort + eventual.** "Ask for N, get M now, top up toward N by a
   deadline." Fewer workers ⇒ lower parallelism, **never failure**. An
   interactive/stage demo must never hang.
2. **Warm pool with idle timeout.** Fungible workers pull tasks from a queue;
   reused workers skip per-task boot *and* keep the shared FSx mount hot
   (compounds with nf-spawn#67/#69). Idle-timeout drain preserves the
   scale-to-zero / no-standing-cluster property.
3. **Capacity fallback.** When a pool is dry (`InsufficientInstanceCapacity`),
   spread across instance-type × AZ × Spot/On-Demand pools instead of erroring.
4. **Cost safety is existential.** No orphaned instances: tag-based reaping +
   idle-timeout so a missed teardown still drains to $0.

---

## 3. What already exists (reuse, don't rebuild)

Confirmed by reading the code:

| Component | What it gives us | File |
|---|---|---|
| `spore-host/cohort` | Reconciliation core: `NewPartialCohort(MinViable)` = "ask N, get M, degrade gracefully"; `Actuator.Start` = **warm-pool resume**; `RungPlacement`+AZ chain = **capacity fallback**; `Assembler` = cohort-scoped **fan-in**. Launches entities via an **unbounded `errgroup`** (`reconcile.go:222`) — per-entity launches are already *concurrent*. | `cohort/{ports,reconcile,cohort}.go` |
| `spawn/pkg/mpicohort` | The **adapter precedent**: implements cohort's Actuator/Observer/Classifier/Enroller/Assembler over spawn's `*aws.Client`. A `taskcohort` sibling follows this exact shape. Its doc explicitly notes it does *not* replace `launchJobArray` — this design is that next stage, for the task-fan-out (easy, per-entity placement) case. | `spawn/pkg/mpicohort/adapter.go` |
| `spawn/pkg/queue` + `userdata.GenerateQueueRunnerUserData` | **Existing prior art**: a *single* instance already pulls and runs many jobs from a queue (`spawn ... --batch-queue`). This is the worker-runner to generalize to a multi-instance pool. | `spawn/pkg/queue/`, `spawn/cmd/launch_batchqueue.go` |
| `spawn` TaskSpec / taskproto wrapper | The stage-in → run → stage-out → durable `completion.json` contract adapters already poll (spawn#386). Becomes the **per-job** runner inside a pooled worker. | `spawn/pkg/taskproto/` |

### Key architectural finding

Because cohort's launch is **already concurrent** (`errgroup`, `reconcile.go:222`),
provisioning a pool of M workers via cohort fires M launches in ~one round-trip
of wall time — *not* M × 3s. **The warm-pool-via-cohort path alone dissolves the
dispatch bottleneck and attacks the boot-per-task tax.** Bulk `RunInstances`
(`MinCount/MaxCount>1`) / `CreateFleet` is then a constant-factor + capacity-spread
optimization, **not** the load-bearing fix — hence deferred to Phase 3.

---

## 4. Proposed architecture

Introduce a **pooled execution mode** in spawn: a set of fungible worker
instances, provisioned as a cohort, that pull `TaskSpec`s from a run-scoped
queue and execute them via the existing taskproto wrapper, cleaning the workspace
between jobs. This is the "many tasks, few reusable workers" inverse of today's
"one instance per task."

```
  workflow adapter (nf-spawn)                    spawn (pooled mode)
  ───────────────────────────                    ────────────────────
  onFlowCreate ──────────────────────────────▶  pool create  (taskcohort:
                                                   PartialCohort MinViable=floor,
                                                   provisions M≤N workers concurrently)
  submit(task) ─ enqueue TaskSpec ────────────▶  run-scoped queue  ◀─┐ workers pull
                 (non-blocking)                                       │ next spec,
  poll completion.json (unchanged) ◀───────────  worker runs taskproto│ clean workdir,
                                                   wrapper per job     │ repeat
  onFlowComplete ────────────────────────────▶  pool drain (idle-timeout +
                                                   tag-reaper backstop → $0)
```

- **Best-effort/eventual** = cohort's `PartialCohort`: dispatch proceeds against
  whatever workers exist; the reconcile loop tops up toward N and replaces reaped
  Spot workers; a short pool just means lower parallelism.
- **Warm pool** = workers persist across jobs (queue pull loop), so per-job cost
  is stage + run only — no boot, no container re-pull, FSx stays mounted.
- **Idle-timeout + tag-reaper** = scale-to-zero preserved; missed teardown still
  drains (existing spore.host reaper + lifecycle invariant).

### Component placement

- **spawn/pkg/taskcohort** (new): cohort adapter for the task-fan-out case —
  Actuator/Observer/Classifier over `*aws.Client`, mirroring `mpicohort`.
  Per-entity placement (no collective fabric) makes this the *easy* case
  `mpicohort` flagged. `Enroller`/`Assembler` nil for the plain pool; `Assembler`
  reserved for an optional log(N) fan-in later.
- **spawn/pkg/queue** (extend): generalize the single-instance job runner to a
  pool-shared queue with per-worker pull + per-task workspace reset.
- **spawn/cmd** (new commands): `spawn pool create|submit|drain|status` (names
  TBD) — the surface adapters call. Reuses taskproto for each job.
- **cohort**: expected **no core change** (verify in Phase 1). `PartialCohort` +
  `Start` + rung fallback already model the semantics. cohort is owned
  (`git@github.com:spore-host/cohort`) and editable, but any change there is a
  coordinated multi-repo tagged release — avoid unless a seam genuinely leaks.
- **nf-spawn** (integration): run-scoped lifecycle hook (see §6).

---

## 5. Phased plan

Every phase preserves best-effort + eventual semantics and the cost-safety
invariant. Each phase is independently shippable and independently measurable.

### Phase 0 — cheap wins, no new architecture (establish the ceiling)
- **spawn:** skip `taskproto.Size()` when `resources.instance_type` is pinned
  (nf-spawn always pins it) — removes the truffle search + pricing lookups from
  every `task run`.
- **nf-spawn:** make `submit()` non-blocking — fire the `spawn task run`
  subprocess and capture its result asynchronously, so RunInstances round-trips
  overlap instead of serializing on `waitFor()`.
- **Outcome:** cuts the ~3s/task dispatch cost and overlaps dispatch. Does **not**
  fix the boot-per-task tax (Little's Law), so peak concurrency improves but is
  still capped. **Re-measure to set the baseline the real fix must beat.**

### Phase 1 — pooled workers (the real fix) — spawn
- `pkg/taskcohort` adapter + queue-backed worker pool + per-job taskproto wrapper.
- `PartialCohort(MinViable=floor)` for graceful degradation; workers pull specs,
  run, **clean the workspace between jobs**, idle-timeout to $0; tag-reaper backstop.
- **Verify cohort needs zero core changes** here; file a cohort issue only if a
  seam genuinely leaks (never PR cohort speculatively).
- Unit/integration-tested against **Substrate** + cohort fakes — no paid AWS.

### Phase 2 — nf-spawn integration
- Run-scoped lifecycle (see §6): one pool per run; `submit()` **enqueues** instead
  of launching; drain `onFlowComplete`.
- Validate on the **real N=108 chr20 run** (paid): TTL set, explicit terminate,
  independent leak-check. Target: peak concurrency → N, dispatch time → seconds.

### Phase 3 (optional) — bulk launch + fan-in
- Bulk `RunInstances` (`MinCount/MaxCount>1`) / `CreateFleet` for homogeneous pool
  provisioning: fewer control-plane calls + instance-type × AZ × Spot/On-Demand
  capacity spread.
- cohort `Assembler` as an optional log(N) tree-merge fan-in over the completed
  cohort.

---

## 6. The nf-spawn integration gap (shared with #69)

nf-spawn's executor is **per-task with no run-level lifecycle hook today** — the
same gap nf-spawn#69 flagged for per-run ephemeral FSx. A pool is a per-*run*
resource, so Phase 2 needs a run-scoped create/drain hook. **Design this hook
once and let both #70 (pool) and #69 (ephemeral FSx) hang off it.**

**Open investigation (Phase 1, before committing Phase 2 shape):** confirm
Nextflow exposes a session/`TraceObserver`(`Factory`) lifecycle to a plugin so we
can create the pool `onFlowCreate` and drain `onFlowComplete`, and thread the pool
id to every `submit()`. If not, fall back options: a deterministic run-scoped pool
name discovered by each `submit()`, or a first-submit-creates + reaper-reaps
pattern (mirrors #69 option 1).

---

## 7. Gotchas (from the issue) → owner

| Gotcha | Handled by |
|---|---|
| **Workdir isolation on reuse** — a reused worker carries prior `*.bam`/`*.vcf.gz`. | Worker resets a clean per-task workspace between jobs (pool runner). |
| **Staggered readiness** — bulk-launched workers reach ready at different times. | cohort per-entity `Observation`; scheduler hands work to each worker as it's ready, never waits for all N. |
| **Spot reclamation mid-run.** | cohort reconcile replaces reaped workers; the in-flight task retries via Nextflow `errorStrategy` (task) + pool (worker). |
| **Per-entity vs collective placement.** | Task fan-out is the *easy* case — per-entity placement is fine, no EFA/placement-group fabric (unlike `mpicohort`). |
| **Teardown reaping.** | Idle-timeout drain + tag-based reaper backstop (spore.host lifecycle invariant). |

---

## 8. Non-goals / risks

- **Not** replacing `launchJobArray` / the one-instance-per-task path — pooled mode
  is additive; single-instance tasks stay the default for small N and long tasks
  (where boot amortization doesn't matter).
- **Risk:** the run-scoped hook may not exist in the Nextflow plugin API →
  Phase 2 shape depends on the §6 investigation. Phase 0/1 are unblocked regardless.
- **Risk:** worker-side queue semantics (at-least-once vs exactly-once pull,
  visibility timeout) need care so two workers can't claim one task; the existing
  `pkg/queue` single-instance model doesn't face this and will need extending.
