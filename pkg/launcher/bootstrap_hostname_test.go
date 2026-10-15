package launcher

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// extractHostnameBlock pulls the hostname-from-Name-tag block out of the real
// bootstrap so the test runs the generated shell, not a copy of it.
func extractHostnameBlock(t *testing.T) string {
	t.Helper()
	script, err := BuildLinuxBootstrap(BootstrapConfig{Username: "ec2-user"})
	if err != nil {
		t.Fatalf("BuildLinuxBootstrap: %v", err)
	}
	const start = "SPAWN_NAME_TAG=$(aws ec2 describe-tags"
	const end = "echo \"✅ Hostname set to $SPAWN_HOSTNAME (from the Name tag)\"\nfi\n"
	i := strings.Index(script, start)
	if i < 0 {
		t.Fatal("the bootstrap does not read the Name tag for the hostname (#897)")
	}
	j := strings.Index(script[i:], end)
	if j < 0 {
		t.Fatal("could not find the end of the hostname block (marker moved?)")
	}
	return script[i : i+j+len(end)]
}

// runHostnameBlock executes the block under set -e (as the bootstrap does)
// with aws/hostnamectl/curl stubbed and /etc redirected to a temp dir. It
// returns the hostname handed to hostnamectl ("" if none) and /etc/hosts.
func runHostnameBlock(t *testing.T, nameTag, arrayID, index string) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()
	block := extractHostnameBlock(t)
	block = strings.ReplaceAll(block, "/etc/hostname", filepath.Join(dir, "hostname"))
	block = strings.ReplaceAll(block, "/etc/hosts", filepath.Join(dir, "hosts"))
	block = strings.ReplaceAll(block, "/etc/cloud/cloud.cfg.d", filepath.Join(dir, "cloud"))
	if err := os.WriteFile(filepath.Join(dir, "hosts"), []byte("127.0.0.1 localhost\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prelude := `set -e
aws() { printf '%s\n' "$NAME_TAG"; }
hostnamectl() { printf '%s' "$2" > "$T/set"; }
curl() { echo 172.31.21.42; }
REGION=us-west-2 INSTANCE_ID=i-0abc TOKEN=t
`
	cmd := exec.Command("bash", "-c", prelude+block) //nolint:gosec // nosemgrep
	cmd.Env = []string{"PATH=/usr/bin:/bin", "T=" + dir, "NAME_TAG=" + nameTag,
		"JOB_ARRAY_ID=" + arrayID, "JOB_ARRAY_INDEX=" + index}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hostname block failed under set -e (this would abort the bootstrap): %v\n%s", err, out)
	}
	set, _ := os.ReadFile(filepath.Join(dir, "set"))
	hosts, _ := os.ReadFile(filepath.Join(dir, "hosts"))
	return string(set), string(hosts)
}

// TestBootstrapSetsHostnameFromName is spawn#897 (2): --name's help says it
// "sets Name tag, DNS, and hostname", and job-array / --mpi members are named
// arr-3, sim-0 — yet `hostname` printed the EC2 default ip-172-31-....
func TestBootstrapSetsHostnameFromName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Linux-only: executes the Linux user-data bootstrap (/etc/hosts) under bash (#1002)")
	}
	cases := []struct {
		desc, tag, arrayID, index, want string
	}{
		{"job-array member", "arr-3", "arr-id", "3", "arr-3"},
		{"mpi member", "sim-0", "sim-id", "0", "sim-0"},
		{"single instance", "mybox", "None", "", "mybox"},
		{"not a valid label", "My_Box.Name", "None", "", "my-box-name"},
		// An --instance-names template without {index} names every member the
		// same; hostnames must stay distinct (MPI tells nodes apart by them).
		{"index-less template", "arr", "arr-id", "2", "arr-2"},
		{"index already present", "worker-12", "w-id", "12", "worker-12"},
		{"no Name tag", "None", "None", "", ""},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			got, hosts := runHostnameBlock(t, c.tag, c.arrayID, c.index)
			if got != c.want {
				t.Fatalf("Name %q: hostname set to %q, want %q", c.tag, got, c.want)
			}
			if c.want == "" {
				if strings.Count(hosts, "\n") != 1 {
					t.Errorf("no hostname to set, but /etc/hosts was changed:\n%s", hosts)
				}
				return
			}
			// It must resolve locally, to the private IP (not 127.x, which
			// would make an MPI daemon advertise loopback for its own name).
			if !strings.Contains(hosts, "172.31.21.42 "+c.want+"\n") {
				t.Errorf("/etc/hosts does not map %s to the private IP:\n%s", c.want, hosts)
			}
		})
	}
}

func TestBootstrapHostnameIsAValidLabel(t *testing.T) {
	long := strings.Repeat("a", 80)
	got, _ := runHostnameBlock(t, long, "arr-id", "7")
	if len(got) > 63 || !strings.HasSuffix(got, "-7") {
		t.Errorf("hostname %q (len %d) is not a <=63-char label keeping the member index", got, len(got))
	}
}

// The hostname must be set before --command starts (the issue's own repro runs
// `hostname` inside --command) and after JOB_ARRAY_INDEX is known (the
// distinct-hostname rule reads it).
func TestBootstrapSetsHostnameBeforeCommandStarts(t *testing.T) {
	script, err := BuildLinuxBootstrap(BootstrapConfig{Username: "ec2-user", Command: "hostname"})
	if err != nil {
		t.Fatalf("BuildLinuxBootstrap: %v", err)
	}
	set := strings.Index(script, `hostnamectl set-hostname "$SPAWN_HOSTNAME"`)
	run := strings.Index(script, `su - "$LOCAL_USERNAME" -c '/tmp/spawn-command.sh'`)
	idx := strings.Index(script, "    JOB_ARRAY_INDEX=$(aws ec2 describe-tags")
	if set < 0 || run < 0 || idx < 0 {
		t.Fatalf("markers missing: hostnamectl=%d command=%d JOB_ARRAY_INDEX=%d", set, run, idx)
	}
	if set > run {
		t.Error("the hostname is set after --command starts, so the workload sees the EC2 default")
	}
	if set < idx {
		t.Error("the hostname is set before JOB_ARRAY_INDEX is read, so array members can collide")
	}
}
