// Packer template: build a Windows 11 disk image from an ISO, ready for
// `aws ec2 import-image`. Primary builder is hyperv-iso (run on a Windows host
// with Hyper-V enabled); a qemu fallback is provided commented-out at the bottom.
//
// Flow: boot the ISO with a secondary "answer" ISO carrying Autounattend.xml →
// unattended install → WinRM comes up → provision.ps1 (EC2 components + sysprep)
// → export VHD. See README.md.

packer {
  required_plugins {
    hyperv = {
      source  = "github.com/hashicorp/hyperv"
      version = ">= 1.1.0"
    }
    // Uncomment for the qemu fallback path:
    // qemu = {
    //   source  = "github.com/hashicorp/qemu"
    //   version = ">= 1.1.0"
    // }
  }
}

variable "iso_path" {
  type        = string
  description = "Path/URL to the Windows ISO."
  default     = "" // e.g. C:/isos/Win11_25H2_EnterpriseEval.iso
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

source "hyperv-iso" "win11" {
  iso_url      = var.iso_path
  iso_checksum = var.iso_checksum

  // Secondary ISO carries Autounattend.xml at its root, where Windows Setup
  // auto-discovers it. Built from this dir's Autounattend.xml by the README's
  // oscdimg step (or set secondary_iso_images to a prebuilt answer ISO).
  secondary_iso_images = ["./answer.iso"]

  communicator   = "winrm"
  winrm_username = "Administrator"
  winrm_password = var.admin_password
  winrm_timeout  = "4h" // unattended install + first boot can be slow

  cpus       = 4
  memory     = 4096
  disk_size  = var.disk_size_mb
  generation = 2 // UEFI; Win11 requires gen2

  // Win11 requires vTPM + Secure Boot to install normally; we enable vTPM and
  // use the MS UEFI cert. (Autounattend also bypasses the checks as a fallback.)
  enable_secure_boot   = true
  secure_boot_template = "MicrosoftUEFICertificateAuthority"
  enable_virtualization_extensions = false

  shutdown_command = "shutdown /s /t 10 /f /d p:4:1 /c \"Packer Shutdown\""
  output_directory = "output-win11"
}

build {
  sources = ["source.hyperv-iso.win11"]

  // Provision the AWS guest components and sysprep. After this runs, the VM
  // shuts down generalized and ready for import-image.
  provisioner "powershell" {
    script = "./provision.ps1"
  }
}

// ---------------------------------------------------------------------------
// qemu fallback (macOS/Linux). Uncomment the plugin above and this source, and
// swap the build `sources` to "source.qemu.win11". Windows-on-qemu needs the
// virtio drivers slipstreamed via the Autounattend / a virtio ISO; see README.
// ---------------------------------------------------------------------------
// source "qemu" "win11" {
//   iso_url          = var.iso_path
//   iso_checksum     = var.iso_checksum
//   communicator     = "winrm"
//   winrm_username   = "Administrator"
//   winrm_password   = var.admin_password
//   winrm_timeout    = "4h"
//   cpus             = 4
//   memory           = 4096
//   disk_size        = "${var.disk_size_mb}M"
//   format           = "vmdk"
//   accelerator      = "hvf" // macOS hypervisor framework
//   firmware         = "/opt/homebrew/share/qemu/edk2-x86_64-code.fd"
//   cd_files         = ["./Autounattend.xml"]
//   shutdown_command = "shutdown /s /t 10 /f"
//   output_directory = "output-win11"
// }
