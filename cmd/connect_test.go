package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spore-host/libs/i18n"
)

// TestConnectCommand_HasSSHAlias validates that 'ssh' is an alias for 'connect'
func TestConnectCommand_HasSSHAlias(t *testing.T) {
	// Check that connectCmd has the 'ssh' alias
	found := false
	for _, alias := range connectCmd.Aliases {
		if alias == "ssh" {
			found = true
			break
		}
	}

	if !found {
		t.Error("connectCmd should have 'ssh' as an alias, but it was not found")
	}
}

// TestConnectCommand_SSHAliasVerification validates all expected aliases
func TestConnectCommand_SSHAliasVerification(t *testing.T) {
	expectedAliases := []string{"ssh"}

	if len(connectCmd.Aliases) != len(expectedAliases) {
		t.Errorf("Expected %d aliases, got %d", len(expectedAliases), len(connectCmd.Aliases))
	}

	for _, expected := range expectedAliases {
		found := false
		for _, alias := range connectCmd.Aliases {
			if alias == expected {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Expected alias %q not found in connectCmd.Aliases", expected)
		}
	}
}

// TestConnectCommand_DefaultValues validates default flag values
func TestConnectCommand_DefaultValues(t *testing.T) {
	tests := []struct {
		name          string
		flagName      string
		expectedValue interface{}
	}{
		{"Default user is empty", "user", ""},
		{"Default key is empty", "key", ""},
		{"Default port is 22", "port", 22},
		{"Default session-manager is false", "session-manager", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flag := connectCmd.Flags().Lookup(tt.flagName)
			if flag == nil {
				t.Fatalf("Flag %q not found", tt.flagName)
			}

			// Get the default value
			defaultValue := flag.DefValue

			switch expected := tt.expectedValue.(type) {
			case string:
				if defaultValue != expected {
					t.Errorf("Flag %q default = %q, want %q", tt.flagName, defaultValue, expected)
				}
			case int:
				if defaultValue != "22" && tt.flagName == "port" {
					t.Errorf("Flag %q default = %q, want \"22\"", tt.flagName, defaultValue)
				}
			case bool:
				if defaultValue != "false" && tt.flagName == "session-manager" {
					t.Errorf("Flag %q default = %q, want \"false\"", tt.flagName, defaultValue)
				}
			}
		})
	}
}

// TestFindSSHKey_ExactName validates finding key with exact name
func TestFindSSHKey_ExactName(t *testing.T) {
	// Create temporary SSH directory
	tmpDir := t.TempDir()
	sshDir := filepath.Join(tmpDir, ".ssh")
	err := os.Mkdir(sshDir, 0700)
	if err != nil {
		t.Fatalf("Failed to create temp SSH dir: %v", err)
	}

	// Create a key file with exact name
	keyName := "my-test-key"
	keyPath := filepath.Join(sshDir, keyName)
	err = os.WriteFile(keyPath, []byte("fake key content"), 0600)
	if err != nil {
		t.Fatalf("Failed to write key file: %v", err)
	}

	// Temporarily override home directory
	oldHome := os.Getenv("HOME")
	if err := os.Setenv("HOME", tmpDir); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Setenv("HOME", oldHome); err != nil {
			t.Fatal(err)
		}
	}()

	// Test finding the key
	foundPath, err := findSSHKey(keyName)
	if err != nil {
		t.Errorf("findSSHKey(%q) returned error: %v", keyName, err)
	}

	if foundPath != keyPath {
		t.Errorf("findSSHKey(%q) = %q, want %q", keyName, foundPath, keyPath)
	}
}

// TestFindSSHKey_WithPemExtension validates finding key with .pem extension
func TestFindSSHKey_WithPemExtension(t *testing.T) {
	tmpDir := t.TempDir()
	sshDir := filepath.Join(tmpDir, ".ssh")
	err := os.Mkdir(sshDir, 0700)
	if err != nil {
		t.Fatalf("Failed to create temp SSH dir: %v", err)
	}

	// Create key with .pem extension
	keyName := "my-key"
	keyPath := filepath.Join(sshDir, keyName+".pem")
	err = os.WriteFile(keyPath, []byte("fake key"), 0600)
	if err != nil {
		t.Fatalf("Failed to write key file: %v", err)
	}

	oldHome := os.Getenv("HOME")
	if err := os.Setenv("HOME", tmpDir); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Setenv("HOME", oldHome); err != nil {
			t.Fatal(err)
		}
	}()

	foundPath, err := findSSHKey(keyName)
	if err != nil {
		t.Errorf("findSSHKey(%q) returned error: %v", keyName, err)
	}

	if foundPath != keyPath {
		t.Errorf("findSSHKey(%q) = %q, want %q", keyName, foundPath, keyPath)
	}
}

// TestFindSSHKey_WithKeyExtension validates finding key with .key extension
func TestFindSSHKey_WithKeyExtension(t *testing.T) {
	tmpDir := t.TempDir()
	sshDir := filepath.Join(tmpDir, ".ssh")
	err := os.Mkdir(sshDir, 0700)
	if err != nil {
		t.Fatalf("Failed to create temp SSH dir: %v", err)
	}

	keyName := "my-key"
	keyPath := filepath.Join(sshDir, keyName+".key")
	err = os.WriteFile(keyPath, []byte("fake key"), 0600)
	if err != nil {
		t.Fatalf("Failed to write key file: %v", err)
	}

	oldHome := os.Getenv("HOME")
	if err := os.Setenv("HOME", tmpDir); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Setenv("HOME", oldHome); err != nil {
			t.Fatal(err)
		}
	}()

	foundPath, err := findSSHKey(keyName)
	if err != nil {
		t.Errorf("findSSHKey(%q) returned error: %v", keyName, err)
	}

	if foundPath != keyPath {
		t.Errorf("findSSHKey(%q) = %q, want %q", keyName, foundPath, keyPath)
	}
}

// TestFindSSHKey_DefaultIdRsa validates fallback to id_rsa
func TestFindSSHKey_DefaultIdRsa(t *testing.T) {
	tmpDir := t.TempDir()
	sshDir := filepath.Join(tmpDir, ".ssh")
	err := os.Mkdir(sshDir, 0700)
	if err != nil {
		t.Fatalf("Failed to create temp SSH dir: %v", err)
	}

	// Create default id_rsa key
	keyPath := filepath.Join(sshDir, "id_rsa")
	err = os.WriteFile(keyPath, []byte("fake key"), 0600)
	if err != nil {
		t.Fatalf("Failed to write key file: %v", err)
	}

	oldHome := os.Getenv("HOME")
	if err := os.Setenv("HOME", tmpDir); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Setenv("HOME", oldHome); err != nil {
			t.Fatal(err)
		}
	}()

	// Try to find a key that doesn't exist - should fallback to id_rsa
	foundPath, err := findSSHKey("nonexistent-key")
	if err != nil {
		t.Errorf("findSSHKey(\"nonexistent-key\") returned error: %v (should fallback to id_rsa)", err)
	}

	if foundPath != keyPath {
		t.Errorf("findSSHKey(\"nonexistent-key\") = %q, want %q (should fallback to id_rsa)", foundPath, keyPath)
	}
}

// TestFindSSHKey_Prioritization validates key search order
func TestFindSSHKey_Prioritization(t *testing.T) {
	tmpDir := t.TempDir()
	sshDir := filepath.Join(tmpDir, ".ssh")
	err := os.Mkdir(sshDir, 0700)
	if err != nil {
		t.Fatalf("Failed to create temp SSH dir: %v", err)
	}

	keyName := "my-key"

	// Create multiple matching keys
	exactPath := filepath.Join(sshDir, keyName)
	pemPath := filepath.Join(sshDir, keyName+".pem")
	keyPath := filepath.Join(sshDir, keyName+".key")

	// Write all keys
	_ = os.WriteFile(exactPath, []byte("exact"), 0600)
	_ = os.WriteFile(pemPath, []byte("pem"), 0600)
	_ = os.WriteFile(keyPath, []byte("key"), 0600)

	oldHome := os.Getenv("HOME")
	if err := os.Setenv("HOME", tmpDir); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Setenv("HOME", oldHome); err != nil {
			t.Fatal(err)
		}
	}()

	// Should find exact name first
	foundPath, err := findSSHKey(keyName)
	if err != nil {
		t.Errorf("findSSHKey(%q) returned error: %v", keyName, err)
	}

	if foundPath != exactPath {
		t.Errorf("findSSHKey(%q) = %q, want %q (exact name should have priority)", keyName, foundPath, exactPath)
	}
}

// TestFindSSHKey_NotFound validates error when key not found
func TestFindSSHKey_NotFound(t *testing.T) {
	tmpDir := t.TempDir()
	sshDir := filepath.Join(tmpDir, ".ssh")
	err := os.Mkdir(sshDir, 0700)
	if err != nil {
		t.Fatalf("Failed to create temp SSH dir: %v", err)
	}

	oldHome := os.Getenv("HOME")
	if err := os.Setenv("HOME", tmpDir); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Setenv("HOME", oldHome); err != nil {
			t.Fatal(err)
		}
	}()

	// Initialize i18n for testing
	if i18n.Global == nil {
		cfg := i18n.Config{Language: "en"}
		_ = i18n.Init(cfg)
	}

	// Try to find non-existent key (and no default keys exist)
	_, err = findSSHKey("definitely-does-not-exist")
	if err == nil {
		t.Error("findSSHKey(\"definitely-does-not-exist\") returned nil error, expected error")
	}
}

// TestFindSSHKey_EmptyKeyName validates error when key name is empty
func TestFindSSHKey_EmptyKeyName(t *testing.T) {
	// Initialize i18n for testing
	if i18n.Global == nil {
		cfg := i18n.Config{Language: "en"}
		_ = i18n.Init(cfg)
	}

	_, err := findSSHKey("")
	if err == nil {
		t.Error("findSSHKey(\"\") returned nil error, expected error for empty key name")
	}
}

// TestFindSSHKey_MultipleDefaultKeys validates fallback order
func TestFindSSHKey_MultipleDefaultKeys(t *testing.T) {
	tmpDir := t.TempDir()
	sshDir := filepath.Join(tmpDir, ".ssh")
	err := os.Mkdir(sshDir, 0700)
	if err != nil {
		t.Fatalf("Failed to create temp SSH dir: %v", err)
	}

	// Create only ed25519 key (id_rsa doesn't exist)
	ed25519Path := filepath.Join(sshDir, "id_ed25519")
	err = os.WriteFile(ed25519Path, []byte("fake key"), 0600)
	if err != nil {
		t.Fatalf("Failed to write key file: %v", err)
	}

	oldHome := os.Getenv("HOME")
	if err := os.Setenv("HOME", tmpDir); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Setenv("HOME", oldHome); err != nil {
			t.Fatal(err)
		}
	}()

	// Should fallback to id_ed25519 when id_rsa doesn't exist
	foundPath, err := findSSHKey("nonexistent")
	if err != nil {
		t.Errorf("findSSHKey(\"nonexistent\") returned error: %v (should fallback to id_ed25519)", err)
	}

	if foundPath != ed25519Path {
		t.Errorf("findSSHKey(\"nonexistent\") = %q, want %q", foundPath, ed25519Path)
	}
}

// TestConnectCommand_RequiresArgs validates that connect requires exactly 1 argument
func TestConnectCommand_RequiresArgs(t *testing.T) {
	// Check that Args is set to ExactArgs(1)
	if connectCmd.Args == nil {
		t.Error("connectCmd.Args is nil, should require exactly 1 argument")
	}

	// The command should have a validator function
	// We can't test the actual cobra.ExactArgs function directly,
	// but we can verify it's set
	t.Log("connectCmd has Args validator set")
}

// TestConnectCommand_FlagExists validates all expected flags exist
func TestConnectCommand_FlagExists(t *testing.T) {
	expectedFlags := []string{
		"user",
		"key",
		"port",
		"session-manager",
		"rdp",
		"ssh",
		"via-ssm",
	}

	for _, flagName := range expectedFlags {
		t.Run(flagName, func(t *testing.T) {
			flag := connectCmd.Flags().Lookup(flagName)
			if flag == nil {
				t.Errorf("Expected flag %q not found", flagName)
			}
		})
	}
}

// TestShellQuoteArgs covers the #369 fix: a post-`--` argv must reach the remote
// shell with argument boundaries intact — in particular `bash -lc "<script>"`
// must keep the whole script as one argument to -lc, not be re-split.
func TestShellQuoteArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"simple", []string{"ls", "-la"}, `'ls' '-la'`},
		{
			"bash -lc multi-token (the #369 repro)",
			[]string{"bash", "-lc", "echo hello && echo world"},
			`'bash' '-lc' 'echo hello && echo world'`,
		},
		{
			"embedded single quotes",
			[]string{"bash", "-lc", "echo 'hi'"},
			`'bash' '-lc' 'echo '\''hi'\'''`,
		},
		{"pipeline preserved as one arg", []string{"bash", "-lc", "a | b > c"}, `'bash' '-lc' 'a | b > c'`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shellQuoteArgs(tt.args); got != tt.want {
				t.Errorf("shellQuoteArgs(%q) =\n  %s\nwant\n  %s", tt.args, got, tt.want)
			}
		})
	}
}
