package plugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ManifestSchemaVersion is the current manifest.json schema version. Bump it on a
// breaking change to the manifest shape so an older spawn can reject a newer
// manifest cleanly rather than misread it.
const ManifestSchemaVersion = 1

// ManifestFileName is the manifest asset's filename in a plugin release.
const ManifestFileName = "manifest.json"

// Manifest is the per-plugin release checksum manifest, published as a GitHub
// Release asset (manifest.json) for the tag <name>-<version>. It records the
// sha256 of the plugin's security-relevant bytes so `spawn` can verify that the
// plugin.yaml it fetched matches the one the maintainers released. Increment 3
// (signing) signs this manifest; increment 2 (this) only asserts the digests.
//
// Files maps a repo-relative path (within the plugin's directory) to its
// lowercase-hex sha256. Today it contains only "plugin.yaml" — the sole executed
// artifact — but the map shape leaves room to cover first-party assets later
// without a schema bump.
type Manifest struct {
	SchemaVersion int               `json:"schema_version"`
	Plugin        string            `json:"plugin"`
	Version       string            `json:"version"`
	Files         map[string]string `json:"files"`
}

// PluginYAMLSHA256 returns the recorded sha256 of the plugin's plugin.yaml, or
// "" if the manifest doesn't cover it.
func (m *Manifest) PluginYAMLSHA256() string {
	if m == nil {
		return ""
	}
	return m.Files["plugin.yaml"]
}

// BuildManifest reads plugins/<name>/plugin.yaml under pluginDir, parses it to
// recover the plugin name and version, and returns a Manifest recording the
// plugin.yaml sha256. pluginDir is the plugin's own directory (e.g.
// plugins/tailscale). The returned manifest's Version is the spec's version.
func BuildManifest(pluginDir string) (*Manifest, error) {
	specPath := filepath.Join(pluginDir, "plugin.yaml")
	data, err := os.ReadFile(specPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", specPath, err)
	}
	spec, err := ParseSpec(data)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", specPath, err)
	}
	if spec.Version == "" {
		return nil, fmt.Errorf("%s: plugin.yaml has no version", specPath)
	}
	return &Manifest{
		SchemaVersion: ManifestSchemaVersion,
		Plugin:        spec.Name,
		Version:       spec.Version,
		Files:         map[string]string{"plugin.yaml": sha256Hex(data)},
	}, nil
}

// ParseManifest decodes a manifest.json, rejecting a schema version this build
// doesn't understand (a newer manifest against an older spawn) so verification
// fails closed rather than silently ignoring unknown integrity data.
func ParseManifest(data []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if m.SchemaVersion != ManifestSchemaVersion {
		return nil, fmt.Errorf("manifest schema version %d unsupported (this spawn understands %d) — upgrade spawn", m.SchemaVersion, ManifestSchemaVersion)
	}
	if m.PluginYAMLSHA256() == "" {
		return nil, fmt.Errorf("manifest does not record a plugin.yaml checksum")
	}
	if !sha256HexRe.MatchString(m.PluginYAMLSHA256()) {
		return nil, fmt.Errorf("manifest plugin.yaml checksum %q is not a valid sha256", m.PluginYAMLSHA256())
	}
	return &m, nil
}

// Encode renders the manifest as indented JSON with a trailing newline, suitable
// for writing to manifest.json. json.MarshalIndent sorts map keys, so the output
// is deterministic for a given manifest.
func (m *Manifest) Encode() ([]byte, error) {
	if len(m.Files) == 0 {
		return nil, fmt.Errorf("manifest has no files")
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
