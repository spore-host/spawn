package sshkey

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureHostIdentity_WritesBlockAndInclude(t *testing.T) {
	home := t.TempDir()

	if err := EnsureHostIdentity(home, "1.2.3.4", "/keys/mykey"); err != nil {
		t.Fatalf("EnsureHostIdentity: %v", err)
	}

	// spawn ssh_config has the host block with IdentityFile + IdentitiesOnly.
	spawnCfg, err := os.ReadFile(SpawnSSHConfigPath(home))
	if err != nil {
		t.Fatalf("read spawn config: %v", err)
	}
	s := string(spawnCfg)
	for _, want := range []string{"Host 1.2.3.4", "IdentityFile /keys/mykey", "IdentitiesOnly yes"} {
		if !strings.Contains(s, want) {
			t.Errorf("spawn ssh_config missing %q:\n%s", want, s)
		}
	}

	// ~/.ssh/config has the Include line.
	userCfg, err := os.ReadFile(filepath.Join(home, ".ssh", "config"))
	if err != nil {
		t.Fatalf("read user config: %v", err)
	}
	if !strings.Contains(string(userCfg), "Include "+SpawnSSHConfigPath(home)) {
		t.Errorf("~/.ssh/config missing Include:\n%s", string(userCfg))
	}
}

func TestEnsureHostIdentity_IdempotentAndUpsert(t *testing.T) {
	home := t.TempDir()

	if err := EnsureHostIdentity(home, "1.2.3.4", "/keys/old"); err != nil {
		t.Fatal(err)
	}
	// Re-point the same host to a new key (simulates a stop/start IP reuse or key
	// change): the block must be REPLACED, not duplicated.
	if err := EnsureHostIdentity(home, "1.2.3.4", "/keys/new"); err != nil {
		t.Fatal(err)
	}
	s := string(mustRead(t, SpawnSSHConfigPath(home)))
	if strings.Count(s, "Host 1.2.3.4") != 1 {
		t.Errorf("expected exactly one Host block, got:\n%s", s)
	}
	if strings.Contains(s, "/keys/old") || !strings.Contains(s, "/keys/new") {
		t.Errorf("block not updated to new key:\n%s", s)
	}

	// The Include must not be duplicated across calls.
	userCfg := string(mustRead(t, filepath.Join(home, ".ssh", "config")))
	if strings.Count(userCfg, "Include ") != 1 {
		t.Errorf("Include duplicated:\n%s", userCfg)
	}
}

func TestEnsureHostIdentity_MultipleHostsAndRemove(t *testing.T) {
	home := t.TempDir()
	if err := EnsureHostIdentity(home, "1.1.1.1", "/keys/a"); err != nil {
		t.Fatal(err)
	}
	if err := EnsureHostIdentity(home, "2.2.2.2", "/keys/b"); err != nil {
		t.Fatal(err)
	}
	s := string(mustRead(t, SpawnSSHConfigPath(home)))
	if !strings.Contains(s, "Host 1.1.1.1") || !strings.Contains(s, "Host 2.2.2.2") {
		t.Fatalf("expected both hosts:\n%s", s)
	}

	if err := RemoveHostIdentity(home, "1.1.1.1"); err != nil {
		t.Fatalf("RemoveHostIdentity: %v", err)
	}
	s = string(mustRead(t, SpawnSSHConfigPath(home)))
	if strings.Contains(s, "Host 1.1.1.1") {
		t.Errorf("host 1.1.1.1 not removed:\n%s", s)
	}
	if !strings.Contains(s, "Host 2.2.2.2") {
		t.Errorf("host 2.2.2.2 wrongly removed:\n%s", s)
	}
}

func TestRemoveHostIdentity_MissingIsNoError(t *testing.T) {
	home := t.TempDir()
	if err := RemoveHostIdentity(home, "9.9.9.9"); err != nil {
		t.Errorf("remove on missing file should be a no-op, got %v", err)
	}
}

func TestEnsureSpawnConfigIncluded_PreservesExisting(t *testing.T) {
	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	original := "Host myserver\n    HostName example.com\n    User me\n"
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := EnsureHostIdentity(home, "3.3.3.3", "/keys/c"); err != nil {
		t.Fatal(err)
	}
	got := string(mustRead(t, filepath.Join(sshDir, "config")))
	if !strings.Contains(got, original) {
		t.Errorf("existing user config not preserved:\n%s", got)
	}
	if !strings.Contains(got, "Include ") {
		t.Errorf("Include not added:\n%s", got)
	}
	// Include must come before the pre-existing Host to take effect.
	if strings.Index(got, "Include ") > strings.Index(got, "Host myserver") {
		t.Errorf("Include should be prepended before existing Host stanzas:\n%s", got)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}
