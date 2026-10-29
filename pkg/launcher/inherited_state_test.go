package launcher

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/instancestate"
)

// `spawn ami create` must hand every per-instance state path to the child
// (#1241): after its cleanup runs on the source, an instance launched from the
// image discards all of instancestate.Paths at its first boot, and the source
// itself, rebooting, keeps them. The source here recorded no owner (launched
// before #1241), the case the cleanup's owner record exists for.
func TestImageCleanupHandsEveryStatePathToTheChild(t *testing.T) {
	const source, child = "i-0source", "i-0child"
	setup := func(t *testing.T) (string, []string) {
		root := t.TempDir()
		old := time.Now().Add(-2 * time.Hour)
		var files []string
		for _, p := range instancestate.Paths {
			f := filepath.Join(root, filepath.FromSlash(p))
			if p == "/var/lib/spored/plugins" {
				f = filepath.Join(f, "example", "state.json")
			}
			touch(t, f)
			if err := os.Chtimes(f, old, old); err != nil {
				t.Fatal(err)
			}
			files = append(files, f)
		}
		if out, code := runCleanup(t, root, source); code != 0 || !strings.Contains(out, ImageCleanOK) {
			t.Fatalf("cleanup exited %d:\n%s", code, out)
		}
		return root, files
	}
	boot := time.Now().Add(-time.Minute)

	t.Run("child discards", func(t *testing.T) {
		root, files := setup(t)
		removed, from, err := instancestate.Reconcile(root, child, boot)
		if err != nil {
			t.Fatal(err)
		}
		if from != source {
			t.Errorf("the child's boot found the state owned by %q, want %q: the cleanup did not record the source", from, source)
		}
		for _, f := range files {
			if _, err := os.Lstat(f); err == nil {
				t.Errorf("%s survived the child's first boot", strings.TrimPrefix(f, root))
			}
		}
		if len(removed) == 0 {
			t.Error("the child's boot removed nothing")
		}
	})
	t.Run("source keeps", func(t *testing.T) {
		// Except the --command run state #1057 removes on purpose, discovered
		// from the bootstrap as that gate discovers it.
		runState := map[string]bool{}
		for _, p := range discoverRunState(t, renderedCommandBootstrap(t)) {
			runState[p] = true
		}
		root, files := setup(t)
		removed, _, err := instancestate.Reconcile(root, source, boot)
		if err != nil || len(removed) != 0 {
			t.Fatalf("the source's own reboot removed %v (err %v)", removed, err)
		}
		for _, f := range files {
			if runState[filepath.ToSlash(strings.TrimPrefix(f, root))] {
				continue
			}
			if _, err := os.Lstat(f); err != nil {
				t.Errorf("the cleanup removed %s from the source, which is still running", strings.TrimPrefix(f, root))
			}
		}
	})
}

// The Linux bootstrap body runs the discard after installing spored and before
// anything that reads the state: the daemon (plugins.json), --command
// (/etc/spawn/command), and everything after the body (MPI's peers-file wait,
// the user's script).
func TestBodyDiscardsInheritedStateBeforeAnythingReadsIt(t *testing.T) {
	body := BootstrapBody()
	at := func(re string) int {
		t.Helper()
		loc := regexp.MustCompile(`(?m)` + re).FindStringIndex(body)
		if loc == nil {
			t.Fatalf("the body has no line matching %s", re)
		}
		return loc[0]
	}
	discard := at(`^/usr/local/bin/spored discard-inherited-state \|\| true$`)
	if install := at(`^if ! spore_install_spored_from_buckets `); discard < install {
		t.Error("the discard runs before spored is installed, so on a launch whose image has no spored it cannot run")
	}
	for what, re := range map[string]string{
		"the daemon starts":  `^systemctl start spored$`,
		"--command is read":  `^if \[ -s /etc/spawn/command \]; then$`,
		"session-timeout.sh": `cat > /etc/profile\.d/session-timeout\.sh`,
		"job-array.sh":       `cat > /etc/profile\.d/job-array\.sh`,
	} {
		if p := at(re); discard > p {
			t.Errorf("the discard runs after %s", what)
		}
	}
}
