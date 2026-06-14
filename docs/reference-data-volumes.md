# Reference data volumes (no AMI baking)

Large read-mostly reference data — a Kraken2 database, a BLAST index, MetaPhlAn
DBs, ML model weights — often needs to be on the instance but is awkward to bake
into a custom AMI: it welds a data blob into the machine image, forces a per-arch
rebake, bloats every root volume, and turns a data update into an AMI rebuild.

spawn separates the data from the machine image with two pieces:

1. **`spawn snapshot create`** — build an EBS snapshot from your data, with **no
   EC2 instance** (the EBS direct APIs write the snapshot directly).
2. **`spawn launch --attach-volume`** — attach a volume created from that
   snapshot to a spore at launch, mounted at a path (read-only by default), on a
   **stock AMI**.

Update the data by re-snapshotting; no AMI rebuild. The root volume stays small.

## Build a snapshot

```bash
# From a directory:
spawn snapshot create --from ./kraken2-db/ --size 20 --name kraken2-k2pluspf

# From a tarball (local or in S3) — .tar, .tar.gz, .tgz:
spawn snapshot create --from ./k2_pluspf.tar.gz --size 20 --name kraken2
spawn snapshot create --from s3://genome-idx/k2_pluspf.tar.gz --size 20 \
  --name kraken2 --region us-east-1

# From a raw filesystem image you already built:
spawn snapshot create --from ./kraken2.raw --size 20 --name kraken2
```

`--from` accepts:

| Source | What spawn does |
|--------|-----------------|
| a **directory** | packs its contents into an ext4 filesystem image, then snapshots |
| a **`.tar` / `.tar.gz` / `.tgz`** | unpacks into an ext4 filesystem image, then snapshots |
| a **raw disk image** | streams the bytes verbatim into the snapshot (no conversion) |

Directory and tarball inputs are converted to ext4 **in-process, in pure Go** —
no `mkfs`, no builder instance — so it works the same from macOS, Linux, and
Windows. `--size` is the volume size the snapshot is built for; the filesystem is
sized to the data and capped at `--size` (the data must fit).

### Local scratch space

When `--from` is a **directory or tarball**, spawn builds the ext4 image in a
**local temporary file** before streaming it into the snapshot (the ext4 builder
needs seekable scratch space). That temp file is roughly the size of the
**uncompressed** data — e.g. a 16 GB Kraken2 DB needs ~16 GB of free space in the
system temp directory while `snapshot create` runs. It's removed when the command
finishes.

A **raw image** source needs no scratch: it streams source → snapshot directly.

> Tip: if you rebuild from the same tarball often, convert it to a raw image once
> (point `--from` at the resulting `.raw` thereafter) to skip the per-build
> conversion and scratch-space cost.

The snapshot source read (including `s3://`) is streamed; only the
directory/tarball → ext4 conversion stages to disk.

## Attach it to a spore

```bash
spawn launch run1 \
  --instance-type r7g.2xlarge \
  --attach-volume snap-0abc123:/opt/databases/kraken2:ro
```

`--attach-volume` takes `snapshot:mountpoint[:ro|:rw]` and is **repeatable** for
multiple volumes. Read-only (`:ro`) is the default and the common case for shared
reference data; use `:rw` for a writable scratch volume.

- A fresh volume is created from the snapshot at launch and **deleted when the
  instance terminates** (`DeleteOnTermination`) — the per-task copy dies with the
  ephemeral spore; the snapshot persists and is reused.
- The mount is **NVMe-aware**: the requested device (e.g. `/dev/sdf`) is resolved
  to the real device on Nitro instances, so the data lands at your mount point
  regardless of device renaming.
- Snapshot-backed volumes are **never reformatted**.

A read-only reference volume fanned out across many instances means many EBS
volumes created from one snapshot — that's expected, and is the cost you're
trading against a per-task S3 download or an oversized root volume.

> **Wide fan-out note:** a volume created from a snapshot lazy-loads blocks from
> S3 on first access, so a large fan-out can each pay first-touch latency. Fast
> Snapshot Restore (FSR) pre-warms a snapshot to remove that — tracked
> separately (it bills per-AZ-hour, so it's an explicit opt-in).

## From Nextflow (nf-spawn)

The [nf-spawn](https://github.com/spore-host/nf-spawn) executor plugin exposes
this per process via `ext.volumes`:

```groovy
process {
    withName: 'KRAKEN2_KRAKEN2' {
        ext.instanceType = 'r7g.2xlarge'
        ext.volumes = [[ snapshot: 'snap-0abc', mount: '/opt/databases/kraken2', readOnly: true ]]
    }
}
```

Each entry maps to a `spawn launch --attach-volume`. Requires spawn ≥ 0.46.0
(`ext.volumes` shipped in nf-spawn 0.3.0).
