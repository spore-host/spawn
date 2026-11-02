package userdata_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/provider"
	"github.com/spore-host/spawn/pkg/userdata"
)

// TestMPIHostfileReadsMarshalPeers ties the MPI user data's hostfile step, which
// reads the peers file without jq (#1483), to the peers file's one writer,
// provider.MarshalPeers, and to its compact form too, so a change of
// indentation cannot silently empty the hostfile. An empty hostfile fails the
// --command gate and leaves mpirun with no hosts. (External test package:
// provider imports userdata.)
func TestMPIHostfileReadsMarshalPeers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Linux-only: runs the Linux MPI user data under bash")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	script, err := userdata.GenerateMPIUserData(userdata.MPIConfig{
		Region: "us-east-1", JobArrayID: "a", JobArrayIndex: 0, JobArraySize: 3, MPIProcessesPerNode: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The step: the SLOTS assignment and every line that writes the hostfile.
	var step []string
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(line, "SLOTS=") || strings.HasSuffix(line, "> /tmp/mpi-hostfile") {
			step = append(step, line)
		}
	}
	if len(step) != 2 {
		t.Fatalf("want the SLOTS line and one hostfile write in the MPI user data, found %q", step)
	}

	peers := []provider.PeerInfo{
		{Index: 0, InstanceID: "i-0", IP: "10.0.0.1", DNS: "ip-10-0-0-1.ec2.internal", Provider: "ec2"},
		{Index: 1, InstanceID: "i-1", IP: "10.0.0.2", DNS: "ip", Provider: "ec2"},
		{Index: 2, InstanceID: "i-2", IP: "10.0.0.3", DNS: "", Provider: "local"},
	}
	indented, err := provider.MarshalPeers(peers)
	if err != nil {
		t.Fatal(err)
	}
	compact, err := json.Marshal(peers)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := provider.MarshalPeers(nil)
	if err != nil {
		t.Fatal(err)
	}
	const want = "10.0.0.1 slots=4\n10.0.0.2 slots=4\n10.0.0.3 slots=4\n"
	for name, tc := range map[string]struct {
		file []byte
		want string
	}{
		"MarshalPeers": {indented, want},
		"compact":      {compact, want},
		"no peers":     {empty, ""},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			peersFile, hostfile := filepath.Join(dir, "peers.json"), filepath.Join(dir, "hostfile")
			if err := os.WriteFile(peersFile, tc.file, 0o644); err != nil {
				t.Fatal(err)
			}
			body := strings.NewReplacer(provider.JobArrayPeersFile, peersFile, "/tmp/mpi-hostfile", hostfile).
				Replace(strings.Join(step, "\n"))
			cmd := exec.Command(bash, "-e", "-c", body) //nolint:gosec // the rendered step
			cmd.Env = []string{"PATH=/usr/bin:/bin"}
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("hostfile step failed: %v\n%s\n%s", err, body, out)
			}
			got, _ := os.ReadFile(hostfile)
			if string(got) != tc.want {
				t.Errorf("hostfile = %q, want %q", got, tc.want)
			}
		})
	}
}
