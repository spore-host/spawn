## `spawn snapshot`

Create EBS snapshots from raw disk images without launching an EC2
instance, so large reference data (a Kraken2 DB, BLAST index, ML weights) can be
attached to spores via 'spawn launch --attach-volume' instead of being baked
into a custom AMI.

```
spawn snapshot
```

### `spawn snapshot create`

Populate a new EBS snapshot directly using the EBS direct APIs — no EC2
instance and no attached volume. The result is a snapshot you can attach with
'spawn launch --attach-volume snap-xxx:/mount'.

--from accepts any of:
  - a directory      → its contents are packed into an ext4 filesystem image
  - a .tar/.tar.gz/.tgz archive → unpacked into an ext4 filesystem image
  - a raw disk image → streamed verbatim (its bytes ARE the block device)

Directories and tarballs are converted to ext4 in-process (pure Go — no mkfs and
no builder instance), so this works the same from macOS, Linux, or Windows. The
ext4 filesystem is sized to the data and capped at --size.

For a directory or tarball source, the ext4 image is built in a local temp file
first (~the uncompressed data size — e.g. ~16 GB for a 16 GB DB) and removed when
done; ensure that much free space, or use --temp-dir to point at a roomier disk.
A raw image needs no scratch (it streams source → snapshot directly). Memory stays
low regardless of size (the upload streams blocks concurrently, never buffering
the whole image).

The upload sends the uncompressed image to AWS over your connection; for a large
DB over a slow uplink, run this from AWS CloudShell or a small in-region EC2
instance so the upload is AWS-internal.

Examples:
  # From a directory:
  spawn snapshot create --from ./kraken2-db/ --size 20 --name kraken2-k2pluspf

  # From a tarball (local or in S3):
  spawn snapshot create --from ./k2_pluspf.tar.gz --size 20 --name kraken2
  spawn snapshot create --from s3://genome-idx/k2_pluspf.tar.gz --size 20 \
    --name kraken2 --region us-east-1

  # From a raw filesystem image you already built:
  spawn snapshot create --from ./kraken2.raw --size 20 --name kraken2

```
spawn snapshot create [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--description` |  | string |  | Snapshot description |
| `--encrypted` |  | bool |  | Create an encrypted snapshot |
| `--from` |  | string |  | Source: a directory, a .tar/.tar.gz/.tgz, or a raw disk image — local path or s3://bucket/key (required) |
| `--kms-key` |  | string |  | Customer-managed KMS key ARN for encryption (implies --encrypted) |
| `--name` |  | string |  | Name tag for the snapshot (also sets spawn:snapshot-name) |
| `--region` |  | string |  | AWS region (default: the configured region) |
| `--size` |  | int64 |  | Volume size in GiB the snapshot is built for; the image must fit (required) |
| `--tag` |  | stringArray |  | Custom tag key=value to set on the snapshot (repeatable). Merged with the spawn:* baseline; cannot override a spawn: tag. |
| `--temp-dir` |  | string |  | Directory for the temporary ext4 image built from a dir/tarball source (default: system temp). Point at a roomy disk for large data. |

### `spawn snapshot mount`

Convenience for the head-node side of the reference-data-volume recipe:
create an EBS volume from a snapshot, attach it to the instance this command runs
on, and mount it (read-only by default) at &lt;mount-point&gt;.

This only works when run ON an EC2 instance (it identifies itself via IMDS). It's
the one-command equivalent of: aws ec2 create-volume --snapshot-id … &&
aws ec2 attach-volume … && sudo mount -o ro …. Use it on a spawn head node (or
any EC2 box running 'nextflow run') so an nf-core pipeline's head-side db_path
validation finds the DB. Tasks don't need this — 'spawn launch --attach-volume'
mounts the volume on each task automatically.

Example:
  sudo spawn snapshot mount snap-0abc123 /opt/databases/kraken2

```
spawn snapshot mount <snapshot-id> <mount-point> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--rw` |  | bool |  | Mount read-write (default: read-only — the right choice for shared reference data) |

