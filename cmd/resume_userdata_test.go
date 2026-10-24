package cmd

import (
	"os"
	"path/filepath"
	"testing"

	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/platform"
)

// TestResumedSweepUserDataFitsEC2 is the resume half of #1274's fold-in:
// `spawn resume` base64-encoded the plain bootstrap with no gzip, unlike every
// other launch path, and the inline bootstrap is far over EC2's user-data
// limits that way, so RunInstances refused every resumed Linux member. Measured
// with the inline bootstrap (what a dev build, and an opted-out caller, sends),
// which is the largest the script gets.
func TestResumedSweepUserDataFitsEC2(t *testing.T) {
	dir := t.TempDir()
	pub := filepath.Join(dir, "id.pub")
	if err := os.WriteFile(pub, []byte("ssh-ed25519 AAAAtest spawn\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plat := &platform.Platform{OS: "linux", HomeDir: dir, SSHPubKeyPath: pub}

	encoded, err := resumeUserData(plat, &spawnaws.LaunchConfig{
		SweepID:         "sweep-1",
		JobArrayCommand: "echo resumed",
	})
	if err != nil {
		t.Fatalf("resumeUserData: %v", err)
	}
	decoded, encodedLen := spawnaws.UserDataSize(encoded)
	if err := spawnaws.CheckUserDataSize(encoded); err != nil {
		t.Fatalf("a resumed sweep member's user data would be refused by RunInstances: %v", err)
	}
	if _, err := decodeUserData(encoded); err != nil {
		t.Fatalf("resumed user data is not in the encoding cloud-init is sent everywhere else: %v", err)
	}
	t.Logf("resumed member user data: %d bytes decoded, %d base64", decoded, encodedLen)
}
