package plugin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Version selection for a bare official name (#816, GHSA-54x4-pc23-4m6j).
//
// This is the input that decides whether verification happens at all, so picking
// the wrong tag is not a cosmetic bug: resolving to a tag that does not exist
// fails the install, and resolving to a neighbouring plugin's tag would verify
// the wrong artifact against the wrong manifest.

func TestParseOfficialTagIsolatesThePluginName(t *testing.T) {
	cases := []struct {
		tag, name   string
		wantVersion string
		wantOK      bool
	}{
		{"demo-1.2.3", "demo", "1.2.3", true},
		{"demo-v1.2.3", "demo", "v1.2.3", true},
		{"demo-1.2.3-rc1", "demo", "1.2.3-rc1", true},
		{"demo-1.2.3+build5", "demo", "1.2.3+build5", true},

		// The cases that matter. A hyphenated neighbour must not be read as a
		// version of the shorter name, or `spawn plugin install tailscale` could
		// resolve to a tailscale-exit-node release.
		{"demo-extra-1.2.3", "demo", "", false},
		{"other-1.2.3", "demo", "", false},
		{"demo", "demo", "", false},
		{"demo-", "demo", "", false},
		{"demo-latest", "demo", "", false},
		{"demo-1.2", "demo", "", false},
		{"demo-1.2.3.4", "demo", "", false},
		{"prefix-demo-1.2.3", "demo", "", false},
	}

	for _, c := range cases {
		gotVersion, _, gotOK := parseOfficialTag(c.tag, c.name)
		if gotOK != c.wantOK {
			t.Errorf("parseOfficialTag(%q, %q) ok = %v, want %v", c.tag, c.name, gotOK, c.wantOK)
			continue
		}
		if gotVersion != c.wantVersion {
			t.Errorf("parseOfficialTag(%q, %q) version = %q, want %q",
				c.tag, c.name, gotVersion, c.wantVersion)
		}
	}
}

// Ordering must be semantic, not lexical.
//
// "1.10.0" sorts BEFORE "1.9.0" as a string, so a lexical max would install an
// older plugin than the one published — and silently, since the install would
// succeed and verify fine.
func TestLatestOfficialVersionOrdersSemantically(t *testing.T) {
	cases := []struct {
		name string
		tags []string
		want string
	}{
		{"double-digit minor beats single", []string{"demo-1.9.0", "demo-1.10.0"}, "1.10.0"},
		{"double-digit patch beats single", []string{"demo-1.0.9", "demo-1.0.10"}, "1.0.10"},
		{"major dominates", []string{"demo-1.99.99", "demo-2.0.0"}, "2.0.0"},
		{"release beats its own prerelease", []string{"demo-2.0.0", "demo-2.0.0-rc1"}, "2.0.0"},
		{"unsorted input", []string{"demo-1.2.0", "demo-0.1.0", "demo-1.1.0"}, "1.2.0"},
		{"v-prefixed", []string{"demo-v1.0.0", "demo-v1.3.0"}, "v1.3.0"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &compositeResolver{apiBase: releasesStub(t, c.tags...)}
			got, err := r.latestOfficialVersion(context.Background(), "spore-host", "spore-plugins", "demo")
			if err != nil {
				t.Fatalf("latestOfficialVersion: %v", err)
			}
			if got != c.want {
				t.Errorf("latest = %q, want %q", got, c.want)
			}
		})
	}
}

// Drafts and prereleases must not be selected.
//
// A draft tag may not exist as a git ref at all, so selecting one turns a working
// install into a 404 — and a prerelease is not what `install <name>` should mean.
func TestLatestOfficialVersionSkipsDraftsAndPrereleases(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !strings.Contains(req.URL.Path, "/releases") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`[
			{"tag_name":"demo-1.0.0"},
			{"tag_name":"demo-5.0.0","draft":true},
			{"tag_name":"demo-4.0.0","prerelease":true}
		]`)) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter -- test fixture
	}))
	defer api.Close()

	r := &compositeResolver{apiBase: api.URL}
	got, err := r.latestOfficialVersion(context.Background(), "spore-host", "spore-plugins", "demo")
	if err != nil {
		t.Fatalf("latestOfficialVersion: %v", err)
	}
	if got != "1.0.0" {
		t.Errorf("latest = %q, want 1.0.0 — a draft or prerelease was selected", got)
	}
}

// A plugin with no releases reports "" and no error; the CALLER decides what that
// means. Conflating it with an error would make "nothing published" and "could
// not ask" indistinguishable, and only one of those is safe information.
func TestLatestOfficialVersionReportsNoReleasesWithoutAnError(t *testing.T) {
	r := &compositeResolver{apiBase: releasesStub(t, "other-1.0.0", "unrelated-2.0.0")}
	got, err := r.latestOfficialVersion(context.Background(), "spore-host", "spore-plugins", "demo")
	if err != nil {
		t.Fatalf("an absent plugin should not be an error: %v", err)
	}
	if got != "" {
		t.Errorf("latest = %q, want \"\"", got)
	}
}

// An API failure must be an error, never an empty result.
//
// If a rate limit returned ("", nil) the caller would read it as "no releases"
// and offer the unverified default branch — a downgrade an attacker could induce
// just by exhausting the quota.
func TestLatestOfficialVersionReportsAPIFailuresAsErrors(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusNotFound} {
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))
		r := &compositeResolver{apiBase: api.URL}
		_, err := r.latestOfficialVersion(context.Background(), "spore-host", "spore-plugins", "demo")
		api.Close()
		if err == nil {
			t.Errorf("HTTP %d was reported as success; a failed lookup must not read as "+
				"\"no releases published\"", status)
		}
	}
}

// Malformed JSON is an error too, for the same reason.
func TestLatestOfficialVersionReportsMalformedJSONAsAnError(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{not json`)) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter -- test fixture
	}))
	defer api.Close()

	r := &compositeResolver{apiBase: api.URL}
	if _, err := r.latestOfficialVersion(context.Background(), "spore-host", "spore-plugins", "demo"); err == nil {
		t.Error("malformed releases JSON was reported as success")
	}
}

// Pagination must be followed, and bounded.
//
// A full first page with the newest release on page two would otherwise install a
// stale version. The repo is small today, which is exactly why this is worth
// asserting rather than assuming.
func TestLatestOfficialVersionFollowsPagination(t *testing.T) {
	pagesServed := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !strings.Contains(req.URL.Path, "/releases") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		pagesServed++
		if req.URL.Query().Get("page") == "1" {
			// A FULL page, so the reader must ask for another.
			var b strings.Builder
			b.WriteString("[")
			for i := 0; i < 100; i++ {
				if i > 0 {
					b.WriteString(",")
				}
				b.WriteString(`{"tag_name":"filler-0.0.1"}`)
			}
			b.WriteString("]")
			_, _ = w.Write([]byte(b.String())) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter -- test fixture
			return
		}
		_, _ = w.Write([]byte(`[{"tag_name":"demo-3.0.0"}]`)) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter -- test fixture
	}))
	defer api.Close()

	r := &compositeResolver{apiBase: api.URL}
	got, err := r.latestOfficialVersion(context.Background(), "spore-host", "spore-plugins", "demo")
	if err != nil {
		t.Fatalf("latestOfficialVersion: %v", err)
	}
	if got != "3.0.0" {
		t.Errorf("latest = %q, want 3.0.0 — the second page was not read", got)
	}
	if pagesServed < 2 {
		t.Errorf("served %d page(s); a full first page must be followed", pagesServed)
	}
	if pagesServed > releaseTagsPageLimit {
		t.Errorf("served %d pages, above the %d-page bound", pagesServed, releaseTagsPageLimit)
	}
}

// releasesStub serves the given tags as non-draft, non-prerelease releases.
func releasesStub(t *testing.T, tags ...string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("[")
	for i, tag := range tags {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"tag_name":"` + tag + `"}`)
	}
	b.WriteString("]")
	body := b.String()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !strings.Contains(req.URL.Path, "/releases") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body)) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter -- test fixture
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}
