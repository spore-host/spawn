package plugin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The layout a ref resolves to is load-bearing and asymmetric: the official
// registry keeps specs under plugins/<name>/plugin.yaml, while a third-party
// github: repo is expected to keep <name>/plugin.yaml at its root. Nothing pinned
// the resolved URL before — the provenance tests matched on a suffix that both
// layouts satisfy — so a regression in either path would have gone unnoticed
// until an install 404'd (spawn#448). These tests record the exact path
// requested, for each host, so a layout change has to be deliberate.

// recordingRawServer serves provTestSpec for any path and records what was asked
// for.
func recordingRawServer(t *testing.T, got *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got = r.URL.Path
		// nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter -- test-only httptest server returning a static plugin-spec fixture (provTestSpec); no user input, no HTML, no XSS surface.
		_, _ = w.Write([]byte(provTestSpec))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchGitHubSpec_ResolvedURLPath(t *testing.T) {
	tests := []struct {
		name     string
		ref      string
		wantPath string
	}{
		{
			// A bare name is the official registry: specs live under plugins/.
			// Getting this wrong is exactly the bug reported in spawn#448.
			name:     "official bare name",
			ref:      "demo",
			wantPath: "/spore-host/spore-plugins/main/plugins/demo/plugin.yaml",
		},
		{
			// A third-party repo keeps the spec at its root — no plugins/ prefix.
			name:     "third-party github ref",
			ref:      "github:someone/their-plugins/demo",
			wantPath: "/someone/their-plugins/main/demo/plugin.yaml",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath string
			raw := recordingRawServer(t, &gotPath)
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNotFound) // commit SHA is best-effort here
			}))
			defer api.Close()

			r := newTestResolver(raw.URL, api.URL)
			if _, err := r.Resolve(context.Background(), tc.ref); err != nil {
				t.Fatalf("Resolve(%q): %v", tc.ref, err)
			}
			if gotPath != tc.wantPath {
				t.Errorf("Resolve(%q) fetched %q, want %q", tc.ref, gotPath, tc.wantPath)
			}
		})
	}
}

func TestFetchGitHubSpec_VersionedRefFetchesAtTag(t *testing.T) {
	// An official versioned ref is fetched AT its release tag (<name>-<version>),
	// not at main: the whole point is that the bytes match the signed release.
	var gotPath string
	raw := recordingRawServer(t, &gotPath)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer api.Close()

	// insecure: the manifest/signature servers aren't stood up here — this test
	// is about the URL, and verification has its own tests.
	r := &compositeResolver{rawBase: raw.URL, apiBase: api.URL, insecure: true}
	if _, err := r.Resolve(context.Background(), "demo@v1.0.0"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	const want = "/spore-host/spore-plugins/demo-v1.0.0/plugins/demo/plugin.yaml"
	if gotPath != want {
		t.Errorf("fetched %q, want %q", gotPath, want)
	}
}

func TestFetchGitHubSpec_RejectsTraversalComponents(t *testing.T) {
	// The URL is assembled by string formatting, so a component containing a path
	// separator or ".." would escape the repo it names. These must be rejected
	// before any request is made.
	var gotPath string
	raw := recordingRawServer(t, &gotPath)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer api.Close()

	for _, ref := range []string{
		"github:someone/their-plugins/../../etc/passwd",
		"github:../evil/their-plugins/demo",
		"github:someone/../evil/demo",
	} {
		gotPath = ""
		if _, err := newTestResolver(raw.URL, api.URL).Resolve(context.Background(), ref); err == nil {
			t.Errorf("Resolve(%q) succeeded; want a validation error", ref)
		}
		if gotPath != "" {
			t.Errorf("Resolve(%q) issued a request for %q; validation must happen before the fetch", ref, gotPath)
		}
	}
}
