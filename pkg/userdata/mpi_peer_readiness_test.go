package userdata

import (
	"strings"
	"testing"
)

// readinessProbe is the per-peer login the readiness loop runs.
const readinessProbe = `"$PEER_IP" true`

// Rank 0 must wait for every peer to accept a non-interactive SSH before
// mpirun, not sleep a constant (#752).
//
// The peers file the script already waits for means the CONTROLLER resolved
// every peer's IP. It says nothing about whether a peer's sshd is up, or whether
// rank 0's public key has reached that peer's /root/.ssh/authorized_keys — which
// pkg/mpicohort/assembler.go is what actually distributes. mpirun SSHes to every
// host in the hostfile, so that is the real precondition, and a constant pad is a
// race that WORSENS with cohort size.
func TestMPIWaitsForPeerSSHBeforeMpirun(t *testing.T) {
	script, err := GenerateMPIUserData(MPIConfig{
		Region:        "us-east-1",
		JobArrayID:    "arr-1",
		JobArrayIndex: 0,
		JobArraySize:  4,
		MPICommand:    "./gchp",
	})
	if err != nil {
		t.Fatalf("GenerateMPIUserData: %v", err)
	}

	// The pad must be gone. Anchored to the line to avoid matching the bounded
	// loop's own `sleep 5`.
	for _, line := range strings.Split(script, "\n") {
		if strings.TrimSpace(line) == "sleep 10" {
			t.Error("the bare `sleep 10` pad is still in the rendered script; it is a race " +
				"against peer sshd + authorized_keys that gets worse with cohort size")
		}
	}

	// BatchMode is load-bearing: it tests sshd AND authorized_keys together. A
	// port probe would pass while MPI still failed.
	if !strings.Contains(script, "BatchMode=yes") {
		t.Error("the peer readiness check does not use ssh BatchMode=yes, so it cannot " +
			"distinguish 'sshd is up' from 'MPI can actually log in'")
	}

	// It must probe every host in the hostfile, not just one.
	if !strings.Contains(script, "/tmp/mpi-hostfile") {
		t.Error("the readiness check does not read the hostfile, so it cannot cover all peers")
	}

	// Bounded, with a NAMED failure — the discipline the peers-file wait above it
	// already follows. An unbounded version would hang the boot, and a silent one
	// would surface as an opaque mpirun error.
	// Anchored to the readiness PROBE (an ssh running `true`), not the bare
	// option: the completion hand-off to peers (#894) also uses BatchMode=yes.
	readyIdx := strings.Index(script, readinessProbe)
	if readyIdx < 0 {
		t.Fatalf("no readiness probe (%q) in the rendered script", readinessProbe)
	}
	tail := script[readyIdx:]
	if !strings.Contains(tail, "600") {
		t.Error("the peer readiness wait has no deadline")
	}
	// The failure exits non-zero, and the script's exit trap writes the failed
	// record (#1483), through spawn_mpi_done with --mpi-command so the reachable
	// peers are signalled too (#894). Executed, with and without a command, by
	// TestMPIPeerReadinessFailureSignalsReachablePeers and
	// TestMPIFailureWritesAFailedCompletion.
	if !strings.Contains(tail, "exit 1") {
		t.Error("a peer that never accepts SSH does not fail the script, so no SPAWN_COMPLETE " +
			"failure record is written and the cohort fails with an opaque mpirun error instead")
	}

	// Ordering: readiness must come after the hostfile is built (it reads it) and
	// before mpirun (it gates it).
	hostfile := strings.Index(script, "> /tmp/mpi-hostfile")
	// The INVOCATION, not the word: "mpirun" also appears in the template's
	// comments near the top, which made the first version of this assertion
	// compare against index 66 and fail on a correct script.
	mpirun := strings.Index(script, "mpirun --mca")
	if !(hostfile < readyIdx && readyIdx < mpirun) {
		t.Errorf("ordering wrong: hostfile=%d readiness=%d mpirun=%d — readiness must read "+
			"the hostfile and gate mpirun", hostfile, readyIdx, mpirun)
	}
}

// Only rank 0 runs mpirun, so only rank 0 should pay the readiness wait. A peer
// waiting for its own peers would deadlock the cohort.
func TestMPIPeerReadinessIsRankZeroOnly(t *testing.T) {
	script, err := GenerateMPIUserData(MPIConfig{
		Region: "us-east-1", JobArrayID: "arr-1",
		JobArrayIndex: 0, JobArraySize: 4, MPICommand: "./gchp",
	})
	if err != nil {
		t.Fatalf("GenerateMPIUserData: %v", err)
	}
	guard := strings.Index(script, `-eq 0 ]; then`)
	ready := strings.Index(script, readinessProbe)
	if guard < 0 || ready < guard {
		t.Errorf("the readiness wait is not inside the rank-0 guard (guard=%d ready=%d); "+
			"every peer would wait for every other peer", guard, ready)
	}
}
