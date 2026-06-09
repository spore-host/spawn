# Building a Windows custom AMI from an ISO

Turn a Windows ISO (e.g. Windows 11 25H2 Enterprise Eval) into an EC2 AMI that
spore.host tools can launch with `spawn launch --ami <id> --os windows`.

> **This is a self-service process you run in _your own_ AWS account.** You
> supply the ISO (and its license), build the image, and `ec2 import-image`
> creates the AMI in your account; spawn then launches from it. Nothing here is
> hosted or run for you — spore.host maintains these templates, but the build,
> the resulting AMI, and the licensing are yours. (For the spore.host team, "your
> account" is the **dev** account.)

EC2 cannot boot an ISO directly. The pipeline is:

```
ISO ─(Packer + Hyper-V/qemu, unattended install)→ VHD
    ─(sysprep + EC2 guest components)→ generalized VHD
    ─(upload to S3 + aws ec2 import-image)→ AMI
    ─(tag spawn:os=windows)→ usable by `spawn launch --os windows`
```

This directory contains the Packer template, unattended-install answer file,
guest provisioning, and the import helper. **A full build is multi-hour and
partly manual** — read all of "Constraints" first.

---

## ⚠️ Constraints — read before starting

1. **Windows *client* (Windows 10/11) licensing on EC2 requires
   [Dedicated Hosts / BYOL](https://docs.aws.amazon.com/AWSEC2/latest/WindowsGuide/dedicated-hosts-bring-your-own-windows-desktop-licenses.html).**
   You cannot run an imported Windows 11 *client* AMI on shared tenancy. Windows
   *Server* AMIs (the stock ones spawn uses by default) have no such restriction
   — only this custom *client* image does. The Enterprise **Eval** edition is
   also time-limited (~90 days); the resulting AMI is for our own feasibility use
   and must not be distributed.

2. **`ec2 import-image` needs a `vmimport` IAM service role + an S3 bucket** in
   the account that will own the AMI. Deploy
   [`../../deployment/cloudformation/vmimport-role.yaml`](../../../deployment/cloudformation/vmimport-role.yaml)
   first (it creates the role with the AWS-required trust + S3/EC2 policy).

3. **The guest must have the AWS components baked in before sysprep** or the
   imported instance won't boot/network/be manageable: EC2Launch v2, the AWS
   NVMe/ENA/PV drivers, and the SSM agent. `provision.ps1` installs these.

4. **Build host:** needs a Windows-capable hypervisor. Hyper-V (Windows host) is
   the primary, most reliable path for Windows guests; qemu (macOS/Linux) is the
   documented fallback. See "Where to build" below.

---

## Where to build

| Option | Pros | Cons |
|--------|------|------|
| **Windows host + Hyper-V** (primary) | Native Windows virtualization, most reliable unattended installs, native DISM/sysprep/ADK | Needs Hyper-V enabled (reboot); a personal box is intrusive for multi-hour builds |
| **macOS/Linux + qemu** (fallback) | No reboot, runs on the dev Mac | Windows-on-qemu is finickier; slower |
| **EC2 `*.metal` Windows builder** (best for repeatability) | Ephemeral, scriptable, no personal machine, fast | Costs a metal instance for the build window; more setup |

`import-image` and the licensing constraint are identical for all three — only
the *VM build* step differs.

---

## Prerequisites on the build host

**Hyper-V (Windows host):**
```powershell
# Enable Hyper-V (REBOOTS the machine):
Enable-WindowsOptionalFeature -Online -FeatureName Microsoft-Hyper-V-All -All
# After reboot, install Packer + the Windows ADK (provides oscdimg, used to
# build the secondary "answer" ISO that carries Autounattend.xml):
winget install HashiCorp.Packer
winget install Microsoft.WindowsADK
```

**macOS + qemu** (the local self-service path — most users have the ISO on
their own machine):
```bash
brew install packer qemu xorriso awscli
# packer  — runs the build
# qemu    — the hypervisor (qemu-system-x86_64) + qemu-img (disk convert)
# xorriso — builds the secondary "answer" ISO carrying Autounattend.xml
#           (macOS has no oscdimg; xorriso is the cross-platform equivalent)
# awscli  — import.sh uploads the disk + runs ec2 import-image
```
The qemu x86_64 UEFI firmware (`edk2-x86_64-code.fd`) ships with the qemu
formula under `$(brew --prefix)/share/qemu/`.

> **⚠️ Apple Silicon (arm64) note:** the Windows ISO is **x86_64**. On an
> Apple-Silicon Mac, qemu must *fully emulate* x86_64 (`-accel tcg`, no `hvf`
> cross-arch acceleration), so the unattended install runs **many hours** and
> can be flaky. It works, but for anything beyond a one-off, build on an x86_64
> host, a Windows host (Hyper-V), or an EC2 `*.metal` builder where the install
> takes ~30-60 min. On an Intel Mac, qemu uses `-accel hvf` and is fast.

---

## Build steps

1. **Build the VM image** (unattended install → provision → sysprep → export).
   The qemu builder attaches `Autounattend.xml` automatically via `cd_files` (a
   second CD) — no manual answer-ISO step. On **Apple Silicon** keep `accel=tcg`
   (default, pure emulation); on **Intel macOS** pass `-var accel=hvf`:
   ```bash
   packer init windows11.pkr.hcl
   packer validate windows11.pkr.hcl
   packer build \
     -var "iso_path=/Volumes/External HD/<your-windows>.iso" \
     -var "efi_code=$(brew --prefix)/share/qemu/edk2-x86_64-code.fd" \
     -var "efi_vars=$(brew --prefix)/share/qemu/edk2-i386-vars.fd" \
     windows11.pkr.hcl
   ```
   (Watch the install: `packer build` prints a VNC address — connect with any
   VNC client to see the Windows Setup screen. The first boot sits at "Waiting
   for WinRM" for the whole install; on Apple Silicon that's hours.)
   Output: `output-win11/win11.vmdk` (qemu emits qcow2; a post-processor
   converts it to stream-optimized VMDK for import). This runs the full Windows
   install + `provision.ps1` (EC2 components) + sysprep generalize/shutdown.
   **~30-60 min on Intel/Hyper-V; many hours emulated on Apple Silicon.**

3. **Import to an AMI** (uploads the VHD to S3, runs `import-image`, tags the
   result). Requires the `vmimport` role (constraint #2):
   ```bash
   ./import.sh <path-to-exported.vhd> <s3-bucket> <region>
   ```
   It prints the new AMI id when the import task completes (can take 20–40 min).

4. **Use it from spawn** — the AMI's Platform metadata may be unset for an
   imported image, so pass `--os windows` explicitly:
   ```bash
   spawn launch winbox --ami <ami-id> --os windows --ttl 4h
   ```
   spored installs itself at boot (#77) and the instance self-manages. For a
   *client* AMI, add Dedicated-Host tenancy per constraint #1.

---

## Files

| File | Purpose |
|------|---------|
| `windows11.pkr.hcl` | Packer template — `hyperv-iso` builder (primary) + commented `qemu` fallback |
| `Autounattend.xml` | Unattended Windows install answer file (edition, partition, admin user, WinRM, Win11 TPM/SecureBoot bypass) |
| `provision.ps1` | Guest provisioning over WinRM: EC2Launch v2 + AWS drivers + SSM agent + OpenSSH, then sysprep |
| `import.sh` | Upload VHD to S3 + `ec2 import-image` + poll + tag the AMI |
| `../../deployment/cloudformation/vmimport-role.yaml` | The `vmimport` service role required by `import-image` |
