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
| **EC2 c8i/m8i/r8i + nested virtualization** (recommended) | Hardware-accelerated KVM on a cheap *virtual* instance (~$0.70/hr, not ~$4/hr metal); ephemeral; scriptable; launch it with spawn itself | A few setup steps (below) |
| **Windows host + Hyper-V** | Native Windows virtualization, reliable installs | Needs Hyper-V enabled (reboot); a personal box is intrusive |
| **macOS/Linux + qemu (local)** | No EC2; runs on the dev box | **Apple Silicon: x86 emulation is many hours/flaky** (see prereqs note); Intel/Linux is fine |

`import-image` and the licensing constraint are identical for all of them — only
the *VM build* step differs.

---

## Recommended: build on an EC2 nested-virtualization instance via spawn

AWS supports nested virtualization (hardware-accelerated KVM inside a *virtual*
instance) on **C8i/M8i/R8i** (Feb 2026, all regions, no extra cost) — so you can
build at native speed without a bare-metal instance. spawn launches it directly:

```bash
# 1. Launch an Ubuntu c8i builder with nested virt + a big scratch volume + TTL.
#    Ubuntu (NOT Amazon Linux 2023 — AL2023's repos ship only qemu-img, no full
#    qemu-system). --nested-virtualization is validated against the instance type.
UBUNTU=$(aws ssm get-parameters --region us-east-1 \
  --names /aws/service/canonical/ubuntu/server/24.04/stable/current/amd64/hvm/ebs-gp3/ami-id \
  --query 'Parameters[0].Value' --output text)
spawn launch winami-builder \
  --instance-type c8i.4xlarge --region us-east-1 \
  --ami "$UBUNTU" --os linux \
  --nested-virtualization \
  --volume-size 200 --ttl 5h --wait-for-ssh=false

# 2. SSH in (spawn injects your key for the local user). Confirm KVM:
#    ls -la /dev/kvm   → present;  grep -c vmx /proc/cpuinfo → >0
#
# 3. The --volume-size 200 lands as a SEPARATE unformatted disk (the Ubuntu
#    root is its own 8 GB device). Format + mount it as the build workspace:
sudo mkfs.ext4 -F /dev/nvme1n1 && sudo mkdir -p /build && sudo mount /dev/nvme1n1 /build
sudo chown "$USER:$USER" /build

# 4. Install the toolchain (Ubuntu makes this easy):
sudo apt-get update
sudo apt-get install -y qemu-system-x86 qemu-utils ovmf xorriso rsync
wget -qO- https://apt.releases.hashicorp.com/gpg | sudo gpg --dearmor -o /usr/share/keyrings/hashicorp-archive-keyring.gpg
echo "deb [signed-by=/usr/share/keyrings/hashicorp-archive-keyring.gpg] https://apt.releases.hashicorp.com $(lsb_release -cs) main" | sudo tee /etc/apt/sources.list.d/hashicorp.list
sudo apt-get update && sudo apt-get install -y packer
sudo usermod -aG kvm "$USER"   # log out/in for the group to take effect
#    Ubuntu OVMF firmware is /usr/share/OVMF/OVMF_CODE_4M.fd + OVMF_VARS_4M.fd.

# 5. Copy templates + the ISO to /build. Use rsync for the multi-GB ISO so it
#    resumes if the connection drops:
rsync --partial --inplace --progress your-windows.iso scttfrdmn@<ip>:/build/windows.iso
scp windows11.pkr.hcl Autounattend.xml provision.ps1 import.sh scttfrdmn@<ip>:/build/

# 6. Build (accel=kvm — fast, native). On Ubuntu point efi_* at the 4M OVMF:
cd /build && packer init windows11.pkr.hcl
packer build \
  -var "iso_path=/build/windows.iso" \
  -var "accel=kvm" \
  -var "efi_code=/usr/share/OVMF/OVMF_CODE_4M.fd" \
  -var "efi_vars=/usr/share/OVMF/OVMF_VARS_4M.fd" \
  windows11.pkr.hcl

# 7. Import + tag from the builder (it has the instance role / aws cli):
./import.sh /build/output-win11/win11.vmdk <s3-bucket> us-east-1

# 8. The builder self-terminates at its TTL; or `spawn terminate winami-builder`.
```

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
