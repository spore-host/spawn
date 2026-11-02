package userdata

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The #1487 gates. The MPI user data installed Open MPI only with yum, so on an
// Ubuntu AMI without --skip-mpi-install --mpi could never work: since #1485 it
// failed closed with 'yum: command not found' on every member. On a base with
// apt-get it now installs Ubuntu's Open MPI (openmpi-bin for mpirun and orted,
// libopenmpi-dev for mpicc, as openmpi-devel on AL), and on a base with
// neither it fails closed with a message naming --skip-mpi-install.

// aptCohort runs rank 0, which must install Open MPI, of a healthy 3-member
// cohort whose package manager is apt-get.
func aptCohort(t *testing.T, o sandboxOpts) sandboxRun {
	t.Helper()
	o.cfg = MPIConfig{Region: "us-east-1", JobArrayID: "arr-1", JobArrayIndex: 0, JobArraySize: 3, MPICommand: "hostname"}
	o.peers, o.self, o.noMPI, o.apt = []string{rank0IP, peerA, peerB}, rank0IP, true, true
	return runSandbox(t, o)
}

func aptCalls(t *testing.T, r sandboxRun, verb string) int {
	t.Helper()
	log, _ := r.read(t, "apt.log")
	n := 0
	for _, l := range strings.Split(log, "\n") {
		if strings.HasPrefix(l, "apt-get ") && strings.Contains(l+" ", " "+verb+" ") {
			n++
		}
	}
	return n
}

func TestMPIInstallsOpenMPIWithApt(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    sandboxOpts
	}{
		{"sandbox", sandboxOpts{}},
		// Ubuntu's own bash, awk (mawk), grep and coreutils, and no yum.
		// Skips unless docker and ubuntu:24.04 are present locally.
		{"ubuntu:24.04 container", sandboxOpts{container: true, ubuntu: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := aptCohort(t, tc.o)
			if r.rc != 0 {
				t.Fatalf("rank 0 exited %d on an apt-based member; --mpi must install Open MPI there (#1487)", r.rc)
			}
			log, _ := r.read(t, "apt.log")
			lines := strings.Split(strings.TrimSpace(log), "\n")
			if len(lines) != 2 || !strings.HasPrefix(lines[0], "apt-get update") {
				t.Fatalf("apt-get calls = %q, want an update then one install", lines)
			}
			for _, w := range []string{" install ", " -y ", " openmpi-bin", " libopenmpi-dev"} {
				if !strings.Contains(lines[1]+" ", w) {
					t.Errorf("the install %q does not have %q", lines[1], strings.TrimSpace(w))
				}
			}
			if _, ok := r.read(t, "yum.log"); ok {
				t.Error("yum ran on a base without yum")
			}
			if hf, _ := r.read(t, "tmp/mpi-hostfile"); hf != rank0IP+" slots=1\n"+peerA+" slots=1\n"+peerB+" slots=1\n" {
				t.Errorf("hostfile = %q", hf)
			}
			if rec, ok := r.record(t, "tmp/SPAWN_COMPLETE"); !ok || rec.Status != "completed" || rec.Source != "mpi" {
				t.Errorf("rank 0 record = %+v (exists=%v), want completed from mpi", rec, ok)
			}
		})
	}
}

// failingApt fails 'apt-get update' (a held lists lock, which
// DPkg::Lock::Timeout does not cover) for its first $APT_FAILS calls, counted.
const failingApt = `#!/bin/bash
echo "apt-get $*" >> "$STUB_ROOT/apt.log"
case " $* " in
  *" update "*)
    n=$(($(cat "$STUB_ROOT/apt.n" 2>/dev/null || echo 0) + 1)); echo "$n" > "$STUB_ROOT/apt.n"
    [ "$n" -gt FAILS ] || { echo "E: Could not get lock /var/lib/apt/lists/lock" >&2; exit 100; } ;;
  *" install "*) cat "$STUB_ROOT/mpirun.pkg" > "$(dirname "$0")/mpirun" && chmod +x "$(dirname "$0")/mpirun" ;;
esac
`

func TestMPIAptRetriesABusyLock(t *testing.T) {
	r := aptCohort(t, sandboxOpts{stubs: map[string]string{"apt-get": strings.Replace(failingApt, "FAILS", "2", 1)}})
	if r.rc != 0 {
		t.Fatalf("rank 0 exited %d although apt's lock was free on the third try", r.rc)
	}
	if u, i := aptCalls(t, r, "update"), aptCalls(t, r, "install"); u != 3 || i != 1 {
		t.Errorf("apt-get update ran %d times and install %d, want 3 and 1", u, i)
	}
}

func TestMPIAptGivesUpAndFailsClosed(t *testing.T) {
	r := aptCohort(t, sandboxOpts{stubs: map[string]string{"apt-get": strings.Replace(failingApt, "FAILS", "1000", 1)}})
	if u := aptCalls(t, r, "update"); u != 10 {
		t.Errorf("apt-get update ran %d times, want the bound of 10", u)
	}
	assertSetupFailed(t, r, 1)
}

func TestMPINoPackageManagerFailsClosed(t *testing.T) {
	r := aptCohort(t, sandboxOpts{stubs: map[string]string{"apt-get": ""}})
	assertSetupFailed(t, r, 1)
	if log, _ := r.read(t, "var/log/spawn-command.log"); !strings.Contains(log, "--skip-mpi-install") {
		t.Errorf("a base with no package manager is not told about --skip-mpi-install:\n%s", log)
	}
}

func assertSetupFailed(t *testing.T, r sandboxRun, rc int) {
	t.Helper()
	if r.rc != rc {
		t.Errorf("rank 0 exited %d, want %d", r.rc, rc)
	}
	rec, ok := r.record(t, "tmp/SPAWN_COMPLETE")
	if !ok || rec.Status != "failed" || rec.Source != "mpi-bootstrap" || rec.ExitCode == nil || *rec.ExitCode != rc {
		t.Errorf("rank 0 record = %+v (exists=%v), want failed from mpi-bootstrap, exit %d", rec, ok, rc)
	}
	if calls, _ := r.read(t, "mpirun.log"); calls != "" {
		t.Errorf("mpirun ran without Open MPI installed: %s", calls)
	}
}

// The bootstrap reads INSTANCE_ID with ec2-metadata, which Ubuntu does not
// ship, so on Ubuntu it is empty and rank 0 recorded an empty key owner.
// pkg/mpicohort reads rank 0's public key only once the owner file names the
// instance (#1338), so the cohort could never assemble. The script reads the
// ID from IMDS itself when INSTANCE_ID is empty.
func TestMPIKeyOwnerIsTheInstanceWithoutEC2Metadata(t *testing.T) {
	const imds = `#!/bin/bash
case " $* " in
  *" -X PUT "*api/token*) echo tok ;;
  *"X-aws-ec2-metadata-token: tok"*meta-data/instance-id*) echo i-0abc123 ;;
  *) exit 22 ;;
esac
`
	r := aptCohort(t, sandboxOpts{noInstanceID: true, stubs: map[string]string{"curl": imds}})
	if r.rc != 0 {
		t.Fatalf("rank 0 exited %d", r.rc)
	}
	if owner, _ := r.read(t, "root/.ssh/spawn-mpi-key-owner"); strings.TrimSpace(owner) != "i-0abc123" {
		t.Errorf("key owner = %q, want the instance's own ID from IMDS; pkg/mpicohort never hands out a key whose owner is not the instance", owner)
	}
}

// mpi.sh must not put the current directory on the library path: with
// LD_LIBRARY_PATH unset, 'lib:$LD_LIBRARY_PATH' ends in an empty entry, which
// the loader reads as ".", for every root shell that sources it.
func TestMPIProfileAddsNoEmptyLibraryPathEntry(t *testing.T) {
	r := aptCohort(t, sandboxOpts{})
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	out, err := exec.Command(bash, "-c", `unset LD_LIBRARY_PATH; . "$1"; printf %s "$LD_LIBRARY_PATH"`, "-", //nolint:gosec // test-built argv
		filepath.Join(r.root, "etc/profile.d/mpi.sh")).Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range strings.Split(string(out), ":") {
		if e == "" {
			t.Fatalf("mpi.sh sets LD_LIBRARY_PATH=%q, which has an empty entry (the current directory)", out)
		}
	}
}
