## `spawn ami`

Create, list, copy and delete AMIs created by spawn.

Examples:
  spawn ami list
  spawn ami list --stack pytorch --arch arm64
  spawn ami create my-instance --name pytorch-2.4-cuda12
  spawn ami copy pytorch-2.4-cuda12 --to-region us-east-2 --wait

```
spawn ami
```

### `spawn ami copy`

Copy a spawn-managed AMI to another region, keeping its spawn tags on the new
AMI and on every snapshot backing it, plus spawn:copied-from=&lt;ami-id&gt;@&lt;region&gt;.
The copy is spawn-managed like its source: 'spawn ami list --region &lt;region&gt;'
lists it, 'spawn footprint' counts it and its snapshots, and
'spawn ami delete &lt;copy-id&gt; --region &lt;region&gt;' removes it.

Only AMIs spawn created (tagged spawn:managed=true) can be copied.

Encryption: an encrypted source is copied encrypted with the target region's
default EBS key (aws/ebs), because a KMS key cannot be used outside its own
region. --kms-key names a key in the target region instead, and also encrypts
the copy of an unencrypted source.

The copy is independent of its source: deleting the source does NOT delete
its copies. Delete each copy in its own region.

Needs ec2:DescribeImages in the source region and ec2:CopyImage and
ec2:CreateTags in the target region.

Examples:
  spawn ami copy ami-0123456789abcdef0 --to-region us-east-2
  spawn ami copy pytorch-2.4-cuda12 --region us-east-1 --to-region us-west-2 --wait
  spawn ami copy ami-0123456789abcdef0 --to-region eu-west-1 --kms-key alias/research

```
spawn ami copy <ami-id|name> --to-region <region> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--kms-key` |  | string |  | KMS key in the target region to encrypt the copy with (default: aws/ebs if the source is encrypted) |
| `--name` |  | string |  | Name for the copy (default: the source AMI's name) |
| `--region` |  | string |  | Region the source AMI is in (default: current region from AWS config) |
| `--to-region` |  | string |  | Region to copy the AMI to (required) |
| `--wait-timeout` |  | duration | `1h0m0s` | How long --wait waits before giving up (the copy itself continues) |
| `--wait` |  | bool |  | Wait for the copy to become available |

### `spawn ami create`

Create an AMI from a running instance

```
spawn ami create <instance-id-or-name> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--description` |  | string |  | Description for the AMI |
| `--force` |  | bool |  | Create the image even if spawn could not remove its --command run-state files (completion file, exit code, the embedded command) from the instance first; they will be baked into the AMI |
| `--name` |  | string |  | Name for the AMI (required) |
| `--reboot` |  | bool |  | Reboot instance before creating AMI (default: no-reboot) |
| `--tag` |  | stringArray |  | Tags in key=value format |
| `--wait` |  | bool |  | Wait for AMI to become available |

### `spawn ami delete`

Deregister a spawn-managed AMI and delete its backing EBS snapshots in one
step. If the AMI was produced by EC2 Image Builder (e.g. 'spawn image import'),
the corresponding Image Builder image resource is also deleted so its
name/version is freed.

This is irreversible. Use 'spawn ami list' to find AMIs.

Examples:
  spawn ami delete ami-0123456789abcdef0
  spawn ami delete ami-0123456789abcdef0 --region us-east-1 --yes

```
spawn ami delete <ami-id> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--region` |  | string |  | AWS region (default: current region from AWS config) |
| `--yes` | `-y` | bool |  | Skip the confirmation prompt |

### `spawn ami list`

List spawn-managed AMIs

```
spawn ami list [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--arch` |  | string |  | Filter by architecture (x86_64 or arm64) |
| `--deprecated` |  | bool |  | Show deprecated AMIs |
| `--gpu` |  | string |  | Filter by GPU support (true or false) |
| `--region` |  | string |  | AWS region (default: current region from AWS config) |
| `--stack` |  | string |  | Filter by stack (spawn:stack tag) |
| `--version` |  | string |  | Filter by version (spawn:version tag) |

### `spawn ami snapshots`

Show the EBS snapshots that back an AMI, with size, state, and whether each
snapshot is shared with other AMIs (which is why 'spawn ami delete' keeps shared
snapshots instead of deleting them).

Examples:
  spawn ami snapshots ami-0123456789abcdef0
  spawn ami snapshots ami-0123456789abcdef0 -o json

```
spawn ami snapshots <ami-id> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--region` |  | string |  | AWS region (default: current region from AWS config) |

