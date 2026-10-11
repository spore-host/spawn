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

// manifestJSONVersion builds a manifest.json body at an explicit version, for
// tests that resolve to a tag other than v1.0.0.
func manifestJSONVersion(t *testing.T, sha, version string) []byte {
	t.Helper()
	m := &Manifest{SchemaVersion: ManifestSchemaVersion, Plugin: "demo", Version: version, Files: map[string]string{"plugin.yaml": sha}}
	data, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return data
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

func TestResolveOfficialBare_ResolvesToLatestReleaseAndVerifies(t *testing.T) {
	// This test used to be TestResolveOfficialBare_NoManifestFetch, and it asserted
	// the opposite: that a bare official ref resolves successfully, does NOT set
	// ManifestVerified, and must never contact the release endpoint ("release
	// endpoint should not be contacted for a bare official ref").
	//
	// That was an accurate description of the code and a false description of the
	// design. A plugin spec runs as root on the instance, so the install form with
	// no verification cannot be the default one — and because the test was written
	// from the code, it certified the gap and would have failed on the fix
	// (CLAUDE.md rule 5, #816, GHSA-54x4-pc23-4m6j).
	//
	// The intent, stated the other way round: a bare name resolves to the plugin's
	// latest release and goes through the SAME verification a versioned ref does.
	specSHA := sha256Hex([]byte(manifestTestSpec))

	raw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Must be fetched AT the resolved tag, not at main.
		if !strings.Contains(r.URL.Path, "/demo-1.2.0/") {
			t.Errorf("plugin.yaml fetched from %q, want the demo-1.2.0 tag", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(manifestTestSpec)) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter -- test fixture
	}))
	defer raw.Close()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/releases") {
			// Deliberately unsorted, with a draft, a prerelease, a newer version of
			// a DIFFERENT plugin, and a same-prefix neighbour: 1.2.0 must win.
			_, _ = w.Write([]byte(`[
				{"tag_name":"demo-1.0.0"},
				{"tag_name":"demo-9.9.9","draft":true},
				{"tag_name":"demo-2.0.0-rc1","prerelease":true},
				{"tag_name":"other-5.0.0"},
				{"tag_name":"demo-extra-7.0.0"},
				{"tag_name":"demo-1.2.0"},
				{"tag_name":"demo-1.1.9"}
			]`)) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter -- test fixture
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer api.Close()

	contacted := false
	release := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contacted = true
		switch {
		case strings.HasSuffix(r.URL.Path, ManifestFileName):
			_, _ = w.Write(manifestJSONVersion(t, specSHA, "v1.2.0")) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter -- test fixture
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer release.Close()

	r := manifestResolver(raw.URL, api.URL, release.URL)
	_, prov, err := r.ResolveWithProvenance(context.Background(), "demo")

	if !contacted {
		t.Error("the release endpoint was never contacted for a bare official ref — verification " +
			"is still being skipped, which is the whole defect")
	}
	if prov != nil && prov.ReleaseTag != "demo-1.2.0" {
		t.Errorf("ReleaseTag = %q, want demo-1.2.0 (the highest non-draft, non-prerelease release)",
			prov.ReleaseTag)
	}
	// The signature asset is absent from this stub, so verification must HARD FAIL
	// rather than quietly resolve — that is the fail-closed behaviour a versioned
	// ref already had and a bare name did not.
	if err == nil {
		t.Fatal("resolution succeeded with no signature on the release; a bare name must be held to " +
			"the same standard as a versioned one")
	}
	if !strings.Contains(err.Error(), "not signed") {
		t.Errorf("error %q does not report the missing signature", err)
	}
}

// A bare official name with NO releases must refuse rather than silently install
// the unsigned spec from the default branch.
func TestResolveOfficialBare_NoReleasesRefuses(t *testing.T) {
	raw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(manifestTestSpec)) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter -- test fixture
	}))
	defer raw.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/releases") {
			_, _ = w.Write([]byte(`[{"tag_name":"other-1.0.0"}]`)) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter -- test fixture
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer api.Close()

	r := manifestResolver(raw.URL, api.URL, "")
	if _, _, err := r.ResolveWithProvenance(context.Background(), "demo"); err == nil {
		t.Fatal("a plugin with no published releases resolved successfully; installing an unsigned " +
			"spec from a mutable branch must be an explicit --insecure choice")
	} else if !strings.Contains(err.Error(), "no published releases") {
		t.Errorf("error %q does not explain that there is nothing to verify against", err)
	}
}

// A releases-API failure must refuse too, not fall back to the default branch.
//
// Separate from the no-releases case: "the API said there are none" and "we could
// not ask" are different, and only one of them is safe to treat as information.
func TestResolveOfficialBare_ReleasesAPIFailureRefuses(t *testing.T) {
	raw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(manifestTestSpec)) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter -- test fixture
	}))
	defer raw.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden) // rate limited
	}))
	defer api.Close()

	r := manifestResolver(raw.URL, api.URL, "")
	if _, _, err := r.ResolveWithProvenance(context.Background(), "demo"); err == nil {
		t.Fatal("a rate-limited releases API resolved successfully from the default branch; an API " +
			"failure must not be able to choose the unverified path")
	} else if !strings.Contains(err.Error(), "could not determine the latest release") {
		t.Errorf("error %q does not explain why it refused", err)
	}
}
