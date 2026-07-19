## `spawn image`

Create custom AMIs that spawn can launch.

Currently supports importing a Windows 11 ISO into an AMI via AWS EC2 Image
Builder's managed import-disk-image workflow (drivers, EC2Launch, SSM agent and
Defender are pre-staged automatically). See infra/amis/windows/README.md.

```
spawn image
```

### `spawn image import`

Convert a Windows 11 ISO into an AMI using EC2 Image Builder's managed
import-disk-image workflow, then tag it so 'spawn launch --os windows' can use it.

The ISO must be a SUPPORTED, NON-evaluation Windows 11 Enterprise image
(23H2 / 24H2 / 25H2 x64) obtained from the Microsoft 365 admin center. Evaluation,
Media-Creation-Tool, and LTSC ISOs are rejected by the service. Bring your own
Microsoft license (BYOL).

The command self-provisions the IAM roles and Image Builder infrastructure
configuration it needs (idempotent); pass --infra-config-arn only to reuse an
existing/custom one. See infra/amis/windows/README.md.

Examples:
  # Local ISO — staging bucket + infra auto-provisioned, nothing to pre-create:
  spawn image import --iso ./Win11_25H2_Enterprise.iso \
    --name win11-25h2 --image-index 3

  # ISO already in S3 (uppercase .ISO key required by the service):
  spawn image import --iso s3://my-bucket/Win11_25H2_Enterprise.ISO \
    --name win11-25h2

```
spawn image import [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--bucket` |  | string |  | S3 bucket to stage a local ISO in (default: managed spawn-iso-import-<account>-<region>, auto-created) |
| `--execution-role` |  | string | `AWSServiceRoleForImageBuilder` | IAM execution role name or ARN |
| `--image-index` |  | int64 | `1` | 1-based edition index in a multi-edition ISO |
| `--infra-config-arn` |  | string |  | Image Builder infrastructure configuration ARN (optional; self-provisioned if omitted) |
| `--instance-type` |  | string | `m6i.large` | Build instance type (when self-provisioning infra) |
| `--iso` |  | string |  | Windows 11 ISO: local path or s3://bucket/key.ISO (required) |
| `--keep-iso` |  | bool |  | Keep the staged ISO (and managed bucket) after the AMI is built; by default they are deleted (only applies with --wait) |
| `--name` |  | string |  | Image Builder image resource name (required) |
| `--no-secure-boot` |  | bool |  | Disable Secure Boot on the output AMI |
| `--no-warm` |  | bool |  | Skip building the warm (fast-boot) AMI; produce only the raw imported base AMI |
| `--region` |  | string | `us-east-1` | AWS region for the import build |
| `--s3-key` |  | string |  | S3 object key for the uploaded ISO (default: derived from filename, .ISO) |
| `--security-group-ids` |  | stringSlice |  | Security groups for the build instance (comma-separated or repeated; when self-provisioning infra) |
| `--subnet-id` |  | string |  | Subnet for the build instance (when self-provisioning infra) |
| `--version` |  | string | `1.0.0` | Semantic version for the output image (major.minor.patch) |
| `--wait-timeout` |  | int | `60` | Max minutes to wait when --wait is set before detaching (the build keeps running) |
| `--wait` |  | bool |  | Wait for the AMI to finish building, then tag and clean up (warm mode implies this) |
| `--warm-instance-type` |  | string | `m7i.xlarge` | Instance type for the warm-build seed (non-burstable; Windows) |
| `--warm-timeout` |  | int | `30` | Safety-net minutes to wait for the warm seed's first boot (Administrator password) before giving up |

### `spawn image status`

Report the current state of an EC2 Image Builder image build started by
'spawn image import' (without --wait). Prints PENDING/BUILDING/.../AVAILABLE/FAILED
and, once available, the output AMI id.

Example:
  spawn image status arn:aws:imagebuilder:us-east-1:123456789012:image/win11-25h2/1.0.0/1

```
spawn image status <image-build-version-arn> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--region` |  | string | `us-east-1` | AWS region of the image build |

### `spawn image verify`

Inspect a local Windows installation ISO and report which editions it
contains and whether 'spawn image import' (EC2 Image Builder import-disk-image)
will accept it — before you spend a real, paid build.

import-disk-image accepts only Windows 11 Enterprise (23H2/24H2/25H2, x64),
non-Evaluation. This reads the ISO's install.wim metadata directly (no mount, no
external tools) and prints each edition with its image index, flags the one to
use, and gives a clear ACCEPTED/REJECTED verdict.

Examples:
  spawn image verify "/Volumes/External HD/Win11_Enterprise_25H2.iso"
  spawn image verify win11.iso -o json

```
spawn image verify <path-to.iso>
```

