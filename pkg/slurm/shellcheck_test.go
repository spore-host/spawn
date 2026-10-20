package slurm

import (
	"testing"
	"time"

	"github.com/spore-host/spawn/internal/shellcheckgate"
)

// shellcheck on the scripts a converted Slurm job runs (#689 part C). The
// user's own batch body is passed through as-is, so a trivially clean one is
// used: the gate is about the lines spawn adds around it.
func TestConvertedScripts_Shellcheck(t *testing.T) {
	job := &SlurmJob{
		FilePath:     "/home/me/my job.sbatch",
		JobName:      "align samples",
		TimeLimit:    2 * time.Hour,
		Nodes:        2,
		TasksPerNode: 4,
		CPUsPerTask:  2,
		WorkingDir:   "/data/run 1",
		ScriptBody:   "echo \"hello from $SLURM_JOB_NAME\"\n",
		Array:        &ArraySpec{Start: 0, End: 9, Step: 1},
	}
	shellcheckgate.Check(t, "pkg/slurm.generateSingleTaskScript", generateSingleTaskScript(job))
	shellcheckgate.Check(t, "pkg/slurm.generateMPIScript", generateMPIScript(job))
	shellcheckgate.Check(t, "pkg/slurm.generateArrayTaskScript", generateArrayTaskScript(job, 3))
}
