package userdata

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Text half of spawn#1479; mpi_rank_wrapper_test.go executes the wrapper.

const (
	mpiRankWrapper    = "/etc/spawn/mpi-rank"
	rankLastLine      = "RANK_LAST_LINE_BEFORE_EXIT"
	rankStragglerNote = "not waiting for processes still holding --mpi-command's output"
)

// rankWrapperBody is what lands in /etc/spawn/mpi-rank.
func rankWrapperBody(t *testing.T, script string) string {
	t.Helper()
	const open = "cat > " + mpiRankWrapper + " <<'EOFMPIRANK'\n"
	i := strings.Index(script, open)
	if i < 0 {
		t.Fatalf("no quoted here-doc writing %s:\n%s", mpiRankWrapper, relevant(script))
	}
	rest := script[i+len(open):]
	j := strings.Index(rest, "\nEOFMPIRANK\n")
	if j < 0 {
		t.Fatal("unterminated rank-wrapper here-doc")
	}
	return rest[:j+1]
}

// TestEveryMPIRankLaunchRunsTheWrapper: every mpirun invocation spawn renders
// runs the rank wrapper, and every member, not only rank 0, writes it (mpirun
// starts ranks on peers through orted, which runs the same path there). The
// invocations are discovered: every non-comment line of the rendered scripts
// that calls mpirun with a flag, and every Go source line in cmd/ and pkg/
// that does, so a second launch path cannot appear without the wrapper.
func TestEveryMPIRankLaunchRunsTheWrapper(t *testing.T) {
	invocation := regexp.MustCompile(`(^|[;&|(]\s*)mpirun\s+-`)
	launches := 0
	for name, cfg := range map[string]MPIConfig{
		"rank 0":             {JobArrayIndex: 0, JobArraySize: 2},
		"rank 1":             {JobArrayIndex: 1, JobArraySize: 2},
		"efa, skip install":  {JobArrayIndex: 0, JobArraySize: 4, EFAEnabled: true, SkipInstall: true, MPIProcessesPerNode: 8},
		"completion file":    {JobArrayIndex: 0, JobArraySize: 2, CompletionFile: "/var/run/x y.json", ReadyGate: "/run/spawn/mpi-ready"},
		"peer, ppn and gate": {JobArrayIndex: 3, JobArraySize: 4, MPIProcessesPerNode: 2, ReadyGate: "/run/spawn/mpi-ready"},
	} {
		cfg.Region, cfg.JobArrayID, cfg.MPICommand = "us-east-1", "arr-1", "./sim"
		script, err := GenerateMPIUserData(cfg)
		if err != nil {
			t.Fatal(err)
		}
		_ = rankWrapperBody(t, script) // every member writes it
		for _, line := range strings.Split(script, "\n") {
			if trim := strings.TrimSpace(line); strings.HasPrefix(trim, "#") || !invocation.MatchString(trim) {
				continue
			}
			launches++
			if !strings.Contains(line, " bash "+mpiRankWrapper+" ") {
				t.Errorf("%s: an mpirun invocation does not run the rank wrapper %s: %s", name, mpiRankWrapper, strings.TrimSpace(line))
			}
		}
	}
	if launches == 0 {
		t.Fatal("found no mpirun invocation in any rendered script: the scan is broken, not the template")
	}

	var sources []string
	for _, root := range []string{"../../cmd", "../../pkg"} {
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return nil //nolint:nilerr // an unreadable file holds no invocation to check
			}
			for i, line := range strings.Split(string(b), "\n") {
				trim := strings.TrimSpace(line)
				if strings.HasPrefix(trim, "//") || strings.HasPrefix(trim, "#") || !invocation.MatchString(trim) {
					continue
				}
				sources = append(sources, fmt.Sprintf("%s:%d", p, i+1))
				if !strings.Contains(line, " bash "+mpiRankWrapper+" ") {
					t.Errorf("%s:%d launches mpirun without the rank wrapper %s (#1479): %s", p, i+1, mpiRankWrapper, trim)
				}
			}
			return nil
		})
	}
	if len(sources) == 0 {
		t.Fatal("found no mpirun invocation in cmd/ or pkg/ source: the scan is broken")
	}
	t.Logf("mpirun invocations in source: %v", sources)
}
