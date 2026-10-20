package slurm

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestGeneratedScripts_QuoteJobNameAndWorkingDir runs each generated script.
// #SBATCH --job-name and --chdir are free text. Unquoted, a job name with a
// space exported only its first word (and the rest as empty variables), and a
// working directory with a space made cd fail, after which the body ran
// wherever the script happened to start (found by the #689 shellcheck gate).
func TestGeneratedScripts_QuoteJobNameAndWorkingDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("runs the generated bash script")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed")
	}
	dir := filepath.Join(t.TempDir(), "run 1 it's")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	job := &SlurmJob{
		JobName:      "align samples; echo INJECTED",
		WorkingDir:   dir,
		Nodes:        2,
		TasksPerNode: 1,
		Array:        &ArraySpec{Start: 0, End: 1, Step: 1},
		ScriptBody:   "printf '%s|%s\\n' \"$SLURM_JOB_NAME\" \"$PWD\"\n",
	}
	want := job.JobName + "|" + dir
	scripts := map[string]string{
		"single": generateSingleTaskScript(job),
		"mpi":    generateMPIScript(job),
		"array":  generateArrayTaskScript(job, 0),
	}
	for name, script := range scripts {
		t.Run(name, func(t *testing.T) {
			out, err := exec.Command(bash, "-c", script).CombinedOutput() // nosemgrep
			if err != nil {
				t.Fatalf("script failed: %v\n%s", err, out)
			}
			if got := strings.TrimSpace(string(out)); got != want {
				t.Errorf("job name|cwd = %q, want %q", got, want)
			}
		})
	}

	t.Run("a missing working dir stops the job", func(t *testing.T) {
		missing := *job
		missing.WorkingDir = filepath.Join(dir, "absent")
		out, err := exec.Command(bash, "-c", generateSingleTaskScript(&missing)).CombinedOutput() // nosemgrep
		if err == nil {
			t.Fatalf("script ran its body after cd failed:\n%s", out)
		}
	})
}
