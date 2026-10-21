package launcher

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The bootstrap's authorized_keys step must not wipe a key it was not given.
//
// EC2 installs a key pair's public key (RunInstances KeyName) into the default
// user's authorized_keys through cloud-init, before user-data runs. A caller
// that launches with a key pair and no PublicKey (`spawn burst --key-name`, or
// a headless Provision caller setting KeyName) had that key truncated to an
// empty file, so the key pair could no longer log in (#924). With a PublicKey
// the step still replaces the file, as `spawn launch` relies on.

// authorizedKeysStep extracts the generated step and points it at homeRoot
// instead of /home, so it can run for real under bash.
func authorizedKeysStep(t *testing.T, script, homeRoot string) string {
	t.Helper()
	// Anchored on code, not on the comment above it: the bootstrap ships
	// without comments (#1246).
	start := strings.Index(script, "if [ -n \"$LOCAL_SSH_KEY_BASE64\" ]; then")
	if start < 0 {
		t.Fatal("bootstrap has no authorized_keys step")
	}
	end := strings.Index(script[start:], "chmod 600 /home/$LOCAL_USERNAME/.ssh/authorized_keys")
	if end < 0 {
		t.Fatal("authorized_keys step has no chmod line to end at")
	}
	return strings.ReplaceAll(script[start:start+end], "/home/", homeRoot+"/")
}

func runAuthorizedKeysStep(t *testing.T, publicKey []byte, existing string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Linux-only: executes the Linux user-data bootstrap's authorized_keys step under bash against a temp home (#1002)")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	script, err := BuildLinuxBootstrap(BootstrapConfig{Username: "ec2-user", PublicKey: publicKey})
	if err != nil {
		t.Fatalf("BuildLinuxBootstrap: %v", err)
	}
	root := t.TempDir()
	sshDir := filepath.Join(root, "ec2-user", ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	keys := filepath.Join(sshDir, "authorized_keys")
	if existing != "" {
		if err := os.WriteFile(keys, []byte(existing), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	driver := "set -e\nLOCAL_USERNAME=ec2-user\nLOCAL_SSH_KEY_BASE64='" +
		base64.StdEncoding.EncodeToString(publicKey) + "'\n" + authorizedKeysStep(t, script, root)
	if out, err := exec.Command("bash", "-c", driver).CombinedOutput(); err != nil { //nolint:gosec // nosemgrep
		t.Fatalf("authorized_keys step failed: %v\n%s\n%s", err, out, driver)
	}
	got, err := os.ReadFile(keys)
	if err != nil {
		t.Fatalf("authorized_keys missing after the step: %v", err)
	}
	return string(got)
}

func TestEmptyPublicKeyKeepsTheKeyPairsKey(t *testing.T) {
	const keyPair = "ssh-ed25519 AAAAC3-from-the-ec2-key-pair burst\n"
	if got := runAuthorizedKeysStep(t, nil, keyPair); got != keyPair {
		t.Errorf("authorized_keys = %q after a keyless bootstrap, want the key pair's key %q kept", got, keyPair)
	}
}

func TestEmptyPublicKeyCreatesAnEmptyFileWhenAbsent(t *testing.T) {
	if got := runAuthorizedKeysStep(t, nil, ""); got != "" {
		t.Errorf("authorized_keys = %q, want an empty file", got)
	}
}

func TestPublicKeyReplacesAuthorizedKeys(t *testing.T) {
	const key = "ssh-ed25519 AAAAC3-spawn-launch-key me"
	got := runAuthorizedKeysStep(t, []byte(key), "ssh-ed25519 AAAAC3-stale old\n")
	if strings.TrimSpace(got) != key {
		t.Errorf("authorized_keys = %q, want exactly the supplied key %q", got, key)
	}
}
