package instancestate

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/internal/treewalk"
	"github.com/spore-host/spawn/pkg/userdata"
)

const parent, child = "i-0parent", "i-0child"

// stateFile is the file a test puts at p: p itself, or, for a directory in
// Paths, a file beneath it (the plugin state store holds one per plugin).
func stateFile(p string) string {
	if p == "/var/lib/spored/plugins" { // the plugin state store, a directory
		return p + "/example/state.json"
	}
	return p
}

func put(t *testing.T, root, p string, mtime time.Time) string {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(p))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(full, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return full
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func owner(t *testing.T, root string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(OwnerFile)))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// The simulated image boot (#1241): a volume carrying another instance's state
// at every path, booted as a different instance, comes up without any of it,
// keeps what this boot already wrote, and records the new owner.
func TestSimulatedImageBootDiscardsInheritedState(t *testing.T) {
	root := t.TempDir()
	boot := time.Now().Add(-time.Minute).Truncate(time.Second)
	snapshot := boot.Add(-24 * time.Hour)
	put(t, root, OwnerFile, snapshot)
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(OwnerFile)), []byte(parent+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stale []string
	for _, p := range Paths {
		stale = append(stale, put(t, root, stateFile(p), snapshot))
	}
	// Written during this boot, before the discard ran: the child's own plugin
	// declarations (the bootstrap writes them before spored is installed) and
	// the peers file the MPI control plane pushed over SSM.
	fresh := []string{
		put(t, root, "/etc/spawn/plugins.json.child", boot.Add(10*time.Second)),
		put(t, root, "/var/lib/spored/plugins/child-plugin/state.json", boot.Add(10*time.Second)),
	}
	freshPeers := put(t, root, "/etc/spawn/job-array-peers.json", boot.Add(20*time.Second))
	stale = removeString(stale, freshPeers)
	fresh = append(fresh, freshPeers)

	removed, from, err := Reconcile(root, child, boot)
	if err != nil {
		t.Fatal(err)
	}
	if from != parent {
		t.Errorf("from = %q, want %q", from, parent)
	}
	for _, p := range stale {
		if exists(p) {
			t.Errorf("inherited %s survived the boot of %s", p, child)
		}
	}
	for _, p := range fresh {
		if !exists(p) {
			t.Errorf("%s, written during this boot, was removed", p)
		}
	}
	if len(removed) != len(stale) {
		t.Errorf("removed %d files, want %d: %v", len(removed), len(stale), removed)
	}
	if got := owner(t, root); got != child {
		t.Errorf("owner = %q, want %q", got, child)
	}

	// Idempotent: the daemon and the bootstrap both run it.
	put(t, root, "/etc/spawn/command", snapshot) // even a stale-looking file is now the owner's
	removed, from, err = Reconcile(root, child, boot)
	if err != nil || from != "" || len(removed) != 0 {
		t.Errorf("second run: removed %v from %q, err %v; want nothing", removed, from, err)
	}
}

func removeString(s []string, x string) []string {
	var out []string
	for _, v := range s {
		if v != x {
			out = append(out, v)
		}
	}
	return out
}

// The source instance's own reboots, the CreateImage reboot among them, keep
// everything.
func TestOwnerRebootKeepsItsState(t *testing.T) {
	root := t.TempDir()
	boot := time.Now().Truncate(time.Second)
	if err := os.MkdirAll(filepath.Join(root, "var/lib/spawn"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(OwnerFile)), []byte(parent+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, p := range Paths {
		files = append(files, put(t, root, stateFile(p), boot.Add(-time.Hour)))
	}
	removed, from, err := Reconcile(root, parent, boot)
	if err != nil || from != "" || len(removed) != 0 {
		t.Fatalf("removed %v from %q, err %v; want nothing", removed, from, err)
	}
	for _, p := range files {
		if !exists(p) {
			t.Errorf("%s removed on its owner's reboot", p)
		}
	}
}

// No owner recorded cannot be told from an older instance that never recorded
// itself; its live state is kept, and it is recorded now.
func TestNoOwnerKeepsStateAndRecordsOne(t *testing.T) {
	root := t.TempDir()
	boot := time.Now().Truncate(time.Second)
	f := put(t, root, "/etc/spawn/plugins.json", boot.Add(-time.Hour))
	removed, _, err := Reconcile(root, child, boot)
	if err != nil || len(removed) != 0 || !exists(f) {
		t.Fatalf("removed %v, err %v, plugins.json present %v", removed, err, exists(f))
	}
	if got := owner(t, root); got != child {
		t.Errorf("owner = %q, want %q", got, child)
	}
}

// Without the boot time nothing is recorded: adopting the inherited state as
// this instance's own would make it permanent.
func TestUnknownBootTimeRecordsNothing(t *testing.T) {
	root := t.TempDir()
	if _, _, err := Reconcile(root, child, time.Time{}); err == nil {
		t.Fatal("want an error")
	}
	if exists(filepath.Join(root, filepath.FromSlash(OwnerFile))) {
		t.Error("owner recorded with no boot time")
	}
	if _, _, err := Reconcile(root, "", time.Now()); err == nil {
		t.Fatal("want an error for an empty instance ID")
	}
}

func TestBootTimeFrom(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "stat")
	if err := os.WriteFile(p, []byte("cpu  1 2 3\nintr 5\nbtime 1700000000\nprocesses 9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := bootTimeFrom(p)
	if err != nil || !got.Equal(time.Unix(1700000000, 0)) {
		t.Errorf("bootTimeFrom = %v, %v", got, err)
	}
	if err := os.WriteFile(p, []byte("cpu 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := bootTimeFrom(p); err == nil {
		t.Error("no btime: want an error")
	}
}

// notInstanceState is every path under the state roots that spawn's source
// names and that is deliberately NOT discarded, with why.
var notInstanceState = map[string]string{
	"/etc/spawn/spored-signing-key.pem":  "spore.host's public signing key, the same on every instance",
	"/etc/spawn/local.yaml":              "operator-written spored config; spawn reads it and never writes it",
	"/etc/profile.d/mpi.sh":              "environment for the MPI install on the volume, which the image keeps",
	"/etc/profile.d/efa.sh":              "environment for the EFA install on the volume, which the image keeps",
	OwnerFile:                            "the owner record itself",
	userdata.MPIClusterKeyStripOwnerFile: "StripClusterKeys's own owner record (#1343); naming another instance is what makes it strip",
}

// stateRoots are the directories spawn's per-instance state lives in on the
// root volume. /run is a tmpfs and never in an image; /tmp is #1057's
// (launcher.ImageStateCleanupScript); /var/log is logs.
var statePath = regexp.MustCompile(`(/etc/spawn|/etc/profile\.d|/var/lib/spored|/var/lib/spawn)/[A-Za-z0-9._-]+`)

// TestEveryWrittenStatePathIsCovered discovers, from every string literal in
// the repository's non-test Go source, the paths spawn names under the state
// roots, and requires each to be covered by Paths (or beneath a directory in
// it) or listed in notInstanceState. A templated name ($VAR, a glob) is not a
// path this can check and is skipped; the one there is today, the command-ran
// marker, is keyed on the instance ID already (#1057).
func TestEveryWrittenStatePathIsCovered(t *testing.T) {
	found := discoverStatePaths(t)
	if len(found) < 15 {
		t.Fatalf("discovered %d state paths: the discovery has broken (%v)", len(found), found)
	}
	covered := map[string]bool{}
	for p, where := range found {
		if _, ok := notInstanceState[p]; ok {
			continue
		}
		var hit string
		for _, q := range Paths {
			if p == q || strings.HasPrefix(p, q+"/") {
				hit = q
			}
		}
		if hit == "" {
			t.Errorf("%s (named in %s) is per-instance state an image would carry to every instance launched from it: "+
				"add it to instancestate.Paths, or to notInstanceState with why it belongs in the image (#1241)", p, where)
			continue
		}
		covered[hit] = true
	}
	for p := range notInstanceState {
		if _, ok := found[p]; !ok {
			t.Errorf("notInstanceState entry %s is named nowhere in the source; remove it", p)
		}
	}
	for _, q := range Paths {
		if !covered[q] {
			t.Errorf("Paths entry %s is named nowhere in the source; remove it, or the discovery has missed a writer", q)
		}
	}
}

func discoverStatePaths(t *testing.T) map[string]string {
	t.Helper()
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	found := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if treewalk.Vanished(root, path, err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "testdata", "dist":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			if treewalk.Vanished(root, path, perr) {
				return nil
			}
			return perr
		}
		rel, _ := filepath.Rel(root, path)
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			for _, m := range statePath.FindAllStringIndex(s, -1) {
				p := strings.TrimRight(s[m[0]:m[1]], ".")
				if m[1] < len(s) && strings.ContainsRune("$*{<", rune(s[m[1]])) {
					continue // templated
				}
				if strings.HasSuffix(p, "-") {
					continue
				}
				if _, seen := found[p]; !seen {
					found[p] = filepath.ToSlash(rel)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range found {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	t.Logf("discovered state paths: %v", keys)
	return found
}
