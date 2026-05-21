package plugin

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
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
//   - name                    official registry (scttfrdmn/spore-plugins)
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
		Owner:   "scttfrdmn",
		Repo:    "spore-plugins",
		Name:    name,
		Version: version,
	}
}

// DefaultResolver returns a composite resolver that handles all ref formats.
func DefaultResolver() RegistryResolver {
	return &compositeResolver{}
}

type compositeResolver struct{}

func (r *compositeResolver) Resolve(ctx context.Context, ref string) (*PluginSpec, error) {
	pr := ParseRef(ref)
	switch pr.Host {
	case "local":
		return ParseSpecFile(pr.Name)
	case "github":
		return fetchGitHubSpec(ctx, pr.Owner, pr.Repo, pr.Name, pr.Version)
	default: // "official"
		return fetchGitHubSpec(ctx, pr.Owner, pr.Repo, pr.Name, pr.Version)
	}
}

// fetchGitHubSpec fetches a plugin.yaml from GitHub raw content.
func fetchGitHubSpec(ctx context.Context, owner, repo, name, version string) (*PluginSpec, error) {
	if owner != "scttfrdmn" {
		log.Printf("warning: installing plugin from unverified source %s/%s — content is not signed or audited", owner, repo)
	}

	// Validate each URL component to prevent path traversal.
	for _, part := range []string{owner, repo, name} {
		if !validGitHubComponent.MatchString(part) {
			return nil, fmt.Errorf("invalid registry ref component %q", part)
		}
	}

	gitRef := "main"
	if version != "" {
		gitRef = version
	}
	if !validGitRef.MatchString(gitRef) || strings.Contains(gitRef, "..") {
		return nil, fmt.Errorf("invalid git ref %q", gitRef)
	}

	url := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/%s/plugin.yaml",
		owner, repo, gitRef, name)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("plugin %q not found in %s/%s@%s", name, owner, repo, gitRef)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: HTTP %d", url, resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSpecBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if len(data) > maxSpecBytes {
		return nil, fmt.Errorf("plugin spec exceeds maximum size (%d bytes)", maxSpecBytes)
	}

	return ParseSpec(data)
}
