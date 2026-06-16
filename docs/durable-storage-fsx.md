# Durable storage & FSx for Lustre

This is the **canonical reference** for FSx Lustre with spawn — how to give a job
high-throughput POSIX storage backed by S3, and (critically) **how to choose its
lifetime so it can't silently cost you money or lose your results.**

> For read-mostly reference data (databases, indexes, model weights) that doesn't
> change during a run, prefer an EBS [reference-data volume](reference-data-volumes.md) —
> it's cheaper and simpler. Reach for FSx Lustre when you need a **shared,
> high-throughput, writable** filesystem with **continuous S3 export** of results.

## 1. Pick a lifetime first

An FSx Lustre filesystem **bills continuously (~$150–200/mo for 1.2 TB
PERSISTENT_2) whether or not a job is using it**, and it often holds the *only*
copy of a job's output. So spawn makes you state its lifetime **explicitly** —
there is no default. Decide before you read any flags:

| You want | Use | Reaped when |
|----------|-----|-------------|
| Storage that lives and dies with one job | `--fsx-create --fsx-lifecycle ephemeral` | the instance terminates (no live user left) |
| Storage that persists across many jobs | `--fsx-create --fsx-lifecycle durable --fsx-ttl 30d` | nothing has used it **and** its TTL has passed |
| To mount a filesystem you already have | `--fsx-id fs-0abc…` | spawn doesn't reap it — it's yours |
| `--fsx-create` **without** `--fsx-lifecycle` | — | **error** (choose a lifetime) |

This is **fail-closed**: a launch that creates an FSx without a lifecycle is
rejected, and `durable` without a `--fsx-ttl` is rejected. You cannot
accidentally create a filesystem that bills forever.

## 2. What each lifetime costs you

- **`ephemeral`** ties the filesystem's cost to a single job. It's created
  **asynchronously** — `spawn launch` fires the create and returns in seconds; the
  instance (via spored) waits for the filesystem to become AVAILABLE, sets up the
  S3 export association, and mounts it (~10 min, overlapping boot/staging). It's
  created in the **same AZ as the instance** (no constraint on where the instance
  launches), and reclaimed automatically when the instance terminates. Best for
  hands-off, one-shot jobs. **No TTL needed** — the instance *is* the lifetime.
  Because the create is non-blocking, this is the path the lagotto capacity-poller
  uses (it never blocks waiting on FSx).
- **`durable`** keeps billing until its TTL, surviving crashes, idle periods, and
  "the job never ran." That's the point — but it also means a **forgotten durable
  FSx is a recurring bill**, which is why the TTL is mandatory. It also **pins
  every job that mounts it to the filesystem's AZ** (FSx Lustre is single-AZ and
  mounts are same-AZ), so it trades AZ flexibility for persistence — fine for
  standing storage, bad for chasing scarce capacity.

## 3. Make sure results actually reach S3

FSx with an **export** data-repository association mirrors writes back to your S3
bucket. Configure it so results land in S3 **continuously**, not only at the end:

```sh
spawn launch myjob --instance-type c7g.4xlarge \
  --fsx-create --fsx-lifecycle ephemeral \
  --fsx-s3-bucket my-bucket \
  --fsx-export-path s3://my-bucket/results/ \
  --command 'run-pipeline --out /fsx/results'
```

Ephemeral filesystems are deleted when the job ends. spawn never sets
`SkipFinalExport`, so any remaining changes flush to S3 on delete — but a
**continuous** export DRA means you're protected even if the instance dies
unexpectedly. (Don't rely on a single end-of-job flush: that's the
[data-loss shape](https://github.com/spore-host/spawn/issues/184) we designed
this around.)

## 4. Durable, shared storage

> **Status:** the `durable` lifecycle contract is in place; the standalone
> `spawn fsx create --name` / `--fsx-name` mount-by-name commands shown below
> land in [#195](https://github.com/spore-host/spawn/issues/195). Until then, use
> `--fsx-create --fsx-lifecycle durable --fsx-ttl …` on a launch, or pre-create
> out of band and mount with `--fsx-id`.

When several jobs share one standing filesystem:

```sh
# create once (this blocks until AVAILABLE, ~10 min)
spawn fsx create --name prospecting --s3-bucket my-bucket \
  --export s3://my-bucket/detections/ --ttl 30d

# then jobs mount it — fast, no creation wait
spawn launch job1 --instance-type g5.4xlarge --fsx-name prospecting --az us-east-1a
```

Every job that mounts it must run in that filesystem's AZ. The TTL is enforced
**only when no instance is using it** — an active job always wins over the clock.

## 5. Just mount one you manage yourself

If you create and tear down FSx out of band, skip all of the above and mount by
id — spawn won't try to reap it:

```sh
spawn launch myjob --fsx-id fs-0abc1234 --fsx-mount-point /fsx
```

## 6. The reaper's promise (why this is safe)

Every spawn-**created** FSx is tracked by the
[ttl-reaper](https://github.com/spore-host/spawn/issues/192): it reclaims a
filesystem only when it is **past its deadline** *and* has **no live instance
still using it** (a refcount derived from the `spawn:fsx-id` tag each mounting
instance carries). A single active job blocks reclamation; an orphaned filesystem
(its job crashed, capacity never came) is reclaimed automatically. Deletion
flushes the export DRA to S3 first. So nothing leaks silently, and nothing
in-use is reaped out from under you.

---

*This doc is the source of truth for FSx lifetimes. lagotto's spawn-config docs
link here rather than restating the rules.*
