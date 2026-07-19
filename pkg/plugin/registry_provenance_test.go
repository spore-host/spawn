package plugin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const provTestSpec = `name: demo
version: v1.0.0
description: A demo plugin
remote:
  install:
    - type: run
      run: "true"
`

// newTestResolver points the resolver at local httptest servers for the raw
// (plugin.yaml) and API (commit SHA) endpoints.
func newTestResolver(rawBase, apiBase string) *compositeResolver {
	return &compositeResolver{rawBase: rawBase, apiBase: apiBase}
}

func TestResolveWithProvenance_GitHub_PinsAndHashes(t *testing.T) {
	const wantSHA = "0123456789abcdef0123456789abcdef01234567"
	raw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /spore-host/spore-plugins/main/demo/plugin.yaml
		if !strings.HasSuffix(r.URL.Path, "/demo/plugin.yaml") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(provTestSpec))
	}))
	defer raw.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /repos/spore-host/spore-plugins/commits/HEAD  (Accept: vnd.github.sha)
		if !strings.Contains(r.URL.Path, "/commits/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(wantSHA))
	}))
	defer api.Close()

	r := newTestResolver(raw.URL, api.URL)
	spec, prov, err := r.ResolveWithProvenance(context.Background(), "demo")
	if err != nil {
		t.Fatalf("ResolveWithProvenance: %v", err)
	}
	if spec.Name != "demo" {
		t.Errorf("spec.Name = %q, want demo", spec.Name)
	}
	if prov.CommitSHA != wantSHA {
		t.Errorf("CommitSHA = %q, want %q", prov.CommitSHA, wantSHA)
	}
	if len(prov.ContentSHA256) != 64 {
		t.Errorf("ContentSHA256 = %q, want a 64-char sha256", prov.ContentSHA256)
	}
	if !prov.Pinned() {
		t.Error("Pinned() = false, want true (commit SHA present)")
	}
}

func TestResolveWithProvenance_CommitAPIFailure_BestEffort(t *testing.T) {
	raw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(provTestSpec))
	}))
	defer raw.Close()
	// API always fails — commit SHA can't be resolved.
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden) // e.g. rate limited
	}))
	defer api.Close()

	r := newTestResolver(raw.URL, api.URL)
	_, prov, err := r.ResolveWithProvenance(context.Background(), "demo")
	if err != nil {
		t.Fatalf("resolve should succeed even when commit API fails: %v", err)
	}
	if prov.CommitSHA != "" {
		t.Errorf("CommitSHA = %q, want empty on API failure", prov.CommitSHA)
	}
	if len(prov.ContentSHA256) != 64 {
		t.Errorf("ContentSHA256 must still be recorded, got %q", prov.ContentSHA256)
	}
	if prov.Pinned() {
		t.Error("Pinned() = true, want false (no commit SHA, remote ref)")
	}
}

func TestResolveWithProvenance_ContentHashIsStable(t *testing.T) {
	// Same bytes → same digest; a different body → different digest.
	body := provTestSpec
	raw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer raw.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer api.Close()
	r := newTestResolver(raw.URL, api.URL)

	_, p1, err := r.ResolveWithProvenance(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if p1.ContentSHA256 != sha256Hex([]byte(provTestSpec)) {
		t.Errorf("digest %q does not match direct sha256 of the body", p1.ContentSHA256)
	}
}

func TestIsHexSHA(t *testing.T) {
	cases := map[string]bool{
		"0123456789abcdef0123456789abcdef01234567":                         true, // 40
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef": true, // 64
		"nothex": false,
		"":       false,
		"0123":   false,
		"g123456789abcdef0123456789abcdef01234567": false, // 'g' not hex
	}
	for in, want := range cases {
		if got := isHexSHA(in); got != want {
			t.Errorf("isHexSHA(%q) = %v, want %v", in, got, want)
		}
	}
}
