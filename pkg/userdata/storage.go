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
`
