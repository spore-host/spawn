package userdata

import (
	"strings"
	"testing"
)

// TestGenerateStorageUserData_FSxBasic verifies the FSx mount script uses
// the correct AL2023 install command and includes modprobe (regression for #316).
func TestGenerateStorageUserData_FSxBasic(t *testing.T) {
	config := StorageConfig{
		FSxLustreEnabled: true,
		FSxFilesystemDNS: "fs-0abc123.fsx.us-east-1.amazonaws.com",
		FSxMountName:     "q5pdvb4v",
		FSxMountPoint:    "/fsx",
	}

	script, err := GenerateStorageUserData(config)
	if err != nil {
		t.Fatalf("GenerateStorageUserData() error = %v", err)
	}

	// Must not CALL amazon-linux-extras (AL2-only) — regression for #316.
	// A comment mentioning it is fine; an actual invocation is not.
	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "amazon-linux-extras") {
			t.Errorf("script must not invoke amazon-linux-extras (AL2 only); found: %q", trimmed)
		}
	}
	if !strings.Contains(script, "dnf install -y lustre-client") {
		t.Error("script must install lustre-client via dnf for AL2023 compatibility")
	}

	// Must load the kernel module before mounting — regression for #316
	if !strings.Contains(script, "modprobe lustre") {
		t.Error("script must call modprobe lustre before mounting")
	}

	// Mount command must reference the filesystem-specific MountName, not a generic path
	if !strings.Contains(script, "q5pdvb4v") {
		t.Error("mount command must include FSx MountName (q5pdvb4v), not a generic path")
	}
	if !strings.Contains(script, "fs-0abc123.fsx.us-east-1.amazonaws.com@tcp:/q5pdvb4v") {
		t.Errorf("expected full mount path <dns>@tcp:/<mountname>, got:\n%s", script)
	}

	// Mount point must be correct
	if !strings.Contains(script, "/fsx") {
		t.Error("script must reference mount point /fsx")
	}

	// fstab entry must be present
	if !strings.Contains(script, "/etc/fstab") {
		t.Error("script must write fstab entry for persistent mount")
	}

	// fstab must NOT use 'defaults' keyword (redundant with explicit options)
	if strings.Contains(script, "defaults,noatime") || strings.Contains(script, "defaults,flock") {
		t.Error("fstab entry must not use 'defaults' alongside explicit Lustre options")
	}

	// Environment variable export
	if !strings.Contains(script, "FSX_MOUNT") {
		t.Error("script must export FSX_MOUNT environment variable")
	}
}

// TestGenerateStorageUserData_FSxMountOptions verifies mount uses noatime,flock.
func TestGenerateStorageUserData_FSxMountOptions(t *testing.T) {
	config := StorageConfig{
		FSxLustreEnabled: true,
		FSxFilesystemDNS: "fs-0abc123.fsx.us-east-2.amazonaws.com",
		FSxMountName:     "abcdef12",
		FSxMountPoint:    "/mnt/scratch",
	}

	script, err := GenerateStorageUserData(config)
	if err != nil {
		t.Fatalf("GenerateStorageUserData() error = %v", err)
	}

	// Must include recommended Lustre mount options
	if !strings.Contains(script, "noatime") {
		t.Error("mount command should include noatime option")
	}
	if !strings.Contains(script, "flock") {
		t.Error("mount command should include flock option")
	}
}

// TestGenerateStorageUserData_FSxMountName_NotGeneric verifies the template
// uses the per-filesystem MountName, not a hardcoded "/fsx" path component.
// The "client profile could not be read" error (rc=-22 EINVAL, issue #316)
// was caused by using the wrong path in the mount command.
func TestGenerateStorageUserData_FSxMountName_NotGeneric(t *testing.T) {
	mountName := "uniquemnt"
	config := StorageConfig{
		FSxLustreEnabled: true,
		FSxFilesystemDNS: "fs-xyz.fsx.us-east-1.amazonaws.com",
		FSxMountName:     mountName,
		FSxMountPoint:    "/fsx",
	}

	script, err := GenerateStorageUserData(config)
	if err != nil {
		t.Fatalf("GenerateStorageUserData() error = %v", err)
	}

	// The mount command must use the actual MountName from the FSx API, not
	// a hardcoded "/fsx" path component. Using the wrong name causes:
	// "LustreError: The client profile '<name>-client' could not be read from MGS"
	if !strings.Contains(script, "@tcp:/"+mountName) {
		t.Errorf("mount command must use per-filesystem MountName %q in @tcp:/<name>, got:\n%s", mountName, script)
	}
	// Verify it's not just using the mount point as the Lustre path
	if strings.Contains(script, "@tcp://fsx") {
		t.Error("mount command must not use mount point path as Lustre path; use FSx MountName")
	}
}

// TestGenerateStorageUserData_EFSBasic verifies EFS mount script uses dnf.
func TestGenerateStorageUserData_EFSBasic(t *testing.T) {
	config := StorageConfig{
		EFSEnabled:       true,
		EFSFilesystemDNS: "fs-0def456.efs.us-east-1.amazonaws.com",
		EFSMountPoint:    "/efs",
		EFSMountOptions:  "nfsvers=4.1,rsize=1048576,wsize=1048576,hard,timeo=600,retrans=2",
	}

	script, err := GenerateStorageUserData(config)
	if err != nil {
		t.Fatalf("GenerateStorageUserData() error = %v", err)
	}

	// Must use dnf, not yum (AL2 only)
	if strings.Contains(script, "yum install") {
		t.Error("script must not use yum; use dnf for AL2023 compatibility")
	}
	if !strings.Contains(script, "dnf install -y nfs-utils") {
		t.Error("script must install nfs-utils via dnf")
	}

	// Mount command
	if !strings.Contains(script, "mount -t nfs4") {
		t.Error("script must mount EFS using nfs4 type")
	}
	if !strings.Contains(script, "fs-0def456.efs.us-east-1.amazonaws.com") {
		t.Error("script must include EFS DNS name in mount command")
	}
	if !strings.Contains(script, "/efs") {
		t.Error("script must include EFS mount point")
	}

	// fstab
	if !strings.Contains(script, "/etc/fstab") {
		t.Error("script must write fstab entry for EFS")
	}

	// Environment variable
	if !strings.Contains(script, "EFS_MOUNT") {
		t.Error("script must export EFS_MOUNT environment variable")
	}
}

// TestGenerateStorageUserData_BothFSxAndEFS verifies both can be mounted.
func TestGenerateStorageUserData_BothFSxAndEFS(t *testing.T) {
	config := StorageConfig{
		FSxLustreEnabled: true,
		FSxFilesystemDNS: "fs-fsx.fsx.us-east-1.amazonaws.com",
		FSxMountName:     "lustre01",
		FSxMountPoint:    "/fsx",
		EFSEnabled:       true,
		EFSFilesystemDNS: "fs-efs.efs.us-east-1.amazonaws.com",
		EFSMountPoint:    "/efs",
		EFSMountOptions:  "nfsvers=4.1,hard",
	}

	script, err := GenerateStorageUserData(config)
	if err != nil {
		t.Fatalf("GenerateStorageUserData() error = %v", err)
	}

	if !strings.Contains(script, "lustre-client") {
		t.Error("combined script must install Lustre client")
	}
	if !strings.Contains(script, "nfs-utils") {
		t.Error("combined script must install nfs-utils")
	}
	if !strings.Contains(script, "lustre01") {
		t.Error("combined script must reference FSx MountName")
	}
	if !strings.Contains(script, "fs-efs.efs.us-east-1.amazonaws.com") {
		t.Error("combined script must reference EFS DNS")
	}
}

// TestGenerateStorageUserData_NeitherEnabled verifies empty config produces no mount commands.
func TestGenerateStorageUserData_NeitherEnabled(t *testing.T) {
	config := StorageConfig{}

	script, err := GenerateStorageUserData(config)
	if err != nil {
		t.Fatalf("GenerateStorageUserData() error = %v", err)
	}

	// Script should be essentially empty (just whitespace from template)
	trimmed := strings.TrimSpace(script)
	if strings.Contains(trimmed, "mount") || strings.Contains(trimmed, "dnf") {
		t.Errorf("empty config should produce no mount commands, got:\n%s", script)
	}
}

// TestGenerateStorageUserData_AttachedVolumes verifies snapshot-backed EBS data
// volumes are mounted read-only without mkfs, and the device is resolved (#144).
func TestGenerateStorageUserData_AttachedVolumes(t *testing.T) {
	config := StorageConfig{
		AttachedVolumes: []AttachedVolume{
			{DeviceName: "/dev/sdf", MountPoint: "/opt/databases/kraken2", ReadOnly: true},
		},
	}

	script, err := GenerateStorageUserData(config)
	if err != nil {
		t.Fatalf("GenerateStorageUserData() error = %v", err)
	}

	if !strings.Contains(script, `spawn_resolve_dev "/dev/sdf"`) {
		t.Error("script must resolve the live (NVMe) device for the requested device name")
	}
	if !strings.Contains(script, `mkdir -p "/opt/databases/kraken2"`) {
		t.Error("script must create the mount point")
	}
	if !strings.Contains(script, "mount -o ro,noatime") {
		t.Errorf("read-only volume must mount with ro,noatime; got:\n%s", script)
	}
	// Snapshot-backed volume already has a filesystem — must never reformat.
	if strings.Contains(script, "mkfs") {
		t.Error("snapshot-backed volume must not be mkfs'd")
	}
	if !strings.Contains(script, "nofail") {
		t.Error("fstab entry should use nofail so a missing data volume doesn't wedge boot")
	}
}

func TestGenerateStorageUserData_AttachedVolumesReadWrite(t *testing.T) {
	config := StorageConfig{
		AttachedVolumes: []AttachedVolume{
			{DeviceName: "/dev/sdf", MountPoint: "/data", ReadOnly: false},
		},
	}
	script, err := GenerateStorageUserData(config)
	if err != nil {
		t.Fatalf("GenerateStorageUserData() error = %v", err)
	}
	if strings.Contains(script, "mount -o ro,") {
		t.Error("read-write volume must not mount read-only")
	}
	if !strings.Contains(script, "mount -o noatime /data") && !strings.Contains(script, "noatime\" /data") {
		// device is resolved into $SPAWN_DEV, so just assert the rw mount has no ro
		if !strings.Contains(script, "mount -o noatime") {
			t.Errorf("read-write volume should mount with noatime (no ro); got:\n%s", script)
		}
	}
}

// TestGenerateStorageUserData_MountPointInjection verifies mount point is shell-escaped.
func TestGenerateStorageUserData_MountPointInjection(t *testing.T) {
	config := StorageConfig{
		FSxLustreEnabled: true,
		FSxFilesystemDNS: "fs-0abc.fsx.us-east-1.amazonaws.com",
		FSxMountName:     "safe01",
		// Mount point with spaces — should be escaped
		FSxMountPoint: "/mnt/fsx data",
	}

	script, err := GenerateStorageUserData(config)
	if err != nil {
		t.Fatalf("GenerateStorageUserData() error = %v", err)
	}

	// Shell escape should have been applied — raw unquoted space would be dangerous
	if strings.Contains(script, "mkdir -p /mnt/fsx data") {
		t.Error("mount point with space must be shell-escaped")
	}
}
