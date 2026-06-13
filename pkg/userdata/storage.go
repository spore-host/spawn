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
	// Register custom template function for shell escaping
	funcMap := template.FuncMap{
		"shellEscape": security.ShellEscape,
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
echo "{{.FSxFilesystemDNS}}@tcp:/{{.FSxMountName}} {{.FSxMountPoint}} lustre noatime,flock,_netdev 0 0" >> /etc/fstab
echo "export FSX_MOUNT={{.FSxMountPoint | shellEscape}}" >> /etc/profile.d/fsx.sh
{{end}}

{{if .EFSEnabled}}
# EFS mounting
dnf install -y nfs-utils
mkdir -p {{.EFSMountPoint | shellEscape}}
mount -t nfs4 -o {{.EFSMountOptions | shellEscape}} {{.EFSFilesystemDNS | shellEscape}}:/ {{.EFSMountPoint | shellEscape}}
echo "{{.EFSFilesystemDNS}}:/ {{.EFSMountPoint}} nfs4 {{.EFSMountOptions}},_netdev 0 0" >> /etc/fstab
echo "export EFS_MOUNT={{.EFSMountPoint | shellEscape}}" >> /etc/profile.d/efs.sh
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
  echo "$SPAWN_DEV {{.MountPoint}} auto {{if .ReadOnly}}ro,{{end}}noatime,nofail 0 2" >> /etc/fstab
else
  echo "spawn: timed out resolving attached volume device {{.DeviceName}} for {{.MountPoint}}" >&2
fi
{{end}}
{{end}}
`
