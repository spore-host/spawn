// Packer template: build a Windows 11 disk image from an ISO, ready for
// `aws ec2 import-image`. Active builder is qemu (macOS/Linux self-service path —
// most users have the ISO on their own machine). A hyperv-iso block for Windows
// hosts is provided commented-out at the bottom.
//
// Flow: boot the ISO with a secondary "answer" ISO carrying Autounattend.xml →
// unattended install → WinRM comes up → provision.ps1 (EC2 components + sysprep)
// → export a disk image (VMDK) for import-image. See README.md.
//
// NOTE (Apple Silicon): the ISO is x86_64; qemu must emulate (accel=tcg), so the
// install takes hours. On Intel macOS / x86_64 Linux it uses hvf/kvm and is fast.

packer {
  required_plugins {
    qemu = {
      source  = "github.com/hashicorp/qemu"
      version = ">= 1.1.0"
    }
  }
}

variable "iso_path" {
  type        = string
  description = "Path/URL to the Windows ISO."
  default     = "" // e.g. /Volumes/External HD/Win11_25H2_EnterpriseEval.iso
}

variable "iso_checksum" {
  type        = string
  description = "ISO checksum, e.g. sha256:<hex> (or 'none' to skip — not recommended)."
  default     = "none"
}

variable "admin_password" {
  type        = string
  description = "Local Administrator password set by Autounattend; Packer uses it for WinRM."
  default     = "SporeBuild!2026"
  sensitive   = true
}

variable "disk_size_mb" {
  type    = number
  default = 40960 // 40 GiB; import resizes/uses as the root volume
}

// Answer media: the qemu builder uses cd_files (below) to build the
// Autounattend CD automatically — no prebuilt ISO needed. This var is only for
// the Hyper-V alternative (commented at the bottom), which takes a prebuilt ISO
// via secondary_iso_images (build it with oscdimg/xorriso; see README).
variable "answer_iso" {
  type    = string
  default = "./answer.iso"
}

// QEMU accelerator: "hvf" on Intel macOS, "kvm" on Linux, "tcg" (pure emulation)
// on Apple Silicon since the guest is x86_64. Override with -var.
variable "accel" {
  type    = string
  default = "tcg"
}

// x86_64 UEFI firmware qemu ships (brew: $(brew --prefix)/share/qemu).
// Win11 requires UEFI — these are attached as pflash via the plugin's efi_*
// options (NOT -bios, which can't load the edk2 code blob).
variable "efi_code" {
  type    = string
  default = "/opt/homebrew/share/qemu/edk2-x86_64-code.fd"
}

variable "efi_vars" {
  type    = string
  default = "/opt/homebrew/share/qemu/edk2-i386-vars.fd"
}

source "qemu" "win11" {
  iso_url      = var.iso_path
  iso_checksum = var.iso_checksum

  // Carry Autounattend.xml on a CD that Packer attaches as the SECOND CD device
  // (the install ISO is the first). Using cd_files — NOT qemuargs — is critical:
  // qemuargs is a full override and would wipe Packer's default disk + install
  // ISO + EFI pflash drives. Windows Setup auto-discovers Autounattend.xml on
  // removable media. headless: no GUI; connect to the VNC port Packer prints to
  // watch the install.
  headless = true
  // Answer CD carries the unattended-install answer file, attached as the SECOND
  // CD device (the install ISO is first). Using cd_files — NOT qemuargs — is
  // critical: qemuargs is a full override and would wipe Packer's default disk +
  // install ISO + EFI pflash drives. Windows Setup auto-discovers Autounattend.xml
  // on removable media. headless: no GUI; connect to the VNC port Packer prints.
  cd_files = ["./Autounattend.xml"]
  cd_label = "UNATTEND"

  // No boot_command, by design. The stock Windows ISO boots via a "Press any key
  // to boot from CD or DVD..." shim (its UEFI El Torito image, efisys.bin) that
  // waits ~5s — and in a headless qemu build there is NO reliable way to land a
  // keypress in that window (OVMF shows the prompt at a late, variable delay, then
  // on timeout falls through to PXE and the UEFI shell; we burned a lot of build
  // runs proving this via VNC screenshots). Rather than fight the timing, the ISO
  // is pre-processed by remaster-noprompt.sh, which swaps the El Torito UEFI boot
  // image for Microsoft's no-prompt variant (efisys_noprompt.bin, shipped on the
  // same media) — so the DVD boots Windows Setup directly, deterministically, with
  // no keypress at all. ALWAYS point iso_path at the remastered *-noprompt.iso.
  // See README "Pre-process the ISO" + remaster-noprompt.sh for the full story.
  boot_wait         = "2s"
  boot_command      = []

  // UEFI boot via pflash (Win11 requirement). The plugin copies efi_firmware_vars
  // to a writable per-build file. q35 is required for UEFI/Secure Boot.
  efi_boot          = true
  efi_firmware_code = var.efi_code
  efi_firmware_vars = var.efi_vars
  machine_type      = "q35"

  communicator   = "winrm"
  winrm_username = "Administrator"
  winrm_password = var.admin_password
  winrm_timeout  = "8h" // emulated install on Apple Silicon is slow

  accelerator    = var.accel
  cpus           = 4
  memory         = 4096
  disk_size      = var.disk_size_mb
  format         = "qcow2" // qemu builder only emits qcow2/raw; converted to VMDK below
  net_device     = "e1000"
  disk_interface = "ide" // Windows Setup has no virtio driver by default

  shutdown_command = "shutdown /s /t 10 /f"
  output_directory = "output-win11"
}

build {
  sources = ["source.qemu.win11"]

  // Provision the AWS guest components and sysprep. After this the VM shuts
  // down generalized, ready for import-image.
  provisioner "powershell" {
    script = "./provision.ps1"
  }

  // import-image doesn't take qcow2; convert the qemu output to VMDK (stream
  // optimized) so import.sh can upload it directly.
  post-processor "shell-local" {
    inline = [
      "qemu-img convert -f qcow2 -O vmdk -o subformat=streamOptimized output-win11/packer-win11 output-win11/win11.vmdk",
      "echo 'Converted → output-win11/win11.vmdk (run ./import.sh output-win11/win11.vmdk <bucket> <region>)'",
    ]
  }
}

// ---------------------------------------------------------------------------
// Hyper-V alternative (Windows build host). Swap the required_plugins to
// `hyperv`, uncomment this source, and set build sources to
// "source.hyperv-iso.win11". Hyper-V is the most reliable Windows builder
// (native, fast) — preferred when a Windows host is available. See README.
// ---------------------------------------------------------------------------
// source "hyperv-iso" "win11" {
//   iso_url              = var.iso_path
//   iso_checksum         = var.iso_checksum
//   secondary_iso_images = [var.answer_iso]
//   communicator         = "winrm"
//   winrm_username       = "Administrator"
//   winrm_password       = var.admin_password
//   winrm_timeout        = "4h"
//   cpus                 = 4
//   memory               = 4096
//   disk_size            = var.disk_size_mb
//   generation           = 2
//   enable_secure_boot   = true
//   secure_boot_template = "MicrosoftUEFICertificateAuthority"
//   shutdown_command     = "shutdown /s /t 10 /f"
//   output_directory     = "output-win11"
// }
