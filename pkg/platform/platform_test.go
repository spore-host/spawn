package platform

import (
	"crypto/rand"
	"crypto/rsa"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// md5FingerprintRe matches AWS-style MD5 fingerprints:
// 16 colon-separated lowercase hex pairs (e.g. "ab:cd:ef:...")
var md5FingerprintRe = regexp.MustCompile(`^([0-9a-f]{2}:){15}[0-9a-f]{2}$`)

// writeTempPublicKey generates a fresh 2048-bit RSA key, writes the SSH
// authorized_keys-format public key to a temp directory, and returns a
// Platform pointing at it.
func writeTempPublicKey(t *testing.T) *Platform {
	t.Helper()

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}

	pub, err := ssh.NewPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("ssh.NewPublicKey: %v", err)
	}

	dir := t.TempDir()
	pubKeyPath := filepath.Join(dir, "id_rsa.pub")
	if err := os.WriteFile(pubKeyPath, ssh.MarshalAuthorizedKey(pub), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	return &Platform{
		OS:            "linux",
		HomeDir:       dir,
		SSHDir:        dir,
		SSHKeyPath:    filepath.Join(dir, "id_rsa"),
		SSHPubKeyPath: pubKeyPath,
		SSHClient:     "ssh",
	}
}

func TestGetPublicKeyFingerprint(t *testing.T) {
	p := writeTempPublicKey(t)

	fp, err := p.GetPublicKeyFingerprint()
	if err != nil {
		t.Fatalf("GetPublicKeyFingerprint: %v", err)
	}

	if !md5FingerprintRe.MatchString(fp) {
		t.Errorf("fingerprint %q does not match AWS MD5 format", fp)
	}
}

func TestGetPublicKeyFingerprint_Deterministic(t *testing.T) {
	p := writeTempPublicKey(t)

	fp1, err := p.GetPublicKeyFingerprint()
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	fp2, err := p.GetPublicKeyFingerprint()
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if fp1 != fp2 {
		t.Errorf("fingerprint not deterministic: %q vs %q", fp1, fp2)
	}
}

func TestGetPublicKeyFingerprint_MissingFile(t *testing.T) {
	p := &Platform{SSHPubKeyPath: "/nonexistent/path/id_rsa.pub"}
	_, err := p.GetPublicKeyFingerprint()
	if err == nil {
		t.Error("expected error for missing public key file")
	}
}

func TestGetSSHCommand(t *testing.T) {
	tests := []struct {
		name      string
		platform  Platform
		user      string
		host      string
		wantParts []string
	}{
		{
			name: "linux command",
			platform: Platform{
				OS:         "linux",
				SSHClient:  "ssh",
				SSHKeyPath: "/home/user/.ssh/id_rsa",
			},
			user:      "ec2-user",
			host:      "10.0.0.1",
			wantParts: []string{"ssh", "-i", "/home/user/.ssh/id_rsa", "ec2-user@10.0.0.1"},
		},
		{
			name: "darwin command",
			platform: Platform{
				OS:         "darwin",
				SSHClient:  "ssh",
				SSHKeyPath: "/Users/alice/.ssh/id_rsa",
			},
			user:      "ubuntu",
			host:      "54.1.2.3",
			wantParts: []string{"ssh", "-i", "/Users/alice/.ssh/id_rsa", "ubuntu@54.1.2.3"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := tt.platform.GetSSHCommand(tt.user, tt.host)
			for _, part := range tt.wantParts {
				if !strings.Contains(cmd, part) {
					t.Errorf("command %q missing %q", cmd, part)
				}
			}
		})
	}
}

func TestGetConfigPath(t *testing.T) {
	t.Run("unix path", func(t *testing.T) {
		p := &Platform{OS: "linux", HomeDir: "/home/alice"}
		path := p.GetConfigPath()
		if !strings.HasPrefix(path, "/home/alice") {
			t.Errorf("path %q does not start with home dir", path)
		}
		if !strings.Contains(path, "config.toml") {
			t.Errorf("path %q does not contain config.toml", path)
		}
	})

	t.Run("darwin path", func(t *testing.T) {
		p := &Platform{OS: "darwin", HomeDir: "/Users/bob"}
		path := p.GetConfigPath()
		if !strings.HasPrefix(path, "/Users/bob") {
			t.Errorf("path %q does not start with home dir", path)
		}
		if !strings.Contains(path, "config.toml") {
			t.Errorf("path %q does not contain config.toml", path)
		}
	})
}

func TestGetLogPath(t *testing.T) {
	t.Run("unix path", func(t *testing.T) {
		p := &Platform{OS: "linux", HomeDir: "/home/alice"}
		path := p.GetLogPath()
		if !strings.HasPrefix(path, "/home/alice") {
			t.Errorf("path %q does not start with home dir", path)
		}
		if !strings.HasSuffix(path, "logs") {
			t.Errorf("path %q does not end with logs", path)
		}
	})
}

func TestGetUsername(t *testing.T) {
	p := &Platform{}

	// Set a known USER env var for the test.
	orig := os.Getenv("USER")
	t.Cleanup(func() { _ = os.Setenv("USER", orig) })

	if err := os.Setenv("USER", "testuser"); err != nil {
		t.Fatal(err)
	}
	if got := p.GetUsername(); got != "testuser" {
		t.Errorf("GetUsername = %q, want testuser", got)
	}

	// When USER is unset and USERNAME is set.
	_ = os.Unsetenv("USER")
	origUN := os.Getenv("USERNAME")
	t.Cleanup(func() { _ = os.Setenv("USERNAME", origUN) })
	if err := os.Setenv("USERNAME", "winuser"); err != nil {
		t.Fatal(err)
	}
	if got := p.GetUsername(); got != "winuser" {
		t.Errorf("GetUsername (USERNAME) = %q, want winuser", got)
	}

	// When both unset, falls back to "user".
	_ = os.Unsetenv("USERNAME")
	if got := p.GetUsername(); got != "user" {
		t.Errorf("GetUsername (fallback) = %q, want user", got)
	}
}

func TestHasSSHKey(t *testing.T) {
	t.Run("returns true when file exists", func(t *testing.T) {
		p := writeTempPublicKey(t)
		if !p.HasSSHKey() {
			t.Error("HasSSHKey should return true when pub key file exists")
		}
	})

	t.Run("returns false when file missing", func(t *testing.T) {
		p := &Platform{SSHPubKeyPath: "/nonexistent/id_rsa.pub"}
		if p.HasSSHKey() {
			t.Error("HasSSHKey should return false when file is missing")
		}
	})
}
