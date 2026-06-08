package sshkey

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestEnsureKey_ED25519(t *testing.T) {
	home := t.TempDir()
	kp, err := EnsureKey(home, "alice", ED25519)
	if err != nil {
		t.Fatalf("EnsureKey: %v", err)
	}
	if kp.Name != "spawn-key-alice" {
		t.Errorf("name = %q, want spawn-key-alice", kp.Name)
	}
	if kp.Algorithm != ED25519 {
		t.Errorf("algorithm = %q, want ed25519", kp.Algorithm)
	}
	// Private key parses as an OpenSSH private key.
	privData, err := os.ReadFile(kp.PrivateKeyPath)
	if err != nil {
		t.Fatalf("read private key: %v", err)
	}
	if _, err := ssh.ParseRawPrivateKey(privData); err != nil {
		t.Errorf("private key does not parse: %v", err)
	}
	// Public key parses as authorized_keys and is ed25519.
	pubData, err := os.ReadFile(kp.PublicKeyPath)
	if err != nil {
		t.Fatalf("read public key: %v", err)
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey(pubData)
	if err != nil {
		t.Fatalf("public key does not parse: %v", err)
	}
	if pub.Type() != ssh.KeyAlgoED25519 {
		t.Errorf("public key type = %q, want %q", pub.Type(), ssh.KeyAlgoED25519)
	}
}

func TestEnsureKey_RSA(t *testing.T) {
	home := t.TempDir()
	kp, err := EnsureKey(home, "bob", RSA)
	if err != nil {
		t.Fatalf("EnsureKey: %v", err)
	}
	if kp.Name != "spawn-key-bob-rsa" {
		t.Errorf("name = %q, want spawn-key-bob-rsa", kp.Name)
	}
	pubData, err := os.ReadFile(kp.PublicKeyPath)
	if err != nil {
		t.Fatalf("read public key: %v", err)
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey(pubData)
	if err != nil {
		t.Fatalf("public key does not parse: %v", err)
	}
	if pub.Type() != ssh.KeyAlgoRSA {
		t.Errorf("public key type = %q, want %q (RSA required for Windows decrypt)", pub.Type(), ssh.KeyAlgoRSA)
	}
}

func TestEnsureKey_Idempotent(t *testing.T) {
	home := t.TempDir()
	kp1, err := EnsureKey(home, "alice", ED25519)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(kp1.PrivateKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	kp2, err := EnsureKey(home, "alice", ED25519)
	if err != nil {
		t.Fatal(err)
	}
	if kp1.PrivateKeyPath != kp2.PrivateKeyPath {
		t.Errorf("paths differ across calls: %q vs %q", kp1.PrivateKeyPath, kp2.PrivateKeyPath)
	}
	after, err := os.ReadFile(kp2.PrivateKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("EnsureKey regenerated the key on second call (not idempotent)")
	}
}

func TestEnsureKey_PrivatePerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix perms not meaningful on Windows")
	}
	home := t.TempDir()
	kp, err := EnsureKey(home, "alice", ED25519)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(kp.PrivateKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("private key perms = %o, want 0600", perm)
	}
}

func TestEnsureKey_BothAlgorithmsCoexist(t *testing.T) {
	home := t.TempDir()
	ed, err := EnsureKey(home, "alice", ED25519)
	if err != nil {
		t.Fatal(err)
	}
	rsa, err := EnsureKey(home, "alice", RSA)
	if err != nil {
		t.Fatal(err)
	}
	if ed.PrivateKeyPath == rsa.PrivateKeyPath {
		t.Error("ed25519 and rsa keys must have distinct paths so both can coexist")
	}
	if !fileExists(ed.PrivateKeyPath) || !fileExists(rsa.PrivateKeyPath) {
		t.Error("both keys should exist on disk")
	}
}

func TestResolve_SpawnKeyFirst(t *testing.T) {
	home := t.TempDir()
	kp, err := EnsureKey(home, "alice", ED25519)
	if err != nil {
		t.Fatal(err)
	}
	// Also drop a ~/.ssh/id_rsa to ensure the spawn key wins.
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sshDir, "id_rsa"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := Resolve(home, "spawn-key-alice")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != kp.PrivateKeyPath {
		t.Errorf("Resolve = %q, want spawn key %q", got, kp.PrivateKeyPath)
	}
}

func TestResolve_BackCompatSSHFallback(t *testing.T) {
	// No spawn key — Resolve must fall back to ~/.ssh, preserving old findSSHKey
	// behavior (exact name, then .pem/.key, then defaults).
	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		file string
		want string
	}{
		{"exact", "mykey", "mykey"},
		{"pem", "withpem", "withpem.pem"},
		{"key", "withkey", "withkey.key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := filepath.Join(sshDir, tc.want)
			if err := os.WriteFile(want, []byte("k"), 0600); err != nil {
				t.Fatal(err)
			}
			got, err := Resolve(home, tc.file)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got != want {
				t.Errorf("Resolve(%q) = %q, want %q", tc.file, got, want)
			}
		})
	}
}

func TestResolve_DefaultIdRsaFallback(t *testing.T) {
	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatal(err)
	}
	idRsa := filepath.Join(sshDir, "id_rsa")
	if err := os.WriteFile(idRsa, []byte("k"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := Resolve(home, "some-ec2-keyname-with-no-local-file")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != idRsa {
		t.Errorf("Resolve = %q, want id_rsa fallback %q", got, idRsa)
	}
}

func TestResolve_EmptyName(t *testing.T) {
	if _, err := Resolve(t.TempDir(), ""); err == nil {
		t.Error("Resolve with empty name must error")
	}
}

func TestResolve_NotFound(t *testing.T) {
	if _, err := Resolve(t.TempDir(), "nonexistent"); err == nil {
		t.Error("Resolve with no matching key must error")
	}
}

func TestFingerprint_Deterministic(t *testing.T) {
	home := t.TempDir()
	kp, err := EnsureKey(home, "alice", RSA)
	if err != nil {
		t.Fatal(err)
	}
	fp1, err := Fingerprint(kp.PublicKeyPath)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	fp2, err := Fingerprint(kp.PublicKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	if fp1 != fp2 {
		t.Errorf("fingerprint not deterministic: %q vs %q", fp1, fp2)
	}
	if len(fp1) != 47 { // 16 bytes → 32 hex + 15 colons
		t.Errorf("fingerprint %q has length %d, want 47 (AWS MD5 format)", fp1, len(fp1))
	}
}
