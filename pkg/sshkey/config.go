package sshkey

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// This file manages a spawn-owned SSH client-config include so that tools which
// shell out to the system `ssh` — notably mutagen, which has no key/identity
// flag — can authenticate to spawn instances without the user having to load a
// key into ssh-agent.
//
// Why a config include and not `ssh-add`: `ssh-add` targets whatever
// $SSH_AUTH_SOCK points at, which on many setups is NOT the agent `ssh` uses
// (e.g. a `~/.ssh/config` `IdentityAgent` pointing at 1Password, whose agent is
// also read-only). An `IdentityFile` in ssh_config, by contrast, is honored by
// every `ssh` invocation regardless of agent, and `IdentitiesOnly yes` makes ssh
// use exactly that key rather than offering agent keys first.
//
// Layout: spawn owns ~/.spawn/ssh_config (a block per host, delimited by markers
// so entries can be added/removed idempotently) and adds a single
// `Include ~/.spawn/ssh_config` line to the top of ~/.ssh/config.

const (
	sshConfigIncludeName = "ssh_config" // under ~/.spawn/
	blockBeginFmt        = "# >>> spawn plugin host %s >>>"
	blockEndFmt          = "# <<< spawn plugin host %s <<<"
)

// SpawnSSHConfigPath returns the path to spawn's managed ssh_config include.
func SpawnSSHConfigPath(homeDir string) string {
	return filepath.Join(homeDir, ".spawn", sshConfigIncludeName)
}

// EnsureHostIdentity makes `ssh <host>` (and therefore mutagen) use keyPath for
// the given host, by writing a Host block into spawn's managed ssh_config
// include and ensuring the user's ~/.ssh/config includes it. It is idempotent:
// re-invoking for the same host replaces that host's block (e.g. after a
// stop/start changes the HostName/IP). host is the value used on the SSH command
// line (a bare hostname/IP; user@ip is not a valid Host token, so callers pass
// the IP/DNS and let the block/HostName carry it).
func EnsureHostIdentity(homeDir, host, keyPath string) error {
	if host == "" || keyPath == "" {
		return fmt.Errorf("host and keyPath are required")
	}
	if err := ensureSpawnConfigIncluded(homeDir); err != nil {
		return err
	}
	block := hostBlock(host, keyPath)
	return upsertBlock(SpawnSSHConfigPath(homeDir), host, block)
}

// hostBlock renders the Host stanza for host → keyPath.
func hostBlock(host, keyPath string) string {
	var b strings.Builder
	fmt.Fprintf(&b, blockBeginFmt+"\n", host)
	fmt.Fprintf(&b, "Host %s\n", host)
	fmt.Fprintf(&b, "    IdentityFile %s\n", keyPath)
	// Use exactly this key and ignore whatever agent IdentityAgent points at
	// (e.g. 1Password), which would otherwise be offered first.
	b.WriteString("    IdentitiesOnly yes\n")
	fmt.Fprintf(&b, blockEndFmt+"\n", host)
	return b.String()
}

// upsertBlock writes block for host into path, replacing an existing block for
// the same host (matched by its BEGIN/END markers) or appending a new one.
func upsertBlock(path, host, block string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create spawn config dir: %w", err)
	}
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read spawn ssh_config: %w", err)
	}
	out := replaceBlock(string(existing), host, block)
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
		return fmt.Errorf("write spawn ssh_config: %w", err)
	}
	return nil
}

// RemoveHostIdentity removes the managed block for host, if present. A missing
// file or block is not an error.
func RemoveHostIdentity(homeDir, host string) error {
	path := SpawnSSHConfigPath(homeDir)
	existing, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read spawn ssh_config: %w", err)
	}
	out := replaceBlock(string(existing), host, "")
	return os.WriteFile(path, []byte(out), 0o600)
}

// replaceBlock returns content with host's marked block replaced by newBlock
// (empty newBlock deletes it, appended otherwise if absent). Leftover blank runs
// are collapsed so repeated add/remove cycles don't accumulate whitespace.
func replaceBlock(content, host, newBlock string) string {
	begin := fmt.Sprintf(blockBeginFmt, host)
	end := fmt.Sprintf(blockEndFmt, host)

	var kept []string
	inBlock := false
	found := false
	sc := bufio.NewScanner(bytes.NewReader([]byte(content)))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.TrimSpace(line) == begin:
			inBlock = true
			found = true
		case strings.TrimSpace(line) == end:
			inBlock = false
		case !inBlock:
			kept = append(kept, line)
		}
	}

	body := strings.TrimRight(strings.Join(kept, "\n"), "\n")
	trimmedNew := strings.TrimRight(newBlock, "\n")
	if trimmedNew == "" {
		// Deletion only.
		if body == "" {
			return ""
		}
		return body + "\n"
	}
	if body == "" {
		return trimmedNew + "\n"
	}
	_ = found
	return body + "\n" + trimmedNew + "\n"
}

// ensureSpawnConfigIncluded makes sure ~/.ssh/config begins with an
// `Include ~/.spawn/ssh_config` directive so ssh reads spawn's managed blocks.
// Idempotent; creates ~/.ssh/config if absent. The Include must appear before
// any `Host *` catch-all to take effect, so it is prepended.
func ensureSpawnConfigIncluded(homeDir string) error {
	sshDir := filepath.Join(homeDir, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		return fmt.Errorf("create ~/.ssh: %w", err)
	}
	userConfig := filepath.Join(sshDir, "config")
	includeLine := "Include " + SpawnSSHConfigPath(homeDir)

	existing, err := os.ReadFile(userConfig)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read ~/.ssh/config: %w", err)
	}
	if includeContainsDirective(string(existing), SpawnSSHConfigPath(homeDir)) {
		return nil
	}

	header := "# Added by spawn: use spawn-managed per-host keys (see ~/.spawn/ssh_config).\n" + includeLine + "\n\n"
	out := header + string(existing)
	if err := os.WriteFile(userConfig, []byte(out), 0o600); err != nil {
		return fmt.Errorf("write ~/.ssh/config: %w", err)
	}
	return nil
}

// includeContainsDirective reports whether config already has an Include line
// referencing spawnConfigPath (matching either the absolute path or the ~ form).
func includeContainsDirective(config, spawnConfigPath string) bool {
	sc := bufio.NewScanner(strings.NewReader(config))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && strings.EqualFold(fields[0], "Include") {
			for _, f := range fields[1:] {
				if f == spawnConfigPath || strings.HasSuffix(f, filepath.Join(".spawn", sshConfigIncludeName)) {
					return true
				}
			}
		}
	}
	return false
}
