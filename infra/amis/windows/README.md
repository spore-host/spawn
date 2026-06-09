# Building a Windows 11 AMI from an ISO

Turn a Windows 11 ISO into an EC2 AMI that spore.host tools can launch with
`spawn launch --ami <id> --os windows`.

This uses **AWS EC2 Image Builder's managed `import-disk-image` workflow** (the
supported, AWS-native Windows-ISO→AMI conversion). Image Builder runs the
install, stages the AWS guest components, and registers the AMI for you — there
is no Packer, no qemu, no manual `import-image`. `spawn image import` wraps the
whole thing.

> **Self-service in _your own_ AWS account.** You supply the ISO (and its
> license); Image Builder creates the AMI in your account; spawn launches from
> it. For the spore.host team, "your account" is the **dev** account.

---

## ⚠️ Constraints — read first

1. **Supported ISOs only.** Image Builder accepts **Windows 11 Enterprise
   23H2 / 24H2 / 25H2 (x64)**. It **rejects**: Evaluation images,
   Media-Creation-Tool ISOs, and LTSC. Get the ISO from the **Microsoft 365
   admin center** (not the consumer Media Creation Tool).
2. **BYOL.** Microsoft licensing is not included with the import — bring your own
   license. See the [AWS + Microsoft licensing FAQ](https://aws.amazon.com/windows/faq/#licensing-q).
   Don't distribute the resulting AMI.
3. **S3 + region.** The ISO must be in S3 in the **same account and region** as
   the import, and the object key must end in an **uppercase `.ISO`** (the
   service is case-sensitive). `spawn image import` uploads a local ISO with the
   correct key for you.
4. **Networking.** In `us-east-1` the S3 gateway endpoint is enough. In other
   regions the build instance needs **NAT egress** to download the AWS drivers
   (`ec2-windows-drivers-downloads.s3.amazonaws.com`); without it the import
   fails. (Microsoft Defender always needs public egress, but its absence only
   skips Defender — the import still succeeds.)

The import auto-installs onto the output AMI: ENA / NVMe / PCISerial /
EC2WinUtil drivers, EC2Launch v2, the SSM agent, and Microsoft Defender, and
sets the Amazon time server. The AMI runs **Sysprep Specialize** at first launch.

---

## Quick start

```bash
# Local ISO → AMI. spawn uploads the ISO, self-provisions the IAM roles +
# Image Builder infrastructure configuration, imports, polls, and tags the AMI.
spawn image import \
  --iso "/Volumes/External HD/Win11_25H2_Enterprise.iso" \
  --bucket my-iso-bucket \
  --name win11-25h2 \
  --version 1.0.0

# ...prints the new AMI id. Launch it (Windows requires a lifetime — #72):
spawn launch winbox --ami <ami-id> --os windows --ttl 4h
```

If the ISO is already in S3 (remember the uppercase `.ISO` key):

```bash
spawn image import --iso s3://my-iso-bucket/Win11_25H2_Enterprise.ISO --name win11-25h2
```

---

## What `spawn image import` does

1. **Ensures the execution role** — the Image Builder service-linked role
   `AWSServiceRoleForImageBuilder` (created if absent).
2. **Uploads the ISO** to `--bucket` with an uppercase `.ISO` key (skipped if
   `--iso` is already an `s3://…` URI).
3. **Self-provisions the build infrastructure** (idempotent), unless you pass
   `--infra-config-arn`:
   - an IAM instance-profile role `spawn-imagebuilder-iso-import` with the
     managed policies `EC2InstanceProfileForImageBuilder` +
     `AmazonSSMManagedInstanceCore`, and its instance profile;
   - an Image Builder infrastructure configuration `spawn-iso-import`
     (`--instance-type`, optional `--subnet-id` / `--security-group-id`,
     `TerminateInstanceOnFailure=true`).
4. **Starts `import-disk-image`** (`Platform=Windows`,
   `OsVersion="Microsoft Windows 11"`).
5. **Polls** the image build until it's `AVAILABLE` (or fails) — typically
   20–40 min. Progress prints the Image Builder status transitions.
6. **Tags** the output AMI `spawn:os=windows` so `spawn connect`/`launch` treat
   it as Windows (belt-and-suspenders — the AMI also registers with
   `Platform=windows`).

### Useful flags

| Flag | Default | Purpose |
|------|---------|---------|
| `--iso` | — (required) | Local path or `s3://bucket/key.ISO` |
| `--name` | — (required) | Image Builder image resource name |
| `--bucket` | — | S3 bucket for a local ISO upload (required for a local `--iso`) |
| `--version` | `1.0.0` | Semantic version of the output image |
| `--region` | `us-east-1` | Region for the import build |
| `--image-index` | `1` | Edition index in a multi-edition ISO |
| `--no-secure-boot` | off | Disable Secure Boot on the output AMI |
| `--infra-config-arn` | — | Reuse an existing infrastructure config instead of self-provisioning |
| `--instance-type` | `m6i.large` | Build instance type (self-provisioning only) |
| `--subnet-id` / `--security-group-id` | — | Build-instance networking (self-provisioning only) |

`-o json` emits `{ami, imageBuildVersionArn, uri}` instead of the human summary.

---

## Troubleshooting

Image Builder streams build logs to CloudWatch Logs:

- **LogGroup:** `/aws/imagebuilder/<name>` (the `--name` you passed)
- **LogStream:** `<version>/<build-version>`

A successful import doesn't guarantee a successful *launch* — that still depends
on your normal EC2/VPC networking.

---

## Use it from spawn

```bash
spawn launch winbox --ami <ami-id> --os windows --ttl 4h
```

`--os windows` is explicit (imported AMIs may have unset Platform metadata, and
the `spawn:os=windows` tag covers it either way). spored installs itself at boot
(#77) and the instance self-manages (TTL always terminates — #72).

---

## Files

| File | Purpose |
|------|---------|
| `README.md` | This runbook |
| (command) `spawn image import` | `cmd/image.go` + `pkg/aws/imagebuilder.go` — the import workflow |

> **Legacy:** an earlier qemu/Packer build pipeline (a hand-rolled unattended
> install + `ec2 import-image`) was removed in favor of Image Builder. Its
> history — including the ISO "no-prompt" El-Torito remaster trick needed to
> boot Windows Setup headlessly under qemu — is in git history and in the
> project memory note `project_windows_ami_build.md`, in case an unsupported
> edition (Eval/LTSC) ever needs a non-Image-Builder path.
