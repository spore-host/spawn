package userdata

import (
	"regexp"
	"strings"
	"testing"
)

// Text half of spawn#1481; mpi_rank_wrapper_test.go runs the rendered options
// under a real mpirun (as root, TestRenderedMPIRunStartsAsRoot, when the test
// runs as root). mpirun runs as root by design: the bootstrap is root's, and
// so are the SSH mesh and the peers' completion write. Open MPI 4.0.1 (AL2)
// ignores the OMPI_ALLOW_RUN_AS_ROOT variables that /etc/profile.d/mpi.sh
// sets, and only the --allow-run-as-root flag works on every supported
// release, so every launch must carry the flag itself.

const mpirunRootFlag = "--allow-run-as-root"

// mpirunOptions is what the rendered rank-0 mpirun line passes before -np.
func mpirunOptions(t *testing.T, script string) []string {
	t.Helper()
	m := regexp.MustCompile(`^mpirun (.*?) -np `).FindStringSubmatch(mpirunLine(t, script))
	if m == nil {
		t.Fatalf("cannot find mpirun's options before -np in:\n%s", mpirunLine(t, script))
	}
	return strings.Fields(m[1])
}

// profileRootEnv is the OMPI_ALLOW_RUN_AS_ROOT* environment the rendered
// /etc/profile.d/mpi.sh exports, as KEY=VALUE.
func profileRootEnv(t *testing.T, script string) []string {
	t.Helper()
	var env []string
	for _, m := range regexp.MustCompile(`(?m)^export (OMPI_ALLOW_RUN_AS_ROOT\w*=\S+)$`).FindAllStringSubmatch(script, -1) {
		env = append(env, m[1])
	}
	return env
}

// TestEveryMPIRunAllowsRoot: every mpirun invocation spawn renders or writes
// (discovered, mpirunLaunches) passes --allow-run-as-root to mpirun itself,
// before the program it runs per rank, not inside the user's command.
func TestEveryMPIRunAllowsRoot(t *testing.T) {
	for _, l := range mpirunLaunches(t) {
		opts := l.line
		if i := strings.Index(opts, " bash "+mpiRankWrapper+" "); i >= 0 {
			opts = opts[:i]
		}
		found := false
		for _, w := range strings.Fields(opts) {
			found = found || w == mpirunRootFlag
		}
		if !found {
			t.Errorf("%s: mpirun runs as root but does not pass %s; Open MPI 4.0.1 (AL2) ignores OMPI_ALLOW_RUN_AS_ROOT and refuses to start (#1481): %s",
				l.where, mpirunRootFlag, strings.TrimSpace(l.line))
		}
	}
}
