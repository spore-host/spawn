package userdata

import (
	"bytes"
	"fmt"
	"text/template" // nosemgrep: go.lang.security.audit.xss.import-text-template.import-text-template

	"github.com/spore-host/spawn/pkg/security"
)

// StorageConfig contains configuration for storage mounting
type StorageConfig struct {
	FSxLustreEnabled bool
	FSxFilesystemDNS string
	FSxMountName     string
	FSxMountPoint    string

	EFSEnabled       bool
	EFSFilesystemDNS string
	EFSMountPoint    string
	EFSMountOptions  string // NFS mount options (e.g., "nfsvers=4.1,rsize=1048576,...")

	// AttachedVolumes are additional EBS data volumes (created from snapshots via
	// the block-device mapping) to mount at boot (#144).
	AttachedVolumes []AttachedVolume
}

// AttachedVolume describes one snapshot-backed EBS data volume to mount.
type AttachedVolume struct {
	DeviceName string // EC2 device name requested in the BDM, e.g. /dev/sdf
	MountPoint string // Absolute mount path
	ReadOnly   bool   // Mount read-only
}

// GenerateStorageUserData generates storage mounting script
func GenerateStorageUserData(config StorageConfig) (string, error) {
	// shellQuote, not security.ShellEscape (#680).
	//
	// ShellEscape is strconv.Quote — Go/C escaping inside DOUBLE quotes — so a
	// mount point containing $(...) EXECUTED at boot, and its quotes collided
	// with the template's own. Three observed renderings for an EFS mount point:
	//
	//   mkdir -p "/efs$(id -u)"                       → substitution runs
	//   echo "export EFS_MOUNT="/efs""                → nested quotes; works only by
	//                                                   accident for simple paths
	//   echo "export EFS_MOUNT="/my efs""             → export EFS_MOUNT=/my, then
	//                                                   `efs` as a command
	//
	// The last one is the common case: a mount point with a space corrupts
	// /etc/profile.d/efs.sh for every login on the instance. security.ShellQuote
	// single-quotes, which suppresses expansion and is self-contained — so the
	// template must NOT add quotes of its own around these values.
	funcMap := template.FuncMap{
		"shellEscape": security.ShellQuote,
	}

	tmpl, err := template.New("storage").Funcs(funcMap).Parse(storageUserDataTemplate)
	if err != nil {
		return "", fmt.Errorf("failed to parse storage template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, config); err != nil {
		return "", fmt.Errorf("failed to execute storage template: %w", err)
	}

	return buf.String(), nil
}

const storageUserDataTemplate = `
{{if .FSxLustreEnabled}}
# FSx Lustre mounting
# Install Lustre 2.15 client from the standard AL2023 repo.
# amazon-linux-extras (AL2 only) is not available on AL2023.
# FSx PERSISTENT_2 runs Lustre server 2.15; the client must match (fixes #316).
dnf install -y lustre-client
modprobe lustre
mkdir -p {{.FSxMountPoint | shellEscape}}
# MountName is the filesystem-specific path component assigned by FSx at
# creation time (e.g. "q5pdvb4v") — NOT "/fsx". Using the wrong name causes
# "client profile could not be read from MGS, rc=-22 EINVAL".
mount -t lustre -o noatime,flock {{.FSxFilesystemDNS | shellEscape}}@tcp:/{{.FSxMountName | shellEscape}} {{.FSxMountPoint | shellEscape}}
# The WHOLE fstab line is single-quoted (#680): fstab wants literal text, so the
# shell must not expand anything in it. Previously these values went raw into a
# double-quoted echo, where a mount point containing $(...) executed at boot.
echo {{printf "%s@tcp:/%s %s lustre noatime,flock,_netdev 0 0" .FSxFilesystemDNS .FSxMountName .FSxMountPoint | shellEscape}} >> /etc/fstab
echo export FSX_MOUNT={{.FSxMountPoint | shellEscape}} >> /etc/profile.d/fsx.sh
{{end}}

{{if .EFSEnabled}}
# EFS mounting
dnf install -y nfs-utils
mkdir -p {{.EFSMountPoint | shellEscape}}
# Retry the mount (#704). EFS DNS can lag mount-target availability: a mount
# target that is already available in this very subnet still produced
#   mount.nfs4: Failed to resolve server fs-….efs.us-east-1.amazonaws.com
# while general DNS worked and the mount target's IP mounted first try. A single
# attempt turned one DNS blip into a launch that boots, bills and runs nothing —
# because #668's readiness barrier correctly refuses to start the workload
# against a directory that is not mounted.
spawn_mount_efs() {
  for attempt in 1 2 3 4 5 6; do
    if mount -t nfs4 -o {{.EFSMountOptions | shellEscape}} {{.EFSFilesystemDNS | shellEscape}}:/ {{.EFSMountPoint | shellEscape}}; then
      return 0
    fi
    # Already mounted by a previous attempt that reported failure late.
    if mountpoint -q {{.EFSMountPoint | shellEscape}} 2>/dev/null; then
      return 0
    fi
    if [ "$attempt" -lt 6 ]; then
      printf 'spawn: EFS mount attempt %s/6 failed; retrying in 10s\n' "$attempt" >&2
      sleep 10
    fi
  done
  # Not fatal here: #668's readiness barrier reads /proc/mounts and fails the
  # workload with a named reason, which is a better signal than aborting the
  # bootstrap half-done. Mounting by the mount-target IP would survive a
  # persistent DNS failure, but needs a DescribeMountTargets call and a new IAM
  # action, so it stays open on #704.
  printf 'spawn: EFS mount failed after 6 attempts over ~60s — DNS for the filesystem may not be resolving; the fstab entry remains, so mount -a will retry\n' >&2
  return 1
}
spawn_mount_efs || true
# Single-quoted as a whole, for the same reason as the FSx line above (#680).
echo {{printf "%s:/ %s nfs4 %s,_netdev 0 0" .EFSFilesystemDNS .EFSMountPoint .EFSMountOptions | shellEscape}} >> /etc/fstab
echo export EFS_MOUNT={{.EFSMountPoint | shellEscape}} >> /etc/profile.d/efs.sh
{{end}}
{{if .AttachedVolumes}}
# Attached EBS data volumes (created from snapshots; #144).
# On Nitro the requested device name (e.g. /dev/sdf) is remapped to an NVMe
# device. AL2023's ec2-utils udev rules create a /dev/sdf symlink to the real
# device; spawn_resolve_dev waits for it and falls back to ebsnvme-id, which
# reports the original mapping each NVMe device was attached as.
spawn_resolve_dev() {
  req="$1"; base="$(basename "$req")"
  for _ in $(seq 1 60); do
    if [ -e "$req" ]; then readlink -f "$req"; return 0; fi
    if [ -e "/dev/${base}" ]; then readlink -f "/dev/${base}"; return 0; fi
    if command -v ebsnvme-id >/dev/null 2>&1; then
      for n in /dev/nvme*n1; do
        [ -e "$n" ] || continue
        if ebsnvme-id "$n" 2>/dev/null | grep -qE "(/dev/)?(sd|xvd)?${base#sd}\b"; then
          echo "$n"; return 0
        fi
      done
    fi
    sleep 2
  done
  return 1
}
{{range .AttachedVolumes}}
mkdir -p {{.MountPoint | shellEscape}}
SPAWN_DEV="$(spawn_resolve_dev {{.DeviceName | shellEscape}})"
if [ -n "$SPAWN_DEV" ]; then
  # Snapshot-backed volumes already carry a filesystem — never reformat.
  mount -o {{if .ReadOnly}}ro,{{end}}noatime "$SPAWN_DEV" {{.MountPoint | shellEscape}}
  # printf with the mount point single-quoted (#680). The whole line cannot be
  # single-quoted like the EFS/FSx ones because $SPAWN_DEV must still expand, so
  # the device comes through a double-quoted argument and the mount point through
  # a quoted literal. Previously {{.MountPoint}} went in raw, where $(...) in a
  # mount point executed at boot.
  printf '%s %s auto {{if .ReadOnly}}ro,{{end}}noatime,nofail 0 2\n' "$SPAWN_DEV" {{.MountPoint | shellEscape}} >> /etc/fstab
else
  printf 'spawn: timed out resolving attached volume device %s for %s\n' \
    {{.DeviceName | shellEscape}} {{.MountPoint | shellEscape}} >&2
fi
{{end}}
{{end}}
`
