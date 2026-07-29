package plugin

// Plugin discovery index.
//
// Resolution alone (see registry.go) can answer "fetch me the plugin named X",
// but it cannot answer "what plugins exist" — a typo and a plugin that hasn't
// been written yet both surface as a 404. This file adds the discovery half: a
// generated index of the official registry's contents that `spawn plugin search`
// and `spawn plugin info` read.
//
// The index is DERIVED, never hand-maintained: BuildIndex walks a checkout's
// plugins/*/plugin.yaml and reads each spec through the same ParseSpec the
// installer uses, so an entry can't describe a plugin differently from the spec
// it points at. spore-plugins CI regenerates it on every push that touches a
// spec, which is what keeps it from drifting (the same arrangement as
// `spawn plugin manifest`: generator here, invoked there).
//
// Scope, deliberately: entries describe plugins in the OFFICIAL registry only.
// Third-party discovery is a separate decision, because signature verification
// is pinned to the spore-plugins release workflow's OIDC identity
// (officialSigSANRegex in signature.go), so a third-party entry could never be
// backed by the trust signal a `verified` field implies. Listing one anyway
// would make `verified: false` an endorsement with nothing behind it. See
// spawn#448.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// IndexSchemaVersion is the schema version BuildIndex emits. A reader that finds
// a higher version than it knows should say so rather than silently misreading
// fields it doesn't understand.
const IndexSchemaVersion = 1

// OfficialIndexPath is the index's location in the official registry, relative
// to the repo root.
const OfficialIndexPath = "index.json"

// Index is the discovery manifest for a plugin registry.
type Index struct {
	SchemaVersion int `json:"schemaVersion"`
	// GeneratedAt is when the index was built (UTC, RFC3339). Readers surface it
	// so a stale cached index is distinguishable from a missing plugin.
	GeneratedAt time.Time `json:"generatedAt"`
	// Source is the owner/repo the index describes, e.g. spore-host/spore-plugins.
	Source  string       `json:"source"`
	Plugins []IndexEntry `json:"plugins"`
}

// IndexEntry describes one plugin available in the registry. It carries only
// what discovery needs — enough to decide whether to install, not a copy of the
// spec. Installing still fetches and verifies the real plugin.yaml.
type IndexEntry struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	// Path is the spec's location within Source, e.g. plugins/tailscale/plugin.yaml.
	// Recorded rather than reconstructed so the layout lives in one place.
	Path string `json:"path"`
	// Verified is true when the entry describes a plugin in the spore-host-owned
	// registry, whose releases are signed by the spore-plugins release workflow
	// and therefore checkable at install time. It is a statement about
	// provenance, not about the plugin being safe or reviewed.
	Verified bool `json:"verified"`
	// ConfigParams names the plugin's config keys, with required ones marked, so
	// `plugin info` can show what must be supplied without a second fetch.
	ConfigParams []IndexConfigParam `json:"configParams,omitempty"`
	// RequiresRoot and OpensPorts mirror the spec's declared permissions so
	// search results can flag a plugin that wants root before it's installed.
	// Absent permissions leave both zero — "not declared", not "declares none".
	RequiresRoot bool  `json:"requiresRoot,omitempty"`
	OpensPorts   []int `json:"opensPorts,omitempty"`
}

// IndexConfigParam is one config key as surfaced by discovery.
type IndexConfigParam struct {
	Name     string `json:"name"`
	Type     string `json:"type,omitempty"`
	Required bool   `json:"required,omitempty"`
}

// BuildIndex walks pluginsDir (the registry checkout's plugins/ directory) and
// builds an index from every plugin.yaml it contains. source is the owner/repo
// the index describes; verified reflects whether that repo is the spore-host
// registry.
//
// generatedAt is passed in rather than read from the clock so the caller
// controls it — CI stamps the commit time, and tests stamp a fixed value, which
// keeps a regenerated index byte-identical when nothing substantive changed.
//
// A spec that fails to parse is a hard error: a half-built index that silently
// omits a plugin is worse than no index, since the omission reads as "that
// plugin doesn't exist".
func BuildIndex(pluginsDir, source string, verified bool, generatedAt time.Time) (*Index, error) {
	entries, err := os.ReadDir(pluginsDir)
	if err != nil {
		return nil, fmt.Errorf("read plugins dir: %w", err)
	}

	idx := &Index{
		SchemaVersion: IndexSchemaVersion,
		GeneratedAt:   generatedAt.UTC().Truncate(time.Second),
		Source:        source,
		Plugins:       []IndexEntry{},
	}

	for _, de := range entries {
		if !de.IsDir() {
			continue
		}
		specPath := filepath.Join(pluginsDir, de.Name(), "plugin.yaml")
		if _, statErr := os.Stat(specPath); statErr != nil {
			// A directory with no plugin.yaml isn't a plugin (docs, fixtures).
			continue
		}
		spec, err := ParseSpecFile(specPath)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", specPath, err)
		}
		// The directory name is what a bare-name ref resolves through, so an
		// entry whose spec name disagrees would be unresolvable. Validate
		// enforces this too; checking here means a bad pairing can't reach the
		// index even if the generator is run without validation.
		if spec.Name != de.Name() {
			return nil, fmt.Errorf("%s: spec name %q does not match directory %q", specPath, spec.Name, de.Name())
		}

		entry := IndexEntry{
			Name:        spec.Name,
			Version:     spec.Version,
			Description: spec.Description,
			// Slash-joined, not filepath.Join: this is a URL path within the
			// repo, so it must not pick up a backslash separator on Windows.
			Path:     "plugins/" + de.Name() + "/plugin.yaml",
			Verified: verified,
		}
		for name, p := range spec.Config {
			entry.ConfigParams = append(entry.ConfigParams, IndexConfigParam{
				Name:     name,
				Type:     p.Type,
				Required: p.Required,
			})
		}
		// Map iteration is random; sort so regenerating an unchanged registry
		// produces an identical file and CI doesn't commit churn.
		sort.Slice(entry.ConfigParams, func(i, j int) bool {
			return entry.ConfigParams[i].Name < entry.ConfigParams[j].Name
		})
		if spec.Permissions != nil {
			entry.RequiresRoot = spec.Permissions.Instance.Root
			entry.OpensPorts = spec.Permissions.Instance.Ports
		}
		idx.Plugins = append(idx.Plugins, entry)
	}

	sort.Slice(idx.Plugins, func(i, j int) bool { return idx.Plugins[i].Name < idx.Plugins[j].Name })

	if len(idx.Plugins) == 0 {
		return nil, fmt.Errorf("no plugin.yaml found under %s", pluginsDir)
	}
	return idx, nil
}

// Encode renders the index as the bytes to commit: indented JSON with a
// trailing newline, so a diff of a regenerated index is readable line-by-line
// and the file is well-formed for tools that expect one.
func (idx *Index) Encode() ([]byte, error) {
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode index: %w", err)
	}
	return append(data, '\n'), nil
}

// ParseIndex decodes an index. It rejects a schema version it doesn't
// understand rather than reading fields that may have changed meaning.
func ParseIndex(data []byte) (*Index, error) {
	var idx Index
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("parse index: %w", err)
	}
	if idx.SchemaVersion > IndexSchemaVersion {
		return nil, fmt.Errorf("index schemaVersion %d is newer than this spawn understands (%d) — upgrade spawn",
			idx.SchemaVersion, IndexSchemaVersion)
	}
	return &idx, nil
}

// Lookup returns the entry for name, or false. Names are matched exactly:
// resolution is exact, so a fuzzy match here would report a plugin that
// `install` then couldn't fetch.
func (idx *Index) Lookup(name string) (IndexEntry, bool) {
	for _, e := range idx.Plugins {
		if e.Name == name {
			return e, true
		}
	}
	return IndexEntry{}, false
}

// Search returns entries matching query as a case-insensitive substring of the
// name or description, sorted with name matches first (a user typing "jupyter"
// wants the jupyterlab plugin above one that merely mentions Jupyter). An empty
// query returns everything.
func (idx *Index) Search(query string) []IndexEntry {
	if strings.TrimSpace(query) == "" {
		out := make([]IndexEntry, len(idx.Plugins))
		copy(out, idx.Plugins)
		return out
	}
	q := strings.ToLower(strings.TrimSpace(query))
	var nameHits, descHits []IndexEntry
	for _, e := range idx.Plugins {
		switch {
		case strings.Contains(strings.ToLower(e.Name), q):
			nameHits = append(nameHits, e)
		case strings.Contains(strings.ToLower(e.Description), q):
			descHits = append(descHits, e)
		}
	}
	return append(nameHits, descHits...)
}
