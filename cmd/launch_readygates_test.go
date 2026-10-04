package cmd

import (
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/launcher"
	"github.com/spore-host/spawn/pkg/userdata"
)

// TestMPIGateDeclaredAndSignalled is the interlock for spawn#664, and the thing
// most likely to break later.
//
// The gate has two halves in two packages: buildUserData DECLARES it (because
// the declaration must be written near the top of the script, before --command
// starts waiting), and the MPI script appended by buildJobArrayMemberConfig
// SIGNALS it. Neither half is any use alone, and the failure modes are
// asymmetric:
//
//   - declared but never signalled: --command waits the full 600s and then FAILS
//     a launch that works today. A regression that bills for ten minutes of
//     nothing on every MPI launch.
//   - signalled but never declared: inert, and #664 is simply still broken.
//
// So this asserts both ends agree on the same path, rather than trusting two
// separate tests that each pass on their own.
func TestMPIGateDeclaredAndSignalled(t *testing.T) {
	script, err := userdata.GenerateMPIUserData(userdata.MPIConfig{
		Region:         "us-east-1",
		JobArrayID:     "arr-1",
		JobArraySize:   2,
		BinariesBucket: "b",
		ReadyGate:      launcher.MPIReadyGate,
	})
	if err != nil {
		t.Fatalf("GenerateMPIUserData: %v", err)
	}

	if !strings.Contains(script, launcher.MPIReadyGate) {
		t.Fatalf("the MPI script does not write %s, but buildUserData declares it as a "+
			"gate — --command would wait %ds and then fail the launch",
			launcher.MPIReadyGate, launcher.ReadyGateTimeoutSecs)
	}

	// And the waiting side must read the same directory the producer creates.
	if !strings.HasPrefix(launcher.MPIReadyGate, launcher.ReadyDir+"/") {
		t.Errorf("MPIReadyGate (%s) is outside ReadyDir (%s); the producer mkdir's one and "+
			"the consumer polls the other", launcher.MPIReadyGate, launcher.ReadyDir)
	}
}

// TestReadyGatesForLaunch: declaring a gate no one signals is the expensive
// direction, so the declaration must track --mpi exactly.
func TestReadyGatesForLaunch(t *testing.T) {
	if got := readyGatesForLaunch(false); len(got) != 0 {
		t.Errorf("without --mpi there is no MPI script to signal the gate, so none must be "+
			"declared; got %v — that would stall --command for the full timeout and then "+
			"fail a launch that works today", got)
	}

	got := readyGatesForLaunch(true)
	if len(got) != 1 || got[0] != launcher.MPIReadyGate {
		t.Errorf("readyGatesForLaunch(true) = %v, want [%s]: without it --command runs "+
			"before mpirun has a hostfile or a peer (#664)", got, launcher.MPIReadyGate)
	}
}
