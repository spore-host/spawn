package plugin

// Fetching and caching the discovery index.
//
// `spawn plugin search` should work on a plane. The index is small, changes only
// when a plugin is published, and is purely advisory — nothing about it gates
// security decisions at install time, which still fetches and verifies the real
// plugin.yaml. So it is cached on disk and served from cache when the network
// isn't there, with the index's own generatedAt surfaced so the reader can tell
// a stale answer from a current one. A cached index that is merely old is useful;
// an index that silently pretends to be current is not.
//
// Nothing here is a security boundary: the index says a plugin exists, and
// install independently fetches and verifies its plugin.yaml. A poisoned cache
// can at worst misdescribe a listing, never cause different code to be run.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// indexCacheTTL is how long a cached index is served without revalidating. The
// registry publishes on merge, so a few hours is a reasonable staleness budget
// for a discovery listing; `--refresh` bypasses it.
const indexCacheTTL = 6 * time.Hour

// IndexSource identifies the registry an index is fetched from.
type IndexSource struct {
	Owner string
	Repo  string
	Ref   string // git ref; "main" when empty
}

// OfficialIndexSource is the spore-host registry.
func OfficialIndexSource() IndexSource {
	return IndexSource{Owner: "spore-host", Repo: "spore-plugins", Ref: "main"}
}

func (s IndexSource) slug() string {
	return s.Owner + "/" + s.Repo
}

// IndexResult is an index plus where it came from, so callers can report
// provenance and age without re-deriving them.
type IndexResult struct {
	Index *Index
	// FromCache is true when the network was not consulted for this result.
	FromCache bool
	// CachedAt is when the served cache file was written; zero for a fresh fetch.
	CachedAt time.Time
	// StaleErr records why the network fetch failed, when a cached index was
	// served in its place. Callers surface it: falling back silently would make
	// an offline machine look like a registry with nothing new in it.
	StaleErr error
}

// IndexFetcher loads a registry index, caching it on disk.
type IndexFetcher struct {
	// RawBase is the raw.githubusercontent.com base; overridable for tests.
	RawBase string
	// CacheDir is where index copies are written; defaults to ~/.spawn/cache.
	CacheDir string
	// HTTPClient is the client used for fetches; defaults to a 15s-timeout client.
	HTTPClient *http.Client
	// Now returns the current time; overridable for tests.
	Now func() time.Time
}

// NewIndexFetcher returns a fetcher with the default endpoints and cache dir.
func NewIndexFetcher() *IndexFetcher {
	return &IndexFetcher{
		RawBase:    defaultRawBase,
		HTTPClient: &http.Client{Timeout: 15 * time.Second},
		Now:        time.Now,
	}
}

func (f *IndexFetcher) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

func (f *IndexFetcher) httpClient() *http.Client {
	if f.HTTPClient != nil {
		return f.HTTPClient
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// cachePath returns the on-disk location for a source's cached index.
func (f *IndexFetcher) cachePath(src IndexSource) (string, error) {
	dir := f.CacheDir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate home dir for index cache: %w", err)
		}
		dir = filepath.Join(home, ".spawn", "cache")
	}
	// Owner and repo are validated against validGitHubComponent before use, so
	// they cannot contain a path separator and can't escape the cache dir.
	return filepath.Join(dir, fmt.Sprintf("plugin-index-%s-%s.json", src.Owner, src.Repo)), nil
}

// Fetch returns the index for src. Unless refresh is set, a cache file younger
// than indexCacheTTL is served without touching the network. When the network
// fetch fails and any cached copy exists, that copy is served with StaleErr set
// rather than failing outright — discovery degrading to "here's what I knew as
// of <time>" beats it not working offline.
func (f *IndexFetcher) Fetch(ctx context.Context, src IndexSource, refresh bool) (*IndexResult, error) {
	for _, part := range []string{src.Owner, src.Repo} {
		if !validGitHubComponent.MatchString(part) {
			return nil, fmt.Errorf("invalid index source component %q", part)
		}
	}
	ref := src.Ref
	if ref == "" {
		ref = "main"
	}
	if !validGitRef.MatchString(ref) {
		return nil, fmt.Errorf("invalid git ref %q", ref)
	}

	cachePath, cacheErr := f.cachePath(src)

	if !refresh && cacheErr == nil {
		if idx, modTime, err := f.readCache(cachePath); err == nil {
			if f.now().Sub(modTime) < indexCacheTTL {
				return &IndexResult{Index: idx, FromCache: true, CachedAt: modTime}, nil
			}
		}
	}

	idx, fetchErr := f.fetchRemote(ctx, src, ref)
	if fetchErr == nil {
		if cacheErr == nil {
			// A cache write failure must not fail the command — the index is in
			// hand and usable; the only cost is refetching next time.
			f.writeCache(cachePath, idx)
		}
		return &IndexResult{Index: idx}, nil
	}

	if cacheErr == nil {
		if idx, modTime, err := f.readCache(cachePath); err == nil {
			return &IndexResult{Index: idx, FromCache: true, CachedAt: modTime, StaleErr: fetchErr}, nil
		}
	}
	return nil, fetchErr
}

func (f *IndexFetcher) fetchRemote(ctx context.Context, src IndexSource, ref string) (*Index, error) {
	rawBase := f.RawBase
	if rawBase == "" {
		rawBase = defaultRawBase
	}
	url := fmt.Sprintf("%s/%s/%s/%s/%s", rawBase, src.Owner, src.Repo, ref, OfficialIndexPath)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	resp, err := f.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch plugin index: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("no plugin index published at %s/%s@%s (%s)", src.Owner, src.Repo, ref, OfficialIndexPath)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch plugin index: HTTP %d", resp.StatusCode)
	}

	// Bounded read: the index is a small listing, and an unbounded read of a
	// remote body is a memory-exhaustion foot-gun.
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("read plugin index: %w", err)
	}
	idx, err := ParseIndex(data)
	if err != nil {
		return nil, err
	}
	if idx.Source == "" {
		idx.Source = src.slug()
	}
	return idx, nil
}

func (f *IndexFetcher) readCache(path string) (*Index, time.Time, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, time.Time{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, time.Time{}, err
	}
	idx, err := ParseIndex(data)
	if err != nil {
		return nil, time.Time{}, err
	}
	return idx, info.ModTime(), nil
}

func (f *IndexFetcher) writeCache(path string, idx *Index) {
	data, err := idx.Encode()
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	// Write-then-rename so a crash mid-write can't leave a truncated cache that
	// later parses as a registry with fewer plugins than it has.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
	}
}
