package plugin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeSpec writes a minimal valid plugin.yaml into dir/<name>/plugin.yaml.
func writeSpec(t *testing.T, dir, name, body string) {
	t.Helper()
	sub := filepath.Join(dir, name)
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", sub, err)
	}
	if err := os.WriteFile(filepath.Join(sub, "plugin.yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
}

const idxSpecTailscale = `name: tailscale
version: v2.1.0
description: Join the instance to a Tailscale tailnet
config:
  auth_key:
    type: string
    required: true
  tag:
    type: string
permissions:
  controller:
    env: []
    network: false
  instance:
    root: true
    network: true
    ports: [41641]
remote:
  install:
    - type: run
      run: echo install
`

const idxSpecJupyter = `name: jupyterlab
version: v1.0.0
description: JupyterLab notebook server
config:
  port:
    type: int
remote:
  install:
    - type: run
      run: echo install
`

func TestBuildIndex_DerivesEntriesFromSpecs(t *testing.T) {
	dir := t.TempDir()
	writeSpec(t, dir, "tailscale", idxSpecTailscale)
	writeSpec(t, dir, "jupyterlab", idxSpecJupyter)

	stamp := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	idx, err := BuildIndex(dir, "spore-host/spore-plugins", true, stamp)
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}

	if idx.SchemaVersion != IndexSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", idx.SchemaVersion, IndexSchemaVersion)
	}
	if !idx.GeneratedAt.Equal(stamp) {
		t.Errorf("GeneratedAt = %v, want %v", idx.GeneratedAt, stamp)
	}
	// Sorted by name, so jupyterlab precedes tailscale regardless of readdir order.
	if len(idx.Plugins) != 2 || idx.Plugins[0].Name != "jupyterlab" || idx.Plugins[1].Name != "tailscale" {
		t.Fatalf("plugins = %+v, want [jupyterlab tailscale]", idx.Plugins)
	}

	ts := idx.Plugins[1]
	if ts.Version != "v2.1.0" {
		t.Errorf("tailscale version = %q, want v2.1.0", ts.Version)
	}
	// The path must be the layout the resolver actually fetches — plugins/<name>/,
	// not <name>/ at the root. That mismatch was the bug in spawn#448.
	if ts.Path != "plugins/tailscale/plugin.yaml" {
		t.Errorf("tailscale path = %q, want plugins/tailscale/plugin.yaml", ts.Path)
	}
	if !ts.Verified {
		t.Error("tailscale Verified = false, want true for the official registry")
	}
	if !ts.RequiresRoot {
		t.Error("tailscale RequiresRoot = false, want true (spec declares instance.root)")
	}
	if len(ts.OpensPorts) != 1 || ts.OpensPorts[0] != 41641 {
		t.Errorf("tailscale OpensPorts = %v, want [41641]", ts.OpensPorts)
	}
	// Config params sorted by name, with required carried through.
	if len(ts.ConfigParams) != 2 || ts.ConfigParams[0].Name != "auth_key" || !ts.ConfigParams[0].Required {
		t.Errorf("tailscale ConfigParams = %+v, want auth_key (required) first", ts.ConfigParams)
	}
	if ts.ConfigParams[1].Name != "tag" || ts.ConfigParams[1].Required {
		t.Errorf("ConfigParams[1] = %+v, want tag (not required)", ts.ConfigParams[1])
	}

	// A spec with no permissions block leaves the capability fields zero: that
	// means "not declared", and must not be reported as "declares no root".
	jl := idx.Plugins[0]
	if jl.RequiresRoot || len(jl.OpensPorts) > 0 {
		t.Errorf("jupyterlab declared no permissions but got root=%v ports=%v", jl.RequiresRoot, jl.OpensPorts)
	}
}

func TestBuildIndex_Deterministic(t *testing.T) {
	dir := t.TempDir()
	writeSpec(t, dir, "tailscale", idxSpecTailscale)
	writeSpec(t, dir, "jupyterlab", idxSpecJupyter)
	stamp := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	// Two builds of an unchanged registry must be byte-identical, or the CI that
	// commits the index churns a diff on every push. Config params come from a
	// map, so this is a real risk without the explicit sort.
	var first []byte
	for i := 0; i < 5; i++ {
		idx, err := BuildIndex(dir, "spore-host/spore-plugins", true, stamp)
		if err != nil {
			t.Fatalf("BuildIndex: %v", err)
		}
		data, err := idx.Encode()
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		if i == 0 {
			first = data
			continue
		}
		if string(data) != string(first) {
			t.Fatalf("index build %d differs from build 0:\n%s\nvs\n%s", i, data, first)
		}
	}
}

func TestBuildIndex_RejectsNameDirMismatch(t *testing.T) {
	dir := t.TempDir()
	// Spec says "tailscale" but lives in dir "tailscale-old": a bare-name ref
	// resolves via the DIRECTORY, so indexing this would advertise a plugin that
	// install could not fetch.
	writeSpec(t, dir, "tailscale-old", idxSpecTailscale)

	_, err := BuildIndex(dir, "spore-host/spore-plugins", true, time.Now())
	if err == nil {
		t.Fatal("expected an error for a spec name / directory mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "does not match directory") {
		t.Errorf("error = %q, want it to name the mismatch", err)
	}
}

func TestBuildIndex_ErrorsOnEmptyAndUnparseable(t *testing.T) {
	// An empty registry is an error: an index with zero plugins reads as "the
	// registry is empty", which is indistinguishable from a broken generator.
	if _, err := BuildIndex(t.TempDir(), "spore-host/spore-plugins", true, time.Now()); err == nil {
		t.Error("expected an error for a plugins dir with no specs, got nil")
	}

	// A spec that doesn't parse must fail the whole build rather than being
	// silently skipped — a missing entry reads as "that plugin doesn't exist".
	dir := t.TempDir()
	writeSpec(t, dir, "broken", "name: broken\nversion: [not a string\n")
	if _, err := BuildIndex(dir, "spore-host/spore-plugins", true, time.Now()); err == nil {
		t.Error("expected an error for an unparseable spec, got nil")
	}
}

func TestBuildIndex_SkipsDirWithoutSpec(t *testing.T) {
	dir := t.TempDir()
	writeSpec(t, dir, "tailscale", idxSpecTailscale)
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	idx, err := BuildIndex(dir, "spore-host/spore-plugins", true, time.Now())
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	if len(idx.Plugins) != 1 {
		t.Errorf("got %d plugins, want 1 (a dir with no plugin.yaml isn't a plugin)", len(idx.Plugins))
	}
}

func TestParseIndex_RejectsNewerSchema(t *testing.T) {
	data := []byte(`{"schemaVersion": 999, "plugins": []}`)
	_, err := ParseIndex(data)
	if err == nil {
		t.Fatal("expected an error for a future schemaVersion, got nil")
	}
	if !strings.Contains(err.Error(), "upgrade spawn") {
		t.Errorf("error = %q, want it to tell the user to upgrade", err)
	}
}

func TestIndexSearch_NameHitsBeforeDescriptionHits(t *testing.T) {
	idx := &Index{Plugins: []IndexEntry{
		{Name: "code-server", Description: "VS Code in the browser, works alongside jupyter"},
		{Name: "jupyterlab", Description: "JupyterLab notebook server"},
	}}

	hits := idx.Search("jupyter")
	if len(hits) != 2 {
		t.Fatalf("got %d hits, want 2", len(hits))
	}
	// jupyterlab matches on NAME, so it must rank above the description-only hit.
	if hits[0].Name != "jupyterlab" {
		t.Errorf("hits[0] = %q, want jupyterlab (name match ranks first)", hits[0].Name)
	}

	if got := idx.Search(""); len(got) != 2 {
		t.Errorf("empty query returned %d, want all 2", len(got))
	}
	if got := idx.Search("nonexistent"); len(got) != 0 {
		t.Errorf("no-match query returned %d, want 0", len(got))
	}
	// Case-insensitive.
	if got := idx.Search("JUPYTERLAB"); len(got) != 1 {
		t.Errorf("uppercase query returned %d, want 1", len(got))
	}
}

func TestIndexLookup_ExactOnly(t *testing.T) {
	idx := &Index{Plugins: []IndexEntry{{Name: "tailscale", Version: "v2.1.0"}}}
	if e, ok := idx.Lookup("tailscale"); !ok || e.Version != "v2.1.0" {
		t.Errorf("Lookup(tailscale) = %+v, %v", e, ok)
	}
	// Prefix must NOT match: resolution is exact, so a fuzzy hit here would
	// report a plugin that install then 404s on.
	if _, ok := idx.Lookup("tail"); ok {
		t.Error("Lookup(tail) matched tailscale; want exact matching only")
	}
}

// ── fetch + cache ─────────────────────────────────────────────────────────────

const testIndexJSON = `{
  "schemaVersion": 1,
  "generatedAt": "2026-07-29T00:00:00Z",
  "source": "spore-host/spore-plugins",
  "plugins": [{"name": "tailscale", "version": "v2.1.0", "path": "plugins/tailscale/plugin.yaml", "verified": true}]
}`

func TestIndexFetcher_FetchesAndCaches(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		// Pin the FULL path: the index lives at the repo root, at the ref asked for.
		if r.URL.Path != "/spore-host/spore-plugins/main/index.json" {
			t.Errorf("unexpected index path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(testIndexJSON))
	}))
	defer srv.Close()

	cacheDir := t.TempDir()
	f := &IndexFetcher{RawBase: srv.URL, CacheDir: cacheDir, Now: time.Now}

	res, err := f.Fetch(context.Background(), OfficialIndexSource(), false)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.FromCache {
		t.Error("first fetch reported FromCache = true")
	}
	if len(res.Index.Plugins) != 1 {
		t.Fatalf("got %d plugins, want 1", len(res.Index.Plugins))
	}

	// Second fetch is served from cache: no additional network hit.
	res2, err := f.Fetch(context.Background(), OfficialIndexSource(), false)
	if err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	if !res2.FromCache {
		t.Error("second fetch did not use the cache")
	}
	if hits != 1 {
		t.Errorf("server hit %d times, want 1 (second read should be cached)", hits)
	}

	// --refresh bypasses the cache.
	if _, err := f.Fetch(context.Background(), OfficialIndexSource(), true); err != nil {
		t.Fatalf("refresh Fetch: %v", err)
	}
	if hits != 2 {
		t.Errorf("server hit %d times after --refresh, want 2", hits)
	}
}

func TestIndexFetcher_ServesStaleCacheWhenOffline(t *testing.T) {
	cacheDir := t.TempDir()

	// Populate the cache from a working server.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(testIndexJSON))
	}))
	f := &IndexFetcher{RawBase: srv.URL, CacheDir: cacheDir, Now: time.Now}
	if _, err := f.Fetch(context.Background(), OfficialIndexSource(), false); err != nil {
		t.Fatalf("seed Fetch: %v", err)
	}
	srv.Close() // now "offline"

	// Force past the TTL so the cache isn't served on the happy path, and make
	// the network fail. Discovery must still answer, flagging staleness.
	f.Now = func() time.Time { return time.Now().Add(indexCacheTTL + time.Hour) }
	res, err := f.Fetch(context.Background(), OfficialIndexSource(), false)
	if err != nil {
		t.Fatalf("offline Fetch should serve stale cache, got error: %v", err)
	}
	if !res.FromCache {
		t.Error("offline fetch did not report FromCache")
	}
	if res.StaleErr == nil {
		t.Error("offline fetch served a cached index with StaleErr = nil; the caller could not tell it was stale")
	}
	if res.CachedAt.IsZero() {
		t.Error("CachedAt is zero; the caller can't report the cache's age")
	}
}

func TestIndexFetcher_ErrorsWhenNoIndexAndNoCache(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	f := &IndexFetcher{RawBase: srv.URL, CacheDir: t.TempDir(), Now: time.Now}
	_, err := f.Fetch(context.Background(), OfficialIndexSource(), false)
	if err == nil {
		t.Fatal("expected an error when the registry publishes no index and nothing is cached")
	}
	// The message must distinguish "no index published" from "plugin not found".
	if !strings.Contains(err.Error(), "no plugin index published") {
		t.Errorf("error = %q, want it to say no index is published", err)
	}
}

func TestIndexFetcher_RejectsInvalidSource(t *testing.T) {
	f := &IndexFetcher{RawBase: "http://127.0.0.1:1", CacheDir: t.TempDir()}
	for _, src := range []IndexSource{
		{Owner: "../etc", Repo: "spore-plugins"},
		{Owner: "spore-host", Repo: "a/b"},
		{Owner: "spore-host", Repo: "spore-plugins", Ref: "main;rm -rf /"},
	} {
		if _, err := f.Fetch(context.Background(), src, false); err == nil {
			t.Errorf("Fetch(%+v) succeeded; want a validation error", src)
		}
	}
}
