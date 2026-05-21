package platform

import (
	"crypto/md5"
	"crypto/x509"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"golang.org/x/crypto/ssh"
)

// Platform represents the current operating system platform
type Platform struct {
	OS            string // "windows", "linux", "darwin"
	HomeDir       string
	SSHDir        string
	SSHKeyPath    string
	SSHPubKeyPath string
	SSHClient     string
}

// Detect detects the current platform and returns configuration
func Detect() (*Platform, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get home directory: %w", err)
	}

	p := &Platform{
		OS:      runtime.GOOS,
		HomeDir: homeDir,
	}

	switch p.OS {
	case "windows":
		// Windows: C:\Users\username\.ssh\id_rsa
		p.SSHDir = filepath.Join(homeDir, ".ssh")
		p.SSHKeyPath = filepath.Join(p.SSHDir, "id_rsa")
		p.SSHPubKeyPath = filepath.Join(p.SSHDir, "id_rsa.pub")
		p.SSHClient = findWindowsSSHClient()

	case "linux", "darwin":
		// Unix: ~/.ssh/id_rsa
		p.SSHDir = filepath.Join(homeDir, ".ssh")
		p.SSHKeyPath = filepath.Join(p.SSHDir, "id_rsa")
		p.SSHPubKeyPath = filepath.Join(p.SSHDir, "id_rsa.pub")
		p.SSHClient = "ssh"
	}

	return p, nil
}

// GetSSHCommand returns the SSH command for connecting to a host
func (p *Platform) GetSSHCommand(user, host string) string {
	keyPath := p.SSHKeyPath

	if p.OS == "windows" {
		// Windows SSH uses forward slashes in paths
		keyPath = filepath.ToSlash(keyPath)
	}

	return fmt.Sprintf("%s -i %s %s@%s", p.SSHClient, keyPath, user, host)
}

// CreateSSHKey creates a new SSH key pair
func (p *Platform) CreateSSHKey() error {
	// Ensure .ssh directory exists
	if err := os.MkdirAll(p.SSHDir, 0700); err != nil {
		return fmt.Errorf("failed to create .ssh directory: %w", err)
	}

	var cmd *exec.Cmd

	switch p.OS {
	case "windows":
		// Windows 10+ includes OpenSSH
		cmd = exec.Command("ssh-keygen.exe",
			"-t", "rsa",
			"-b", "4096",
			"-f", p.SSHKeyPath,
			"-N", "", // No passphrase
			"-C", "spawn-generated-key")

	default:
		// Linux/macOS
		cmd = exec.Command("ssh-keygen",
			"-t", "rsa",
			"-b", "4096",
			"-f", p.SSHKeyPath,
			"-N", "", // No passphrase
			"-C", "spawn-generated-key")
	}

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to create SSH key: %w", err)
	}

	return nil
}

// HasSSHKey checks if an SSH key exists
func (p *Platform) HasSSHKey() bool {
	_, err := os.Stat(p.SSHPubKeyPath)
	return err == nil
}

// ReadPublicKey reads the SSH public key
func (p *Platform) ReadPublicKey() ([]byte, error) {
	return os.ReadFile(p.SSHPubKeyPath)
}

// GetUsername returns the current system username
func (p *Platform) GetUsername() string {
	if username := os.Getenv("USER"); username != "" {
		return username
	}
	if username := os.Getenv("USERNAME"); username != "" {
		return username
	}
	return "user"
}

// GetPublicKeyFingerprint returns the MD5 fingerprint of the local SSH public key
// in the same format AWS uses (MD5 hash of DER-encoded public key)
func (p *Platform) GetPublicKeyFingerprint() (string, error) {
	publicKeyData, err := p.ReadPublicKey()
	if err != nil {
		return "", fmt.Errorf("failed to read public key: %w", err)
	}

	// Parse SSH public key
	pubKey, _, _, _, err := ssh.ParseAuthorizedKey(publicKeyData)
	if err != nil {
		return "", fmt.Errorf("failed to parse SSH public key: %w", err)
	}

	// Convert to crypto public key
	cryptoKey := pubKey.(ssh.CryptoPublicKey).CryptoPublicKey()

	// Marshal to DER format (PKIX format for public keys)
	derBytes, err := x509.MarshalPKIXPublicKey(cryptoKey)
	if err != nil {
		return "", fmt.Errorf("failed to marshal public key to DER: %w", err)
	}

	// nosemgrep: use-of-md5 -- AWS SSH key fingerprint format requires MD5 (RFC 4716)
	hash := md5.Sum(derBytes)

	// Format as colon-separated hex pairs (AWS format)
	fingerprint := ""
	for i, b := range hash {
		if i > 0 {
			fingerprint += ":"
		}
		fingerprint += fmt.Sprintf("%02x", b)
	}

	return fingerprint, nil
}

// findWindowsSSHClient finds the SSH client on Windows
func findWindowsSSHClient() string {
	// Check for OpenSSH (Windows 10+)
	if path, err := exec.LookPath("ssh.exe"); err == nil {
		return path
	}

	// Check for PuTTY
	puttyPath := filepath.Join(os.Getenv("ProgramFiles"), "PuTTY", "putty.exe")
	if _, err := os.Stat(puttyPath); err == nil {
		return puttyPath
	}

	// Default to ssh (will fail if not found)
	return "ssh"
}

// EnableWindowsColors enables ANSI color support on Windows
func EnableWindowsColors() {
	if runtime.GOOS == "windows" {
		// Enable ANSI escape sequences in Windows Console
		// This works on Windows 10+ with modern terminals
		cmd := exec.Command("cmd", "/c", "")
		_ = cmd.Run()
	}
}

// GetConfigPath returns the path for spawn configuration
func (p *Platform) GetConfigPath() string {
	switch p.OS {
	case "windows":
		// Windows: %APPDATA%\spawn\config.toml
		appData := os.Getenv("APPDATA")
		if appData == "" {
			appData = filepath.Join(p.HomeDir, "AppData", "Roaming")
		}
		return filepath.Join(appData, "spawn", "config.toml")

	default:
		// Unix: ~/.spawn/config.toml
		return filepath.Join(p.HomeDir, ".spawn", "config.toml")
	}
}

// GetLogPath returns the path for spawn logs
func (p *Platform) GetLogPath() string {
	switch p.OS {
	case "windows":
		// Windows: %LOCALAPPDATA%\spawn\logs
		localAppData := os.Getenv("LOCALAPPDATA")
		if localAppData == "" {
			localAppData = filepath.Join(p.HomeDir, "AppData", "Local")
		}
		return filepath.Join(localAppData, "spawn", "logs")

	default:
		// Unix: ~/.spawn/logs
		return filepath.Join(p.HomeDir, ".spawn", "logs")
	}
}
