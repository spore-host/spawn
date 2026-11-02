package userdata

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The #1491 gates (owner decision): with --efa, rank 0's mpirun is the EFA
// installer's Open MPI, /opt/amazon/openmpi/bin/mpirun, the one AWS documents
// for EFA, and the distribution's Open MPI is not installed. Without --efa
// nothing changes: the distribution's mpirun runs even when an
// /opt/amazon/openmpi exists on the image.
//
// The sandbox runs the whole rendered member scripts (rank 0 and its peers)
// with an mpirun stand-in in each place: the distribution's on PATH, and the
// one the stand-in EFA installer puts under /opt/amazon/openmpi/bin. Each
// records which it is, so the test asserts which one the script actually ran,
// not what the script text says.

const (
	distroMarker = "distro"
	efaMarker    = "efa"
)

// markedMpirun is the sandbox's mpirun stand-in, recording marker first.
func markedMpirun(marker string) string {
	return "#!/bin/bash\necho " + marker + " >> \"$STUB_ROOT/which-mpirun\"\n" + strings.TrimPrefix(sandboxMpirun, "#!/bin/bash\n")
}

// efaInstallerTar stands in for tar -xf aws-efa-installer-latest.tar.gz: it
// unpacks an efa_installer.sh that, run from <host root>/tmp/aws-efa-installer,
// installs the marked mpirun at <host root>/opt/amazon/openmpi/bin, as the
// real installer does at /opt/amazon/openmpi; or, with openmpi=false, installs
// nothing there (an installer run with --minimal or --mpi=none).
//
// On an apt-get base it fails as the real installer did on a stock Ubuntu
// AMI (live, spawn#1491) unless 'apt-get update' ran first: it apt-installs
// its dependencies and the image's indexes are stale.
func efaInstallerTar(openmpi bool) string {
	const staleApt = `if command -v apt-get >/dev/null && ! grep -q "^apt-get update" "$STUB_ROOT/apt.log" 2>/dev/null; then echo "E: Unable to locate package environment-modules" >&2; exit 1; fi`
	install := ""
	if openmpi {
		install = `d="$(cd ../.. && pwd)/opt/amazon/openmpi/bin"; mkdir -p "$d"; cat "$STUB_ROOT/efa-mpirun" > "$d/mpirun"; chmod +x "$d/mpirun"`
	}
	return `#!/bin/bash
mkdir -p aws-efa-installer
printf '#!/bin/bash\necho "efa_installer.sh $*" >> "$STUB_ROOT/efa.log"\n%s\n%s\n' '` + staleApt + `' '` + install + `' > aws-efa-installer/efa_installer.sh
chmod +x aws-efa-installer/efa_installer.sh
`
}

// efaCohort runs rank 0 of a healthy 3-member --mpi-command cohort.
func efaCohort(t *testing.T, efa bool, o sandboxOpts) sandboxRun {
	t.Helper()
	o.cfg = MPIConfig{Region: "us-east-1", JobArrayID: "arr-1", JobArrayIndex: 0, JobArraySize: 3, MPICommand: "hostname", EFAEnabled: efa}
	o.peers, o.self = []string{rank0IP, peerA, peerB}, rank0IP
	if o.stubs == nil {
		o.stubs = map[string]string{}
	}
	if _, ok := o.stubs["tar"]; !ok {
		o.stubs["tar"] = efaInstallerTar(true)
	}
	o.stubs["curl"] = "#!/bin/bash\necho \"curl $*\" >> \"$STUB_ROOT/curl.log\"\n: > aws-efa-installer-latest.tar.gz\n"
	if _, ok := o.stubs["mpirun"]; !ok && !o.noMPI {
		o.stubs["mpirun"] = markedMpirun(distroMarker)
	}
	prev := o.setup
	o.setup = func(t *testing.T, root string) {
		if err := os.WriteFile(filepath.Join(root, "efa-mpirun"), []byte(markedMpirun(efaMarker)), 0o755); err != nil {
			t.Fatal(err)
		}
		if prev != nil {
			prev(t, root)
		}
	}
	return runSandbox(t, o)
}

func whichMpirun(t *testing.T, r sandboxRun) string {
	t.Helper()
	got, _ := r.read(t, "which-mpirun")
	return strings.TrimSpace(got)
}

func distroInstalls(t *testing.T, r sandboxRun) []string {
	t.Helper()
	var out []string
	for _, f := range []string{"yum.log", "apt.log"} {
		log, _ := r.read(t, f)
		for _, l := range strings.Split(log, "\n") {
			if strings.Contains(l, " install ") && strings.Contains(l, "openmpi") {
				out = append(out, l)
			}
		}
	}
	return out
}

func TestMPIWithEFARunsTheInstallersOpenMPI(t *testing.T) {
	for name, o := range map[string]sandboxOpts{
		"an image that has the distribution's mpirun": {},
		"yum, no mpirun":     {noMPI: true},
		"apt-get, no mpirun": {noMPI: true, apt: true},
	} {
		t.Run(name, func(t *testing.T) {
			r := efaCohort(t, true, o)
			if r.rc != 0 {
				t.Fatalf("rank 0 exited %d with --efa", r.rc)
			}
			if got := whichMpirun(t, r); got != efaMarker {
				t.Errorf("with --efa, rank 0 ran mpirun %q, want the EFA installer's /opt/amazon/openmpi/bin/mpirun (#1491)", got)
			}
			if in := distroInstalls(t, r); len(in) != 0 {
				t.Errorf("with --efa, the distribution's Open MPI was installed: %q", in)
			}
			if rec, ok := r.record(t, "tmp/SPAWN_COMPLETE"); !ok || rec.Status != "completed" || rec.Source != "mpi" {
				t.Errorf("rank 0 record = %+v (exists=%v), want completed from mpi", rec, ok)
			}
			// mpi.sh, read by login shells and sourced here: the installer's
			// Open MPI and libfabric tools first, its libraries first, and no
			// empty LD_LIBRARY_PATH entry (#1487).
			sh, _ := r.read(t, "etc/profile.d/mpi.sh")
			for _, want := range []string{
				"export PATH=/opt/amazon/openmpi/bin:/opt/amazon/efa/bin:$PATH\n",
				"export LD_LIBRARY_PATH=/opt/amazon/openmpi/lib64:/opt/amazon/openmpi/lib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}\n",
			} {
				if !strings.Contains(sh, strings.ReplaceAll(want, "/opt/", r.root+"/opt/")) {
					t.Errorf("mpi.sh lacks %q:\n%s", want, sh)
				}
			}
			if strings.Contains(sh, "/usr/lib64/openmpi") {
				t.Errorf("with --efa, mpi.sh still puts the distribution's Open MPI on PATH:\n%s", sh)
			}
		})
	}
}

// Without --efa nothing changes, even on an image that has the EFA stack.
func TestMPIWithoutEFARunsTheDistributionsOpenMPI(t *testing.T) {
	r := efaCohort(t, false, sandboxOpts{setup: func(t *testing.T, root string) {
		d := filepath.Join(root, "opt", "amazon", "openmpi", "bin")
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "mpirun"), []byte(markedMpirun(efaMarker)), 0o755); err != nil {
			t.Fatal(err)
		}
	}})
	if r.rc != 0 {
		t.Fatalf("rank 0 exited %d without --efa", r.rc)
	}
	if got := whichMpirun(t, r); got != distroMarker {
		t.Errorf("without --efa, rank 0 ran mpirun %q, want the distribution's", got)
	}
	if log, ok := r.read(t, "efa.log"); ok {
		t.Errorf("without --efa the EFA installer ran: %q", log)
	}
	sh, _ := r.read(t, "etc/profile.d/mpi.sh")
	if strings.Contains(sh, "/opt/amazon") {
		t.Errorf("without --efa, mpi.sh names /opt/amazon:\n%s", sh)
	}
	for _, want := range []string{
		"export PATH=/usr/lib64/openmpi/bin:$PATH\n",
		"export LD_LIBRARY_PATH=/usr/lib64/openmpi/lib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}\n",
	} {
		if !strings.Contains(sh, want) {
			t.Errorf("without --efa, mpi.sh lacks %q:\n%s", want, sh)
		}
	}
}

// An EFA installer that leaves no /opt/amazon/openmpi must fail the member
// closed, not let PATH fall through to the distribution's mpirun.
func TestMPIWithEFAFailsClosedWithoutTheInstallersOpenMPI(t *testing.T) {
	r := efaCohort(t, true, sandboxOpts{stubs: map[string]string{"tar": efaInstallerTar(false)}})
	assertSetupFailed(t, r, 1)
	if got := whichMpirun(t, r); got != "" {
		t.Errorf("mpirun %q ran although the EFA installer installed no Open MPI", got)
	}
	if log, _ := r.read(t, "var/log/spawn-command.log"); !strings.Contains(log, "no Open MPI at "+r.root+"/opt/amazon/openmpi after the EFA installer") {
		t.Errorf("the failure does not name the missing Open MPI:\n%s", log)
	}
}

// On Ubuntu the installer needs fresh apt indexes, and with --efa the Open
// MPI install that refreshed them is skipped: every Ubuntu --efa member
// failed closed in the installer (live, spawn#1491). The update must run, and
// a busy lock is retried with the same bound as the Open MPI install.
func TestMPIWithEFARefreshesAptIndexes(t *testing.T) {
	r := efaCohort(t, true, sandboxOpts{noMPI: true, apt: true, stubs: map[string]string{"apt-get": strings.Replace(failingApt, "FAILS", "2", 1)}})
	if r.rc != 0 {
		t.Fatalf("rank 0 exited %d with --efa on an apt base", r.rc)
	}
	// The sandbox runs all 3 members against one stub counter: rank 0's two
	// busy locks, then one successful update per member.
	if u := aptCalls(t, r, "update"); u != 5 {
		t.Errorf("apt-get update ran %d times, want 5 (two busy locks, then one per member)", u)
	}
	if log, _ := r.read(t, "efa.log"); !strings.Contains(log, "efa_installer.sh -y -g") {
		t.Errorf("the EFA installer did not run: %q", log)
	}
}

func TestMPIWithEFAAptGivesUpAndFailsClosed(t *testing.T) {
	r := efaCohort(t, true, sandboxOpts{noMPI: true, apt: true, stubs: map[string]string{"apt-get": strings.Replace(failingApt, "FAILS", "1000", 1)}})
	if u := aptCalls(t, r, "update"); u != 10 {
		t.Errorf("apt-get update ran %d times, want the bound of 10", u)
	}
	if log, ok := r.read(t, "efa.log"); ok {
		t.Errorf("the EFA installer ran without fresh indexes: %q", log)
	}
	assertSetupFailed(t, r, 1)
}
