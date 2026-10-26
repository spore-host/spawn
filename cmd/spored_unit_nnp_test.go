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

	"github.com/spore-host/spawn/internal/treewalk"
)

// nnpUnitHeredoc matches a shell line that writes the spored unit through a
// heredoc (`cat > /etc/systemd/system/spored.service <<'EOF'`). Group 1 is the
// delimiter.
var nnpUnitHeredoc = regexp.MustCompile(`^\s*(?:sudo\s+)?(?:cat\s*>|tee(?:\s+-a)?)\s*/etc/systemd/system/spored\.service\s*<<-?\s*['"]?(\w+)['"]?\s*$`)

// nnpUnitTarget matches any line that makes the unit's install path a write
// target, whatever the shape. Every such line must also match nnpUnitHeredoc, so an
// emitter this gate cannot read fails it instead of slipping past.
var nnpUnitTarget = regexp.MustCompile(`(?:>|tee(?:\s+-a)?|cp\s+\S+|install\s+.*)\s*/etc/systemd/system/spored\.service`)

type nnpUnit struct {
	where string
	lines []string
}

// findSporedUnitBodies discovers every spored systemd unit the module defines:
// each file named spored.service and each heredoc that writes
// /etc/systemd/system/spored.service (CLAUDE.md gate rule 4 — no list). #958
// introduces an equivalent discoverSporedUnits for the PrivateTmp gate; when both
// land, this test should switch to it.
func findSporedUnitBodies(t *testing.T) []nnpUnit {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(thisFile), "..")
	var units []nnpUnit
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if treewalk.Vanished(root, path, err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "dist", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		// Tests mention the path to check it; markdown describes it.
		if strings.HasSuffix(path, "_test.go") || strings.HasSuffix(path, ".md") {
			return nil
		}
		data, err := os.ReadFile(path) //nolint:gosec // test reads repo files
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		lines := strings.Split(string(data), "\n")
		if d.Name() == "spored.service" {
			units = append(units, nnpUnit{where: rel, lines: lines})
			return nil
		}
		for i := 0; i < len(lines); i++ {
			if !nnpUnitTarget.MatchString(lines[i]) {
				continue
			}
			m := nnpUnitHeredoc.FindStringSubmatch(lines[i])
			if m == nil {
				t.Errorf("%s:%d writes the spored unit in a form this gate cannot parse:\n  %s",
					rel, i+1, strings.TrimSpace(lines[i]))
				continue
			}
			end := -1
			for j := i + 1; j < len(lines); j++ {
				if strings.TrimSpace(lines[j]) == m[1] {
					end = j
					break
				}
			}
			if end < 0 {
				t.Errorf("%s:%d: heredoc %q writing the spored unit is never closed", rel, i+1, m[1])
				continue
			}
			units = append(units, nnpUnit{where: rel + ":" + strconv.Itoa(i+2), lines: lines[i+1 : end]})
			i = end
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	// A discovery that found nothing has broken, not proven the repo clean.
	if len(units) == 0 {
		t.Fatalf("found no spored systemd unit under %s — discovery is broken", root)
	}
	return units
}

// TestSporedUnit_NoNewPrivileges requires NoNewPrivileges= set truthy in the
// [Service] section of every spored unit. spored runs as root and execs user hooks
// (pre-stop, queue jobs) and, via `su - <user>`, drops to the instance user; none
// of that needs to GAIN privilege, so a child that tries (a setuid binary, file
// capabilities) should be refused. The canonical unit (pkg/launcher/bootstrap.go),
// scripts/spored.service and the DCV unit always set it; the web-app unit in
// cmd/app.go did not.
func TestSporedUnit_NoNewPrivileges(t *testing.T) {
	for _, u := range findSporedUnitBodies(t) {
		section := ""
		ok := false
		for _, l := range u.lines {
			l = strings.TrimSpace(l)
			if l == "" || strings.HasPrefix(l, "#") || strings.HasPrefix(l, ";") {
				continue
			}
			if strings.HasPrefix(l, "[") && strings.HasSuffix(l, "]") {
				section = l
				continue
			}
			key, val, found := strings.Cut(l, "=")
			if !found || section != "[Service]" || strings.TrimSpace(key) != "NoNewPrivileges" {
				continue
			}
			switch strings.ToLower(strings.TrimSpace(val)) {
			case "true", "yes", "on", "1":
				ok = true
			default:
				ok = false // a later falsy assignment wins in systemd
			}
		}
		if !ok {
			t.Errorf("spored unit at %s does not set NoNewPrivileges=true in [Service]", u.where)
		}
	}
}
