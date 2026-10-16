package cmd

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// sporedUnit is one systemd unit definition for spored found in the repo: either
// a standalone spored.service file or the body of a heredoc that writes
// /etc/systemd/system/spored.service.
type sporedUnit struct {
	where     string // file:line of the unit's first line
	startLine int
	lines     []string
}

// sporedUnitWrite matches a shell line that writes the spored unit through a
// heredoc, e.g. `cat > /etc/systemd/system/spored.service <<'EOF'` or
// `tee /etc/systemd/system/spored.service <<EOF`. Group 1 is the delimiter.
var sporedUnitWrite = regexp.MustCompile(`^\s*(?:sudo\s+)?(?:cat\s*>|tee(?:\s+-a)?)\s*/etc/systemd/system/spored\.service\s*<<-?\s*['"]?(\w+)['"]?\s*$`)

// sporedUnitPath matches any mention of the unit's install path that is a
// redirection or tee target — an emitter, whatever shape it takes. Every such line
// must also match sporedUnitWrite, so an emitter the parser cannot read fails the
// gate instead of slipping past it.
var sporedUnitPath = regexp.MustCompile(`(?:>|tee(?:\s+-a)?|cp\s+\S+|install\s+.*)\s*/etc/systemd/system/spored\.service`)

// discoverSporedUnits walks the spawn module and returns every spored systemd
// unit it defines. It does not use a list of files (CLAUDE.md gate rule 4): a new
// emitter is covered as soon as it exists.
func discoverSporedUnits(t *testing.T, repoRoot string) []sporedUnit {
	t.Helper()
	var units []sporedUnit
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "dist", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		// Tests mention the path to check it; CHANGELOG/docs describe it.
		if strings.HasSuffix(path, "_test.go") || strings.HasSuffix(path, ".md") {
			return nil
		}
		data, err := os.ReadFile(path) //nolint:gosec // test reads repo files
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(repoRoot, path)
		lines := strings.Split(string(data), "\n")

		if d.Name() == "spored.service" {
			units = append(units, sporedUnit{where: rel + ":1", startLine: 1, lines: lines})
			return nil
		}
		for i := 0; i < len(lines); i++ {
			if !sporedUnitPath.MatchString(lines[i]) {
				continue
			}
			m := sporedUnitWrite.FindStringSubmatch(lines[i])
			if m == nil {
				t.Errorf("%s:%d writes the spored unit in a form this gate cannot parse; "+
					"use a `cat > /etc/systemd/system/spored.service <<'EOF'` heredoc or teach "+
					"discoverSporedUnits the new form:\n  %s", rel, i+1, strings.TrimSpace(lines[i]))
				continue
			}
			delim := m[1]
			start := i + 1
			end := -1
			for j := start; j < len(lines); j++ {
				if strings.TrimSpace(lines[j]) == delim {
					end = j
					break
				}
			}
			if end < 0 {
				t.Errorf("%s:%d: heredoc %q writing the spored unit is never closed", rel, i+1, delim)
				continue
			}
			units = append(units, sporedUnit{
				where:     rel + ":" + strconv.Itoa(start+1),
				startLine: start + 1,
				lines:     lines[start:end],
			})
			i = end
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", repoRoot, err)
	}
	// A discovery that found nothing has broken, not proven the repo clean.
	if len(units) == 0 {
		t.Fatalf("found no spored systemd unit under %s — discovery is broken", repoRoot)
	}
	return units
}

// activeDirectives returns the unit's non-comment, non-blank lines with their
// absolute line numbers.
func (u sporedUnit) activeDirectives() map[int]string {
	out := map[int]string{}
	for i, l := range u.lines {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") || strings.HasPrefix(l, ";") {
			continue
		}
		out[u.startLine+i] = l
	}
	return out
}

func sporedRepoRoot() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..") // .../spawn
}

// TestSporedUnit_NoPrivateTmp guards against regressing #66.
//
// spored watches the host-side completion file (default /tmp/SPAWN_COMPLETE),
// which users, `spawn connect`, and nf-spawn write to the real /tmp. systemd's
// PrivateTmp=true gives the daemon an isolated /tmp where it can never see that
// file, silently disabling --on-complete and --pre-stop. Every place that emits
// the spored systemd unit must therefore NOT set PrivateTmp=true.
//
// The emitters are discovered, not listed (#946): a hand-written list missed the
// two units in cmd/app.go.
func TestSporedUnit_NoPrivateTmp(t *testing.T) {
	root := sporedRepoRoot()
	for _, u := range discoverSporedUnits(t, root) {
		for ln, d := range u.activeDirectives() {
			key, val, ok := strings.Cut(d, "=")
			if !ok || strings.TrimSpace(key) != "PrivateTmp" {
				continue
			}
			switch strings.ToLower(strings.TrimSpace(val)) {
			case "no", "false", "off", "0", "disconnected":
				continue
			}
			t.Errorf("spored unit at %s: line %d sets %q — this hides /tmp/SPAWN_COMPLETE "+
				"from spored and breaks --on-complete/--pre-stop (#66)", u.where, ln, d)
		}
	}
}

// TestSporedUnit_AfterNetworkOnline requires every spored unit to order itself
// after, and want, network-online.target. systemd stops units in reverse start
// order, so this is what keeps the network up while spored's shutdown path runs:
// it flushes spawn:compute-seconds (#928) and deregisters DNS, both over the
// network.
func TestSporedUnit_AfterNetworkOnline(t *testing.T) {
	root := sporedRepoRoot()
	for _, u := range discoverSporedUnits(t, root) {
		var after, wants bool
		for _, d := range u.activeDirectives() {
			key, val, ok := strings.Cut(d, "=")
			if !ok {
				continue
			}
			fields := strings.Fields(val)
			has := func() bool {
				for _, f := range fields {
					if f == "network-online.target" {
						return true
					}
				}
				return false
			}
			switch strings.TrimSpace(key) {
			case "After":
				after = after || has()
			case "Wants", "Requires":
				wants = wants || has()
			}
		}
		if !after {
			t.Errorf("spored unit at %s lacks After=network-online.target — spored's shutdown "+
				"flush of spawn:compute-seconds and DNS deregistration need the network (#928)", u.where)
		}
		if !wants {
			t.Errorf("spored unit at %s lacks Wants=network-online.target — After= alone does "+
				"not pull the target in", u.where)
		}
	}
}

// TestDiscoverSporedUnits_FindsKnownEmitters checks the discovery itself, so a
// regex that silently stops matching cannot turn the gates above into no-ops.
// The known emitters are a floor, not the set: discovery may find more.
func TestDiscoverSporedUnits_FindsKnownEmitters(t *testing.T) {
	root := sporedRepoRoot()
	perFile := map[string]int{}
	for _, u := range discoverSporedUnits(t, root) {
		f, _, _ := strings.Cut(u.where, ":")
		perFile[filepath.ToSlash(f)]++
	}
	for f, min := range map[string]int{
		"cmd/app.go":                2,
		"pkg/launcher/bootstrap.go": 1,
		"scripts/spored.service":    1,
	} {
		if perFile[f] < min {
			t.Errorf("discovery found %d spored unit(s) in %s, want at least %d (found: %v)",
				perFile[f], f, min, perFile)
		}
	}
}
