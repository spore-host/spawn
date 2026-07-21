package plugin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const manifestTestSpec = `name: demo
version: v1.0.0
description: A demo plugin
remote:
  install:
    - type: run
      run: "true"
`

func TestBuildAndParseManifest_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	pluginDir := filepath.Join(dir, "demo")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.yaml"), []byte(manifestTestSpec), 0o644); err != nil {
		t.Fatal(err)
	}

	m, err := BuildManifest(pluginDir)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if m.Plugin != "demo" || m.Version != "v1.0.0" {
		t.Errorf("manifest = %+v, want plugin=demo version=v1.0.0", m)
	}
	if m.PluginYAMLSHA256() != sha256Hex([]byte(manifestTestSpec)) {
		t.Errorf("manifest digest %q != direct sha256 of the spec", m.PluginYAMLSHA256())
	}

	data, err := m.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := ParseManifest(data)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if got.PluginYAMLSHA256() != m.PluginYAMLSHA256() {
		t.Errorf("round-trip digest mismatch: %q != %q", got.PluginYAMLSHA256(), m.PluginYAMLSHA256())
	}
}

func TestParseManifest_Rejects(t *testing.T) {
	cases := map[string]string{
		"bad schema version": `{"schema_version":99,"plugin":"demo","version":"v1.0.0","files":{"plugin.yaml":"` + strings.Repeat("a", 64) + `"}}`,
		"no plugin.yaml":     `{"schema_version":1,"plugin":"demo","version":"v1.0.0","files":{}}`,
		"non-hex digest":     `{"schema_version":1,"plugin":"demo","version":"v1.0.0","files":{"plugin.yaml":"nothex"}}`,
		"not json":           `{not json`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseManifest([]byte(body)); err == nil {
				t.Errorf("expected error for %s, got nil", name)
			}
		})
	}
}

// manifestResolver wires the raw (plugin.yaml), api (commit), and release
// (manifest.json) endpoints to httptest servers.
func manifestResolver(rawBase, apiBase, releaseBase string) *compositeResolver {
	return &compositeResolver{rawBase: rawBase, apiBase: apiBase, releaseBase: releaseBase}
}

// manifestJSON builds a manifest.json body for the given plugin.yaml sha256.
func manifestJSON(t *testing.T, sha string) []byte {
	t.Helper()
	m := &Manifest{SchemaVersion: ManifestSchemaVersion, Plugin: "demo", Version: "v1.0.0", Files: map[string]string{"plugin.yaml": sha}}
	data, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// officialServers wires raw/api/release httptest servers where the plugin.yaml
// and manifest.json are served for demo@v1.0.0 but NO signature asset exists
// (unsigned release). Returns the three servers; caller closes them.
func officialServersUnsigned(t *testing.T) (raw, api, release *httptest.Server) {
	t.Helper()
	specSHA := sha256Hex([]byte(manifestTestSpec))
	raw = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/plugins/demo/plugin.yaml") || !strings.Contains(r.URL.Path, "/demo-v1.0.0/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(manifestTestSpec)) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter -- test fixture
	}))
	api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound) // commit resolution is best-effort
	}))
	release = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Serve the manifest, but 404 the signature asset (unsigned release).
		if strings.HasSuffix(r.URL.Path, "/demo-v1.0.0/manifest.json") {
			_, _ = w.Write(manifestJSON(t, specSHA)) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter -- test fixture
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	return raw, api, release
}

// TestResolveOfficialVersioned_UnsignedIsHardFail: signatures are mandatory, so an
// official versioned ref whose release has a valid manifest but NO signature asset
// is rejected.
func TestResolveOfficialVersioned_UnsignedIsHardFail(t *testing.T) {
	raw, api, release := officialServersUnsigned(t)
	defer raw.Close()
	defer api.Close()
	defer release.Close()

	r := manifestResolver(raw.URL, api.URL, release.URL)
	_, _, err := r.ResolveWithProvenance(context.Background(), "demo@v1.0.0")
	if err == nil {
		t.Fatal("expected a hard failure for an unsigned official release, got nil")
	}
	if !strings.Contains(err.Error(), "is not signed") {
		t.Errorf("error %q does not explain the missing signature", err)
	}
}

// TestResolveOfficialVersioned_InsecureBypassesUnsigned: --insecure downgrades the
// missing-signature hard fail to a warning; resolution succeeds and the manifest
// still verifies (ManifestVerified set, SignatureVerified not).
func TestResolveOfficialVersioned_InsecureBypassesUnsigned(t *testing.T) {
	raw, api, release := officialServersUnsigned(t)
	defer raw.Close()
	defer api.Close()
	defer release.Close()

	r := manifestResolver(raw.URL, api.URL, release.URL)
	r.insecure = true
	_, prov, err := r.ResolveWithProvenance(context.Background(), "demo@v1.0.0")
	if err != nil {
		t.Fatalf("insecure resolve should not fail on a missing signature: %v", err)
	}
	if !prov.ManifestVerified {
		t.Error("ManifestVerified = false, want true (manifest still checked under --insecure)")
	}
	if prov.SignatureVerified {
		t.Error("SignatureVerified = true, want false (no signature served)")
	}
	if prov.ReleaseTag != "demo-v1.0.0" {
		t.Errorf("ReleaseTag = %q, want demo-v1.0.0", prov.ReleaseTag)
	}
}

func TestResolveOfficialVersioned_InsecureDowngradesManifestMismatch(t *testing.T) {
	raw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(manifestTestSpec)) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter -- test fixture
	}))
	defer raw.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) }))
	defer api.Close()
	release := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(manifestJSON(t, strings.Repeat("b", 64))) // wrong digest // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter -- test fixture
	}))
	defer release.Close()

	// With insecure, a manifest mismatch downgrades to a warning and resolution
	// still succeeds (but ManifestVerified stays false — nothing was verified).
	r := manifestResolver(raw.URL, api.URL, release.URL)
	r.insecure = true
	_, prov, err := r.ResolveWithProvenance(context.Background(), "demo@v1.0.0")
	if err != nil {
		t.Fatalf("insecure resolve should not fail on mismatch: %v", err)
	}
	if prov.ManifestVerified {
		t.Error("ManifestVerified = true, want false (mismatch was only warned about)")
	}
}

func TestResolveOfficialVersioned_ManifestMismatch(t *testing.T) {
	raw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(manifestTestSpec)) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter -- test fixture
	}))
	defer raw.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) }))
	defer api.Close()
	release := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Manifest records a DIFFERENT digest than the served spec.
		_, _ = w.Write(manifestJSON(t, strings.Repeat("b", 64))) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter -- test fixture
	}))
	defer release.Close()

	r := manifestResolver(raw.URL, api.URL, release.URL)
	_, _, err := r.ResolveWithProvenance(context.Background(), "demo@v1.0.0")
	if err == nil {
		t.Fatal("expected a checksum mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("error %q does not mention checksum mismatch", err)
	}
}

func TestResolveOfficialVersioned_ManifestMissing(t *testing.T) {
	raw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(manifestTestSpec)) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter -- test fixture
	}))
	defer raw.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) }))
	defer api.Close()
	release := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound) // no manifest asset on the release
	}))
	defer release.Close()

	r := manifestResolver(raw.URL, api.URL, release.URL)
	_, _, err := r.ResolveWithProvenance(context.Background(), "demo@v1.0.0")
	if err == nil {
		t.Fatal("expected a missing-manifest error, got nil")
	}
	if !strings.Contains(err.Error(), "no checksum manifest") {
		t.Errorf("error %q does not mention missing manifest", err)
	}
}

func TestResolveOfficialBare_NoManifestFetch(t *testing.T) {
	// A bare official ref (no version) has no release/manifest; resolution must
	// succeed and must NOT mark ManifestVerified. The release server fails the
	// test if it's ever contacted.
	raw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(manifestTestSpec)) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter -- test fixture
	}))
	defer raw.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) }))
	defer api.Close()
	release := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("release endpoint should not be contacted for a bare official ref")
		w.WriteHeader(http.StatusNotFound)
	}))
	defer release.Close()

	r := manifestResolver(raw.URL, api.URL, release.URL)
	_, prov, err := r.ResolveWithProvenance(context.Background(), "demo")
	if err != nil {
		t.Fatalf("ResolveWithProvenance: %v", err)
	}
	if prov.ManifestVerified {
		t.Error("ManifestVerified = true for a bare ref, want false")
	}
}
