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
	// stubs replaces or adds stand-ins by name.
	stubs map[string]string
	// setup runs before the script, with the sandbox root.
	setup func(t *testing.T, root string)
	// container runs the script in amazonlinux:2.
	container bool
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
[ "$*" = "true" ] && exit 0
mkdir -p "$STUB_ROOT/hosts/$host"
cmd="$*"
exec bash -c "${cmd//$STUB_ROOT\/tmp\//$STUB_ROOT/hosts/$host/}"
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
		"mpirun": "#!/bin/bash\necho rank-output-from-mpirun\necho rank-stderr >&2\nexit " + strconv.Itoa(o.mpiRC) + "\n",
	}
	if o.noMPI {
		delete(s, "mpirun")
	}
	for k, v := range o.stubs {
		s[k] = v
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

// containerImage is the AL2 image the container runs use. It is never pulled
// by a test: a run needs the image present locally, and skips otherwise.
const containerImage = "amazonlinux:2"

func containerAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	if err := exec.Command("docker", "image", "inspect", containerImage).Run(); err != nil {
		t.Skipf("%s not present locally (docker pull %s to run this)", containerImage, containerImage)
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
	if o.container {
		containerAvailable(t)
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
	if o.setup != nil {
		o.setup(t, root)
	}
	file := filepath.Join(root, "member.sh")
	if err := os.WriteFile(file, []byte("set -e\necho \"sandbox bash $BASH_VERSION\"\n"+relocate(script, root)), 0o755); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + path, "STUB_ROOT=" + root, "STUB_UNREACHABLE=" + o.unreachable, "INSTANCE_ID=i-self", "HOME=" + root}

	var cmd *exec.Cmd
	if o.container {
		args := []string{"run", "--rm", "--network", "none", "-v", root + ":" + root,
			"--user", strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())}
		for _, e := range env {
			args = append(args, "-e", e)
		}
		cmd = exec.Command("docker", append(args, containerImage, "/bin/bash", file)...) //nolint:gosec // test-built argv
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
	if o.container && !strings.Contains(string(out), "sandbox bash 4.2.") {
		t.Fatalf("the container run did not run under AL2's bash 4.2")
	}
	return sandboxRun{mpiRun: mpiRun{root: root}, rc: rc, out: string(out)}
}
