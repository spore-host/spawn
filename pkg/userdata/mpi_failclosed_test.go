package userdata

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// These are the #1483 gates. On a stock AL2 --ami the MPI user data died at its
// jq hostfile step (127, jq not installed), after the peers-file wait and
// before the peer-SSH wait, the two steps that wrote a failed completion record
// on their own. So no member wrote one, --on-complete never fired, and the
// cohort billed to its TTL. Owner principle: a failure must never leave
// instances billing silently to the TTL.
//
// So the script fails closed: any exit before a record is written writes a
// failed one, with the exit status and "source": "mpi-bootstrap", on the
// failing member; on rank 0 with --mpi-command, once the hostfile exists, it is
// handed to every peer too, as mpirun's result is (#894). Each case forces one
// step of the whole rendered script to fail in the sandbox (runSandbox).

// forwardAwk fails awk on the peers file, which is the hostfile step, and
// runs the real awk (the next on PATH) for everything else.
const forwardAwk = `#!/bin/bash
case " $* " in *job-array-peers.json*) echo "awk: forced hostfile failure" >&2; exit 2 ;; esac
self=$(dirname "$0")
for d in ${PATH//:/ }; do
  [ "$d" != "$self" ] && [ -x "$d/awk" ] && exec "$d/awk" "$@"
done
exit 127
`

func mpiFailCfg(rank int, cmd string) MPIConfig {
	return MPIConfig{Region: "us-east-1", JobArrayID: "arr-1", JobArrayIndex: rank, JobArraySize: 3,
		MPICommand: cmd, ReadyGate: "/run/spawn/mpi-ready"}
}

type failCase struct {
	name string
	opts sandboxOpts
	rc   int // the forced step's status: the script's and the record's
	// peersSignalled: rank 0 hands the failed record to the other members.
	peersSignalled bool
}

func failCases() []failCase {
	cohort := []string{rank0IP, peerA, peerB}
	var cases []failCase
	for _, rank := range []int{0, 1} {
		self := []string{rank0IP, peerA}[rank]
		at := func(name string, o sandboxOpts, rc int, signalled bool) {
			o.cfg = mpiFailCfg(rank, "hostname")
			o.self = self
			cases = append(cases, failCase{name: "rank" + strconv.Itoa(rank) + "/" + name, opts: o, rc: rc, peersSignalled: signalled && rank == 0})
		}
		at("install", sandboxOpts{peers: cohort, noMPI: true, stubs: map[string]string{"yum": "#!/bin/bash\nexit 1\n"}}, 1, false)
		at("peers file never arrives", sandboxOpts{}, 1, false)
		at("hostfile", sandboxOpts{peers: cohort, stubs: map[string]string{"awk": forwardAwk}}, 2, false)
		at("mpirun setup", sandboxOpts{peers: cohort, setup: func(t *testing.T, root string) {
			// The rank wrapper's path is a directory, so writing it fails.
			if err := os.MkdirAll(filepath.Join(root, "etc/spawn/mpi-rank"), 0o755); err != nil {
				t.Fatal(err)
			}
		}}, 1, true)
	}
	cases = append(cases,
		failCase{name: "rank0/key exchange", opts: sandboxOpts{cfg: mpiFailCfg(0, "hostname"), self: rank0IP, peers: cohort,
			stubs: map[string]string{"ssh-keygen": "#!/bin/bash\nexit 1\n"}}, rc: 1},
		// Without --mpi-command there is no mpirun to report, but a setup
		// failure is still recorded, for --on-complete.
		failCase{name: "rank0/peer readiness, no --mpi-command", opts: sandboxOpts{cfg: mpiFailCfg(0, ""), self: rank0IP, peers: cohort,
			unreachable: peerB}, rc: 1},
		failCase{name: "rank1/hostfile, no --mpi-command", opts: sandboxOpts{cfg: mpiFailCfg(1, ""), self: peerA, peers: cohort,
			stubs: map[string]string{"awk": forwardAwk}}, rc: 2},
	)
	return cases
}

func assertFailedCompletion(t *testing.T, r sandboxRun, c failCase) {
	t.Helper()
	if r.rc != c.rc {
		t.Errorf("the member script exited %d, want the failed step's %d", r.rc, c.rc)
	}
	rec, ok := r.record(t, "tmp/SPAWN_COMPLETE")
	if !ok {
		t.Fatalf("no SPAWN_COMPLETE after a failed MPI step, so --on-complete never fires and the member bills to its TTL (#1483)")
	}
	if rec.Status != "failed" || rec.ExitCode == nil || *rec.ExitCode != c.rc || rec.Source != "mpi-bootstrap" {
		t.Errorf("record = %+v, want status=failed exit_code=%d source=mpi-bootstrap", rec, c.rc)
	}
	if log, _ := r.read(t, "var/log/spawn-command.log"); !strings.Contains(log, "MPI setup failed (status "+strconv.Itoa(c.rc)) {
		t.Errorf("the failure is not reported in /var/log/spawn-command.log:\n%s", log)
	}
	for _, ip := range []string{peerA, peerB} {
		prec, got := r.record(t, "hosts/"+ip+"/SPAWN_COMPLETE")
		switch {
		case c.peersSignalled && !got:
			t.Errorf("rank 0 failed after the hostfile but never signalled peer %s, which rides to TTL", ip)
		case c.peersSignalled && (prec.Status != "failed" || prec.ExitCode == nil || *prec.ExitCode != c.rc):
			t.Errorf("peer %s record = %+v, want rank 0's failed record, exit_code %d", ip, prec, c.rc)
		case !c.peersSignalled && got:
			t.Errorf("peer %s was signalled although rank 0 had no hostfile or this is not rank 0", ip)
		}
	}
}

func TestMPIFailureWritesAFailedCompletion(t *testing.T) {
	for _, c := range failCases() {
		t.Run(c.name, func(t *testing.T) {
			assertFailedCompletion(t, runSandbox(t, c.opts), c)
		})
	}
}

// TestMPISuccessWritesNoBootstrapRecord: the trap fires only on failure. A peer
// that set up cleanly waits for rank 0's record, and without --mpi-command the
// --command workload writes its own; a record from setup would end both early.
// Also the AL2 half of #1483: the sandbox has no jq, as AL2 has none, and the
// hostfile is still built from the peers file.
func TestMPISuccessWritesNoBootstrapRecord(t *testing.T) {
	testMPISuccess(t, false)
}

func testMPISuccess(t *testing.T, container bool) {
	cohort := []string{rank0IP, peerA, peerB}
	wantHostfile := rank0IP + " slots=1\n" + peerA + " slots=1\n" + peerB + " slots=1\n"
	for _, c := range []struct {
		name string
		cfg  MPIConfig
		self string
		want string // rank 0's record status with --mpi-command; "" = no record
	}{
		{"rank1 with --mpi-command", mpiFailCfg(1, "hostname"), peerA, ""},
		{"rank0 without --mpi-command", mpiFailCfg(0, ""), rank0IP, ""},
		{"rank0 with --mpi-command", mpiFailCfg(0, "hostname"), rank0IP, "completed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := runSandbox(t, sandboxOpts{cfg: c.cfg, self: c.self, peers: cohort, container: container})
			if r.rc != 0 {
				t.Errorf("the member script exited %d on a healthy cohort", r.rc)
			}
			if hf, _ := r.read(t, "tmp/mpi-hostfile"); hf != wantHostfile {
				t.Errorf("hostfile = %q, want %q (one line per peer, from the peers file, without jq)", hf, wantHostfile)
			}
			if g, _ := r.read(t, "run/spawn/mpi-ready"); strings.TrimSpace(g) != "ok" {
				t.Errorf("--command gate = %q, want ok", g)
			}
			rec, ok := r.record(t, "tmp/SPAWN_COMPLETE")
			switch {
			case c.want == "" && ok:
				t.Errorf("a member that set up cleanly wrote %+v; it must wait for the workload's record", rec)
			case c.want != "" && (!ok || rec.Status != c.want || rec.Source != "mpi"):
				t.Errorf("rank 0 record = %+v (exists=%v), want status=%s source=mpi", rec, ok, c.want)
			}
		})
	}
}

// TestMPIUserDataOnAL2Container runs the same scripts in an amazonlinux:2
// container: AL2's bash 4.2, awk, grep and coreutils, and no jq. Skips unless
// docker and the image are present locally (the test never pulls it).
func TestMPIUserDataOnAL2Container(t *testing.T) {
	t.Run("healthy cohort", func(t *testing.T) { testMPISuccess(t, true) })
	for _, c := range failCases() {
		if !strings.HasSuffix(c.name, "/hostfile") && !strings.HasSuffix(c.name, "/mpirun setup") {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			c.opts.container = true
			assertFailedCompletion(t, runSandbox(t, c.opts), c)
		})
	}
}
