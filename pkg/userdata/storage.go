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
	// EFSMountTargetIP is the mount target's IP, used ONLY as a fallback after
	// the DNS attempts fail (#704). Empty disables the fallback. Never written to
	// /etc/fstab: a mount-target replacement changes the IP, and the DNS name is
	// the durable reference.
	EFSMountTargetIP string
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

// GenerateStorageUserData generates storage mounting script.
//
// EFS is mounted by the mount-target IP FIRST, with the DNS name as the fallback
// (spawn#718). #704 shipped the obvious order and a hardware smoke showed it was
// backwards: twice out of three runs a mount target already `available` in the
// instance's own subnet produced
//
//	mount.nfs4: Failed to resolve server fs-....efs.us-east-1.amazonaws.com
//
// for the whole 60-second retry budget, while the same mount by IP succeeded on
// the first try. None of the usual reasons to prefer the name hold here:
//
//   - Encryption in transit would need it, because stunnel validates the
//     certificate against the DNS name — but spawn cannot ask for it.
//     EFSMountOptions emits a fixed set and ParseCustomOptions accepts no tls or
//     iam key, so there is no code path that mounts with TLS.
//   - fstab surviving a mount-target replacement is close to worthless for an
//     instance that lives minutes to hours, and had a target really been
//     replaced the live NFS mount would already be broken.
//   - DNS resolves to the mount target in the querying instance's AZ for free,
//     but spawn resolves the right target from the instance's own subnet at
//     launch — deterministic rather than hopeful.
//
// So the name costs a minute of boot in the common failure and buys nothing. It
// stays as the fallback for when no IP could be resolved at all, chiefly a caller
// without elasticfilesystem:DescribeMountTargets, and it is what goes in
// /etc/fstab.
//
// The ordering also fixes a verification problem rather than tolerating it. With
// DNS first, the IP path ran only when DNS failed — load-bearing and never
// exercised — and because the failure is intermittent, a passing smoke could not
// prove it either way. Now the primary path runs on every launch and the rarely
// taken path is the well-understood one.
//
// Rationale lives here rather than in the template because template bytes are
// shipped in every instance's 16 KB user-data budget, and cloud-init never reads
// a comment.
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
# Mount by the mount-target IP first, DNS second (#718). Rationale in storage.go.
spawn_mount_efs() {
{{if .EFSMountTargetIP}}
  if mount -t nfs4 -o {{.EFSMountOptions | shellEscape}} {{.EFSMountTargetIP | shellEscape}}:/ {{.EFSMountPoint | shellEscape}}; then
    return 0
  fi
  if mountpoint -q {{.EFSMountPoint | shellEscape}} 2>/dev/null; then
    return 0
  fi
  # A failure here is usually routing or a security group, which DNS will not
  # fix either — but falling through costs nothing and covers a stale IP.
  printf 'spawn: mount by mount-target IP failed; falling back to the DNS name\n' >&2
{{end}}
  for attempt in 1 2 3 4 5 6; do
    if mount -t nfs4 -o {{.EFSMountOptions | shellEscape}} {{.EFSFilesystemDNS | shellEscape}}:/ {{.EFSMountPoint | shellEscape}}; then
      return 0
    fi
    # Already mounted by a previous attempt that reported failure late.
    if mountpoint -q {{.EFSMountPoint | shellEscape}} 2>/dev/null; then
      return 0
    fi
    if [ "$attempt" -lt 6 ]; then
      printf 'spawn: EFS DNS mount attempt %s/6 failed; retrying in 10s\n' "$attempt" >&2
      sleep 10
    fi
  done
  # Not fatal: #668's readiness barrier reads /proc/mounts and fails the workload
  # with a named reason, which is a better signal than aborting the bootstrap
  # half-done.
  printf 'spawn: EFS mount failed via mount-target IP and after 6 DNS attempts over ~60s; the fstab entry remains, so mount -a will retry\n' >&2
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
