package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
)

// validGitHubComponent matches safe GitHub owner/repo/plugin-name components.
var validGitHubComponent = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,99}$`)

// validGitRef matches safe git refs (tags, branches, commit SHAs).
var validGitRef = regexp.MustCompile(`^[a-zA-Z0-9._/-]{1,128}$`)

// maxSpecBytes caps the plugin spec download size (64 KiB).
const maxSpecBytes = 64 * 1024

// RegistryResolver resolves plugin specs by reference string.
type RegistryResolver interface {
	Resolve(ctx context.Context, ref string) (*PluginSpec, error)
	// ResolveWithProvenance resolves the spec and also reports where it came
	// from and its content digest, so callers (inspect/install) can pin and
	// record the exact bytes. The returned Provenance is never nil on success.
	ResolveWithProvenance(ctx context.Context, ref string) (*PluginSpec, *Provenance, error)
}

// Provenance captures the resolved origin of a plugin spec: the parsed ref, the
// immutable commit SHA it resolved to (best-effort — empty when the GitHub API
// couldn't be reached), and the sha256 of the exact plugin.yaml bytes that were
// fetched. It makes "inspect then install" auditable and pinnable.
type Provenance struct {
	Host          string `json:"host"` // official | github | local
	Owner         string `json:"owner,omitempty"`
	Repo          string `json:"repo,omitempty"`
	Name          string `json:"name"`
	RequestedRef  string `json:"requested_ref,omitempty"`  // the tag/branch the user asked for ("" = default branch)
	CommitSHA     string `json:"commit_sha,omitempty"`     // resolved immutable commit (best-effort; empty if unknown)
	ContentSHA256 string `json:"content_sha256,omitempty"` // sha256 of the fetched plugin.yaml
}

// Pinned reports whether the spec resolved to an immutable reference — either a
// known commit SHA (remote) or a local file (whose bytes we hashed).
func (p *Provenance) Pinned() bool {
	if p == nil {
		return false
	}
	return p.CommitSHA != "" || p.Host == "local"
}

// PluginRef holds the parsed components of a plugin reference.
type PluginRef struct {
	Host    string // "official", "github", "local"
	Owner   string
	Repo    string
	Name    string
	Version string // empty = default branch
}

// ParseRef parses a plugin reference string into a PluginRef.
//
// Supported formats:
//   - name                    official registry (spore-host/spore-plugins)
//   - name@v1.2.0             official registry, pinned to git tag
//   - github:user/repo/name   custom GitHub repository
//   - github:user/repo/name@v1.2.0  custom GitHub repository, pinned
//   - ./path/to/plugin.yaml   local filesystem path
func ParseRef(ref string) PluginRef {
	if strings.HasPrefix(ref, "./") || strings.HasPrefix(ref, "/") || strings.HasPrefix(ref, "../") {
		return PluginRef{Host: "local", Name: ref}
	}

	if strings.HasPrefix(ref, "github:") {
		rest := strings.TrimPrefix(ref, "github:")
		var version string
		if idx := strings.LastIndex(rest, "@"); idx >= 0 {
			version = rest[idx+1:]
			rest = rest[:idx]
		}
		parts := strings.SplitN(rest, "/", 3)
		if len(parts) == 3 {
			return PluginRef{Host: "github", Owner: parts[0], Repo: parts[1], Name: parts[2], Version: version}
		}
		return PluginRef{Host: "github", Name: rest, Version: version}
	}

	// Official registry.
	var version string
	name := ref
	if idx := strings.Index(ref, "@"); idx >= 0 {
		name = ref[:idx]
		version = ref[idx+1:]
	}
	return PluginRef{
		Host:    "official",
		Owner:   "spore-host",
		Repo:    "spore-plugins",
		Name:    name,
		Version: version,
	}
}

// Default GitHub endpoints. Overridable on compositeResolver for tests.
const (
	defaultRawBase = "https://raw.githubusercontent.com"
	defaultAPIBase = "https://api.github.com"
)

// DefaultResolver returns a composite resolver that handles all ref formats.
func DefaultResolver() RegistryResolver {
	return &compositeResolver{rawBase: defaultRawBase, apiBase: defaultAPIBase}
}

type compositeResolver struct {
	rawBase string // raw.githubusercontent.com base (plugin.yaml bytes)
	apiBase string // api.github.com base (commit-SHA resolution)
}

func (r *compositeResolver) Resolve(ctx context.Context, ref string) (*PluginSpec, error) {
	spec, _, err := r.ResolveWithProvenance(ctx, ref)
	return spec, err
}

func (r *compositeResolver) ResolveWithProvenance(ctx context.Context, ref string) (*PluginSpec, *Provenance, error) {
	pr := ParseRef(ref)
	prov := &Provenance{Host: pr.Host, Owner: pr.Owner, Repo: pr.Repo, Name: pr.Name, RequestedRef: pr.Version}

	if pr.Host == "local" {
		data, err := os.ReadFile(pr.Name)
		if err != nil {
			return nil, nil, fmt.Errorf("read plugin spec %s: %w", pr.Name, err)
		}
		spec, err := ParseSpec(data)
		if err != nil {
			return nil, nil, err
		}
		prov.ContentSHA256 = sha256Hex(data)
		return spec, prov, nil
	}

	spec, data, err := r.fetchGitHubSpec(ctx, pr.Owner, pr.Repo, pr.Name, pr.Version)
	if err != nil {
		return nil, nil, err
	}
	prov.ContentSHA256 = sha256Hex(data)
	// Resolve the mutable ref to an immutable commit SHA, best-effort: a failure
	// (rate limit, offline) leaves CommitSHA empty and never blocks the install.
	prov.CommitSHA = r.resolveCommitSHA(ctx, pr.Owner, pr.Repo, pr.Version)
	return spec, prov, nil
}

// sha256Hex returns the lowercase hex sha256 of b.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// fetchGitHubSpec fetches a plugin.yaml from GitHub raw content, returning both
// the parsed spec and the exact bytes (so the caller can hash them).
func (r *compositeResolver) fetchGitHubSpec(ctx context.Context, owner, repo, name, version string) (*PluginSpec, []byte, error) {
	if owner != "spore-host" {
		log.Printf("warning: installing plugin from unverified source %s/%s — content is not signed or audited", owner, repo)
	}

	// Validate each URL component to prevent path traversal.
	for _, part := range []string{owner, repo, name} {
		if !validGitHubComponent.MatchString(part) {
			return nil, nil, fmt.Errorf("invalid registry ref component %q", part)
		}
	}

	gitRef := "main"
	if version != "" {
		gitRef = version
	}
	if !validGitRef.MatchString(gitRef) || strings.Contains(gitRef, "..") {
		return nil, nil, fmt.Errorf("invalid git ref %q", gitRef)
	}

	url := fmt.Sprintf("%s/%s/%s/%s/%s/plugin.yaml", r.rawBase, owner, repo, gitRef, name)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil, fmt.Errorf("plugin %q not found in %s/%s@%s", name, owner, repo, gitRef)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("fetch %s: HTTP %d", url, resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSpecBytes+1))
	if err != nil {
		return nil, nil, fmt.Errorf("read response: %w", err)
	}
	if len(data) > maxSpecBytes {
		return nil, nil, fmt.Errorf("plugin spec exceeds maximum size (%d bytes)", maxSpecBytes)
	}

	spec, err := ParseSpec(data)
	if err != nil {
		return nil, nil, err
	}
	return spec, data, nil
}

// resolveCommitSHA turns a mutable ref (tag/branch, or "" for the default branch)
// into an immutable commit SHA via the GitHub commits API. Best-effort: any error
// — rate limit, offline, non-200 — returns "" and is logged, never fatal. Pinning
// is an additive benefit; the content sha256 is always recorded regardless.
func (r *compositeResolver) resolveCommitSHA(ctx context.Context, owner, repo, version string) string {
	if owner == "" || repo == "" {
		return ""
	}
	ref := version
	if ref == "" {
		ref = "HEAD" // GitHub resolves HEAD to the repo's default branch tip
	}
	if !validGitRef.MatchString(ref) || strings.Contains(ref, "..") {
		return ""
	}
	url := fmt.Sprintf("%s/repos/%s/%s/commits/%s", r.apiBase, owner, repo, ref)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ""
	}
	// This media type makes the API return the bare commit SHA as the body.
	req.Header.Set("Accept", "application/vnd.github.sha")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("note: could not pin %s/%s@%s to a commit (%v); recording content digest only", owner, repo, ref, err)
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		log.Printf("note: could not pin %s/%s@%s to a commit (HTTP %d); recording content digest only", owner, repo, ref, resp.StatusCode)
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	if err != nil {
		return ""
	}
	sha := strings.TrimSpace(string(body))
	if !isHexSHA(sha) {
		return ""
	}
	return sha
}

// isHexSHA reports whether s looks like a git commit SHA (40 hex chars, or a
// 64-char sha256 object id).
func isHexSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}
