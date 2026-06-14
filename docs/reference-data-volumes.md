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

### Local scratch space and memory

When `--from` is a **directory or tarball**, spawn builds the ext4 image in a
**local temporary file** before uploading it (the ext4 builder needs seekable
scratch space). That temp file is roughly the size of the **uncompressed** data
— e.g. a 16 GB Kraken2 DB needs ~16 GB of free disk while `snapshot create` runs.
It's removed when the command finishes.

Use **`--temp-dir`** to put that scratch file somewhere other than the system
temp directory — e.g. a roomy external disk:

```bash
spawn snapshot create --from ./k2_pluspf.tar.gz --size 20 --name kraken2 \
  --temp-dir /Volumes/BigDisk/spawn-tmp
```

Memory stays low regardless of image size: the upload streams the image
block-by-block (peak RAM is a small bounded buffer, not the image size), and the
blocks are uploaded concurrently to fill the link.

A **raw image** source needs no scratch at all: it streams source → snapshot
directly.

> Tip: if you rebuild from the same tarball often, convert it to a raw image once
> (point `--from` at the resulting `.raw` thereafter) to skip the per-build
> conversion and scratch-space cost.

The snapshot source read (including `s3://`) is streamed; only the
directory/tarball → ext4 conversion stages to disk.

### Build in-region for large uploads

The snapshot upload sends the (uncompressed) image to AWS over your connection —
e.g. ~12–16 GB for a Kraken2 DB. Over a home/office uplink that is bandwidth-bound
and can take a long time regardless of the streaming/parallelism above. To avoid
the round trip, run `spawn snapshot create` **from inside AWS** — AWS CloudShell
or a small short-lived EC2 instance in the target region — where the upload is
AWS-internal and fast. Build once there, then `--attach-volume` the snapshot from
anywhere.

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
