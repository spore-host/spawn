package userdata

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// The MPI sandbox runs a WHOLE rendered member script under bash, the way the
// bootstrap appends and runs it (under set -e), not a slice of it (#1483). Every
// absolute path the script writes is moved under a temp root. PATH holds only
// stand-ins for the tools that act on the cluster (ssh, ssh-keygen, hostname,
// mpirun, yum, sleep) and links to the base tools of a stock MPI base
// (mpiBaseTools), so a tool the script needs and the base lacks, such as jq on
// AL2, fails here as it did on EC2, with "command not found" (127).
//
// container runs the same script in an amazonlinux:2 container instead (bash
// 4.2 and AL2's own awk, grep, coreutils), when one is available locally.

type sandboxOpts struct {
	cfg MPIConfig
	// peers are the cohort's IPs, in rank order, written as the peers file the
	// control plane pushes. nil: no peers file ever arrives.
	peers []string
	// self is this member's address (hostname -I).
	self string
	// mpiRC is the stand-in mpirun's exit status.
	mpiRC int
	// unreachable is a peer IP the stand-in ssh refuses.
	unreachable string
	// noMPI leaves mpirun off PATH, so the script installs it with yum.
	noMPI bool
	// stubs replaces or adds stand-ins by name; "" removes one.
	stubs map[string]string
	// setup runs before the script, with the sandbox root.
	setup func(t *testing.T, root string)
	// container runs the script in amazonlinux:2, or in ubuntu if set.
	container bool
	ubuntu    bool
	// peerScriptAt is, per peer IP, the login at which that peer's own MPI
	// script runs (see sandboxSSH); 0 or absent is the first.
	peerScriptAt map[string]int
	// noPeerScripts runs no peer's MPI script, so no peer is ever ready.
	noPeerScripts bool
	// apt makes apt-get, not yum, this base's package manager (Ubuntu,
	// #1487): its stand-in installs mpirun. ubuntu implies it.
	apt bool
	// noInstanceID leaves INSTANCE_ID unset, as the bootstrap does on a base
	// without ec2-metadata (Ubuntu).
	noInstanceID bool
}

type sandboxRun struct {
	mpiRun
	rc  int
	out string
}

// sandboxTargets are the absolute path prefixes moved under the sandbox root.
var sandboxTargets = []string{"/tmp/", "/var/", "/etc/", "/root/", "/run/"}

// relocate moves every sandboxTargets path under root in one pass, so a root
// that itself starts with one of them (macOS's /var/folders) is not rewritten.
func relocate(script, root string) string {
	var pairs []string
	for _, p := range sandboxTargets {
		pairs = append(pairs, p, root+p)
	}
	return strings.NewReplacer(pairs...).Replace(script)
}

// sandboxSSH runs a command on a member: on this one (STUB_SELF) as given, and
// on a peer with /tmp/ moved to hosts/<ip>/ and /etc/ to hosts/<ip>/etc/. A
// peer accepts SSH at once, but its own MPI script (hosts/<ip>.sh, see
// peerScripts) runs only at its hosts/<ip>.at'th login (default the first),
// before that login's command: the peer's files appear after SSH accepts, as
// on EC2, where rank 0's key reaches a peer over SSM ahead of its bootstrap
// (#1486). Logins are counted, not timed.
const sandboxSSH = `#!/bin/bash
host=""
while [ $# -gt 0 ]; do
  case "$1" in
    -n) shift ;;
    -o) shift 2 ;;
    *) host="$1"; shift; break ;;
  esac
done
echo "$host $*" >> "$STUB_ROOT/ssh.log"
[ "$host" = "$STUB_UNREACHABLE" ] && exit 255
h="$STUB_ROOT/hosts/$host"
cmd="$*"
if [ "$host" != "$STUB_SELF" ]; then
  if [ -f "$h.sh" ]; then
    n=$(($(cat "$h.n" 2>/dev/null || echo 0) + 1))
    echo "$n" > "$h.n"
    if [ "$n" -ge "$(cat "$h.at" 2>/dev/null || echo 1)" ]; then
      bash "$h.sh" > "$h.log" 2>&1 || echo "peer $host: its MPI script failed" >&2
      rm -f "$h.sh"
    fi
  fi
  mkdir -p "$h"
  cmd="${cmd//$STUB_ROOT\/tmp\//$h/}"
  cmd="${cmd//$STUB_ROOT\/etc\//$h/etc/}"
fi
[ "$cmd" = "true" ] && exit 0
exec bash -c "$cmd"
`

// sandboxMpirun stands in for mpirun as far as where the ranks run: it runs
// the program it was given (the words after -hostfile FILE) once per hostfile
// line, as rank N, on that member's files (hosts/<ip>/etc/ for a peer), and
// exits with the first failing rank's status, as Open MPI aborts the job on
// one. Then it prints its marker lines and exits MPIRC if every rank passed.
const sandboxMpirun = `#!/bin/bash
echo "mpirun $*" >> "$STUB_ROOT/mpirun.log"
while [ $# -gt 0 ] && [ "$1" != -hostfile ]; do shift; done
hf=$2
shift 2
n=0 rc=0
for ip in $(awk '{print $1}' "$hf"); do
  prog="$*"
  [ "$ip" = "$STUB_SELF" ] || prog="${prog//$STUB_ROOT\/etc\//$STUB_ROOT/hosts/$ip/etc/}"
  OMPI_COMM_WORLD_RANK=$n bash -c "$prog" || { r=$?; [ "$rc" -ne 0 ] || rc=$r; }
  n=$((n + 1))
done
[ "$rc" -eq 0 ] || exit "$rc"
echo rank-output-from-mpirun
echo rank-stderr >&2
exit $MPIRC
`

func sandboxStubs(o sandboxOpts) map[string]string {
	s := map[string]string{
		"hostname": "#!/bin/bash\necho '" + o.self + " 172.17.0.1'\n",
		"sleep":    "#!/bin/bash\nexit 0\n",
		"ssh":      sandboxSSH,
		"ssh-keygen": `#!/bin/bash
while [ $# -gt 0 ]; do case "$1" in -f) f=$2; shift 2 ;; *) shift ;; esac; done
echo private > "$f"; echo "ssh-rsa AAAAstub spawn-mpi-cohort" > "$f.pub"
`,
		"yum":    "#!/bin/bash\necho \"yum $*\" >> \"$STUB_ROOT/yum.log\"\n",
		"mpirun": sandboxMpirun,
	}
	if o.apt || o.ubuntu {
		delete(s, "yum")
		// Installs the mpirun stand-in, once update and install have both run.
		s["apt-get"] = `#!/bin/bash
echo "apt-get $*" >> "$STUB_ROOT/apt.log"
case " $* " in *" install "*) cat "$STUB_ROOT/mpirun.pkg" > "$(dirname "$0")/mpirun" && chmod +x "$(dirname "$0")/mpirun" ;; esac
`
	}
	if o.noMPI {
		delete(s, "mpirun")
	}
	for k, v := range o.stubs {
		s[k] = v
		if v == "" {
			delete(s, k) // "": this base has no such tool
		}
	}
	return s
}

// sandboxPeers is the peers file as provider.MarshalPeers writes it (an
// indented array of PeerInfo; this package cannot import provider, so
// TestMPIHostfileReadsMarshalPeers in the external test package ties the
// hostfile step to MarshalPeers itself).
func sandboxPeers(t *testing.T, ips []string) []byte {
	t.Helper()
	type peer struct {
		Index      int    `json:"index"`
		InstanceID string `json:"instance_id"`
		IP         string `json:"ip"`
		DNS        string `json:"dns"`
		Provider   string `json:"provider"`
	}
	peers := make([]peer, 0, len(ips))
	for i, ip := range ips {
		peers = append(peers, peer{Index: i, InstanceID: "i-" + strconv.Itoa(i), IP: ip, DNS: "ip-" + strconv.Itoa(i) + ".ec2.internal", Provider: "ec2"})
	}
	b, err := json.MarshalIndent(peers, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ubuntuImage is the Ubuntu base the Ubuntu container runs use (#1487). Never
// pulled by a test either.
const ubuntuImage = "ubuntu:24.04"

// containerImage is the AL2 image the container runs use. It is never pulled
// by a test: a run needs the image present locally, and skips otherwise.
const containerImage = "amazonlinux:2"

func containerAvailable(t *testing.T, image string) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	if err := exec.Command("docker", "image", "inspect", image).Run(); err != nil {
		t.Skipf("%s not present locally (docker pull %s to run this)", image, image)
	}
}

func runSandbox(t *testing.T, o sandboxOpts) sandboxRun {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Linux-only: runs the Linux MPI user data under bash")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	image := containerImage
	if o.ubuntu {
		image = ubuntuImage
	}
	if o.container {
		containerAvailable(t, image)
	}
	if o.cfg.MPIProcessesPerNode == 0 {
		o.cfg.MPIProcessesPerNode = 1
	}
	script, err := GenerateMPIUserData(o.cfg)
	if err != nil {
		t.Fatalf("GenerateMPIUserData: %v", err)
	}

	// Not resolved through symlinks: the script's paths are unquoted, as
	// on an instance, and a resolved TMPDIR can hold a space.
	root := t.TempDir()
	for _, d := range []string{"tmp", "var/log", "etc/spawn", "etc/profile.d", "root", "run", "bin", "base", "hosts"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if o.peers != nil {
		if err := os.WriteFile(filepath.Join(root, "etc/spawn/job-array-peers.json"), sandboxPeers(t, o.peers), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range sandboxStubs(o) {
		if err := os.WriteFile(filepath.Join(root, "bin", name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(root, "bin") + ":/usr/bin:/bin"
	if !o.container {
		// Only the base tools, so the host's own jq or mpirun cannot stand in.
		for tool := range mpiBaseTools {
			if _, stubbed := sandboxStubs(o)[tool]; stubbed {
				continue
			}
			if p, err := exec.LookPath(tool); err == nil {
				if err := os.Symlink(p, filepath.Join(root, "base", tool)); err != nil {
					t.Fatal(err)
				}
			}
		}
		path = filepath.Join(root, "bin") + ":" + filepath.Join(root, "base")
	}
	if o.cfg.JobArrayIndex == 0 && !o.noPeerScripts {
		peerScripts(t, o, root)
	}
	if o.setup != nil {
		o.setup(t, root)
	}
	file := filepath.Join(root, "member.sh")
	if err := os.WriteFile(file, []byte("set -e\necho \"sandbox bash $BASH_VERSION\"\n"+relocate(script, root)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "mpirun.pkg"), []byte(sandboxStubs(sandboxOpts{mpiRC: o.mpiRC})["mpirun"]), 0o755); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + path, "STUB_ROOT=" + root, "STUB_UNREACHABLE=" + o.unreachable, "HOME=" + root,
		"STUB_SELF=" + o.self, "MPIRC=" + strconv.Itoa(o.mpiRC)}
	if !o.noInstanceID {
		env = append(env, "INSTANCE_ID=i-self")
	}

	var cmd *exec.Cmd
	if o.container {
		args := []string{"run", "--rm", "--network", "none", "-v", root + ":" + root,
			"--user", strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())}
		for _, e := range env {
			args = append(args, "-e", e)
		}
		cmd = exec.Command("docker", append(args, image, "/bin/bash", file)...) //nolint:gosec // test-built argv
	} else {
		cmd = exec.Command(bash, file) //nolint:gosec // the sandbox's own script
		cmd.Env = env
	}
	out, err := cmd.CombinedOutput()
	rc := 0
	if ee, ok := err.(*exec.ExitError); ok {
		rc = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running the member script: %v\n%s", err, out)
	}
	t.Logf("member script (rank %d) exited %d:\n%s", o.cfg.JobArrayIndex, rc, out)
	if o.container && !o.ubuntu && !strings.Contains(string(out), "sandbox bash 4.2.") {
		t.Fatalf("the container run did not run under AL2's bash 4.2")
	}
	return sandboxRun{mpiRun: mpiRun{root: root}, rc: rc, out: string(out)}
}

// peerScripts writes, for rank 0's run, each other member's own rendered MPI
// script (its rank, the same cohort) relocated under hosts/<ip>/, with that
// member's copy of the peers file, for sandboxSSH to run. A peer then writes
// its files, and its readiness marker, the way its own user data does.
func peerScripts(t *testing.T, o sandboxOpts, root string) {
	t.Helper()
	for i, ip := range o.peers {
		if ip == o.self {
			continue
		}
		cfg := o.cfg
		cfg.JobArrayIndex = i
		script, err := GenerateMPIUserData(cfg)
		if err != nil {
			t.Fatalf("GenerateMPIUserData(rank %d): %v", i, err)
		}
		h := filepath.Join(root, "hosts", ip)
		for _, d := range []string{"tmp", "var/log", "etc/spawn", "etc/profile.d", "root", "run"} {
			if err := os.MkdirAll(filepath.Join(h, d), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(h, "etc/spawn/job-array-peers.json"), sandboxPeers(t, o.peers), 0o644); err != nil {
			t.Fatal(err)
		}
		body := "set -e\nexport INSTANCE_ID=i-" + strconv.Itoa(i) + " HOME=" + h + "\n" + relocate(script, h)
		if err := os.WriteFile(h+".sh", []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if at := o.peerScriptAt[ip]; at > 0 {
			if err := os.WriteFile(h+".at", []byte(strconv.Itoa(at)), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}
