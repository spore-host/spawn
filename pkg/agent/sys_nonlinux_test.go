//go:build !linux

package agent

import (
	"context"
	"errors"
	"testing"
)

// Off Linux an FSx mount must fail, never report success: spored does not mount
// Lustre there (#194), and a nil would make maybeMountPendingFSx log
// "fsx: mounted" for a filesystem that is not there.
func TestSysMountLustre_FailsOffLinux(t *testing.T) {
	err := sysMountLustre(context.Background(), "fs.example", "mnt", "/fsx")
	if !errors.Is(err, errLustreUnsupported) {
		t.Fatalf("sysMountLustre off Linux = %v, want errLustreUnsupported", err)
	}
}
