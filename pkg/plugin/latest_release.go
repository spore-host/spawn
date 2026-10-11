package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Bare-name resolution for official plugins (#816, GHSA-54x4-pc23-4m6j).
//
// `spawn plugin install <name>` — the shortest and most natural form — used to
// resolve to the registry's `main` branch. The verification machinery is well
// built and genuinely fail-closed, but it is gated on `prov.ReleaseTag != ""`,
// and a bare name left that empty. So the one install form a person is most
// likely to type performed no signature check, no checksum check, and emitted no
// warning — while a plugin spec runs as root on the instance.
//
// The gate was not wrong when it was written: bare-name resolution 404'd against
// the official registry layout, so the path was unreachable (#448). That bug was
// fixed, and fixing it silently armed this one. Measured today,
// plugins/tailscale/plugin.yaml on main returns HTTP 200.
//
// The fix is to make the verified path the DEFAULT rather than the opt-in: a bare
// official name resolves to the plugin's latest release tag, which is exactly the
// input the verified path already takes. Nothing about verification changes — it
// simply now runs.

// officialTagRe captures the version from a per-plugin release tag
// `<name>-<version>`. Anchored, and the name is quoted by the caller, so a tag
// for `tailscale-exit-node` cannot be read as a version of `tailscale`.
var officialTagRe = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)(?:[-+]([0-9A-Za-z.-]+))?$`)

// releaseTagsPageLimit bounds how many pages of releases are fetched. The
// registry holds a handful of plugins at a few versions each; this exists so a
// pathological repo cannot turn one install into an unbounded walk.
const releaseTagsPageLimit = 5

type githubRelease struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

// semverKey is an ordered representation of a release version. Pre-release
// versions sort below their release, which is what SemVer requires and what stops
// `1.2.0-rc1` being picked over `1.2.0`.
type semverKey struct {
	major, minor, patch int
	pre                 string
}

func (a semverKey) less(b semverKey) bool {
	if a.major != b.major {
		return a.major < b.major
	}
	if a.minor != b.minor {
		return a.minor < b.minor
	}
	if a.patch != b.patch {
		return a.patch < b.patch
	}
	// A pre-release precedes the corresponding release.
	if (a.pre == "") != (b.pre == "") {
		return a.pre != ""
	}
	return a.pre < b.pre
}

// parseOfficialTag returns the version carried by tag for the named plugin, and
// whether the tag belongs to that plugin at all.
func parseOfficialTag(tag, name string) (string, semverKey, bool) {
	prefix := name + "-"
	if !strings.HasPrefix(tag, prefix) {
		return "", semverKey{}, false
	}
	version := strings.TrimPrefix(tag, prefix)
	// Tolerate a leading v on the version segment, as validate.go's semverRe does.
	bare := strings.TrimPrefix(version, "v")
	m := officialTagRe.FindStringSubmatch(bare)
	if m == nil {
		return "", semverKey{}, false
	}
	maj, _ := strconv.Atoi(m[1])
	min, _ := strconv.Atoi(m[2])
	pat, _ := strconv.Atoi(m[3])
	return version, semverKey{major: maj, minor: min, patch: pat, pre: m[4]}, true
}

// latestOfficialVersion returns the highest non-draft, non-prerelease version
// published for name in owner/repo, or "" when the plugin has no releases.
//
// A transport or API failure is returned as an error rather than as "no
// releases": the caller turns an empty result into "install unverified or
// refuse", and a rate-limited API must not be able to choose the unverified
// branch.
func (r *compositeResolver) latestOfficialVersion(ctx context.Context, owner, repo, name string) (string, error) {
	type candidate struct {
		version string
		key     semverKey
	}
	var found []candidate

	for page := 1; page <= releaseTagsPageLimit; page++ {
		url := fmt.Sprintf("%s/repos/%s/%s/releases?per_page=100&page=%d", r.apiBase, owner, repo, page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return "", fmt.Errorf("create releases request: %w", err)
		}
		req.Header.Set("Accept", "application/vnd.github+json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", fmt.Errorf("list releases for %s/%s: %w", owner, repo, err)
		}
		var releases []githubRelease
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			return "", fmt.Errorf("list releases for %s/%s: HTTP %d", owner, repo, resp.StatusCode)
		}
		decErr := json.NewDecoder(resp.Body).Decode(&releases)
		_ = resp.Body.Close()
		if decErr != nil {
			return "", fmt.Errorf("decode releases for %s/%s: %w", owner, repo, decErr)
		}

		for _, rel := range releases {
			if rel.Draft || rel.Prerelease {
				continue
			}
			if version, key, ok := parseOfficialTag(rel.TagName, name); ok {
				found = append(found, candidate{version: version, key: key})
			}
		}

		// A short page is the last page. Checking the page length rather than
		// trusting a single request is the lesson from every other paginated read
		// in this repo: an empty FIRST page with more to come is a real shape.
		if len(releases) < 100 {
			break
		}
	}

	if len(found) == 0 {
		return "", nil
	}
	sort.Slice(found, func(i, j int) bool { return found[i].key.less(found[j].key) })
	return found[len(found)-1].version, nil
}
