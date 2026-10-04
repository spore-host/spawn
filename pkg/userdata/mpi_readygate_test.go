package userdata

import (
	"strings"
	"testing"
)

// TestMPIReadyGateSignalledBeforeMpirun is the producer half of spawn#664.
//
// The MPI setup script is appended AFTER the bootstrap body that launches
// --command, so a workload starts before the peers file or hostfile exist. The
// gate closes that window — but WHERE it is signalled decides whether the fix
// works or makes things worse.
//
// It must fire once the hostfile is built and BEFORE the rank-0 mpirun. That
// mpirun is the job itself and can run for hours; signalling after it would make
// a --command workload wait out the entire run, which looks exactly like a hang.
func TestMPIReadyGateSignalledBeforeMpirun(t *testing.T) {
	script, err := GenerateMPIUserData(MPIConfig{
		Region:        "us-east-1",
		JobArrayID:    "arr-1",
		JobArrayIndex: 0,
		JobArraySize:  2,
		MPICommand:    "./gchp",
		ReadyGate:     "/run/spawn/mpi-ready",
	})
	if err != nil {
		t.Fatalf("GenerateMPIUserData: %v", err)
	}

	gate := strings.Index(script, "/run/spawn/mpi-ready")
	hostfile := strings.Index(script, "> /tmp/mpi-hostfile")
	mpirun := strings.Index(script, "mpirun --mca")

	if gate < 0 {
		t.Fatal("the MPI script never signals its gate, so a --command workload waits the " +
			"full timeout and then fails a launch that would have worked (#664)")
	}
	if hostfile < 0 || mpirun < 0 {
		t.Fatalf("could not locate the hostfile build (%d) or mpirun (%d)", hostfile, mpirun)
	}
	if gate < hostfile {
		t.Errorf("gate signalled at %d, before the hostfile is built at %d — the workload "+
			"would start with no hostfile, which is the bug", gate, hostfile)
	}
	if gate > mpirun {
		t.Errorf("gate signalled at %d, AFTER mpirun at %d. That mpirun is the job and can "+
			"run for hours, so --command would wait out the whole run — indistinguishable "+
			"from a hang, and worse than the bug being fixed.", gate, mpirun)
	}

	// An empty hostfile means no peers resolved. Reporting that as ready would
	// launch the workload into a cluster of one.
	if !strings.Contains(script, "failed:no MPI peers resolved") {
		t.Error("an empty hostfile must be reported as a FAILED gate, not a ready one")
	}
}

// TestMPIReadyGateOmittedWhenUnset keeps the script unchanged for callers that
// do not declare the gate — a signal with nothing waiting is inert, but emitting
// it unconditionally would change every existing MPI script.
func TestMPIReadyGateOmittedWhenUnset(t *testing.T) {
	script, err := GenerateMPIUserData(MPIConfig{
		Region:       "us-east-1",
		JobArrayID:   "arr-1",
		JobArraySize: 2,
	})
	if err != nil {
		t.Fatalf("GenerateMPIUserData: %v", err)
	}
	if strings.Contains(script, "/run/spawn") {
		t.Errorf("no ReadyGate was set, so the script must not reference one:\n%s", script)
	}
}
