package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/platform"
)

// TestBuildUserDataHonoursCompletionFile is the CLI half of the #1131 gate: the
// launch's --completion-file (LaunchConfig.CompletionFile, which spored watches)
// must reach the --command wrapper's completion write. pkg/launcher's tests
// prove the builder renders whatever it is given; this proves the CLI gives it.
func TestBuildUserDataHonoursCompletionFile(t *testing.T) {
	dir := t.TempDir()
	pub := filepath.Join(dir, "id.pub")
	if err := os.WriteFile(pub, []byte("ssh-ed25519 AAAAtest spawn\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plat := &platform.Platform{OS: "linux", HomeDir: dir, SSHPubKeyPath: pub}

	s, err := buildUserData(plat, &spawnaws.LaunchConfig{
		JobArrayCommand: "true",
		OnComplete:      "terminate",
		CompletionFile:  "/data/done",
	}, "", nil)
	if err != nil {
		t.Fatalf("buildUserData: %v", err)
	}
	// The wrapper writes to a variable the script assigns at the top, so the
	// static body can be published once per release (#1274).
	if !strings.Contains(s, `cat > "$SPAWN_CMD_COMPLETION_FILE" <<JSONEOF`) {
		t.Error("the --command wrapper does not write the completion-file variable")
	}
	if !strings.Contains(s, "\nSPAWN_CMD_COMPLETION_FILE='/data/done'\n") {
		t.Error("the --command wrapper's completion file is not the configured --completion-file /data/done; spored would never see completion (#1131)")
	}
	if strings.Contains(s, "SPAWN_CMD_COMPLETION_FILE='/tmp/SPAWN_COMPLETE'") || strings.Contains(s, "cat > /tmp/SPAWN_COMPLETE <<JSONEOF") {
		t.Error("the --command wrapper still writes the default /tmp/SPAWN_COMPLETE despite --completion-file /data/done")
	}
}
