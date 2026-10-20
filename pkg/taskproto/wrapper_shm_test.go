package taskproto

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func shmPtr(v float64) *float64 { return &v }

func shmSpec(shm *float64) *TaskSpec {
	return &TaskSpec{
		TaskID:    "lammps-64",
		Container: "ghcr.io/example/lammps:2024",
		Command:   []string{"mpirun", "-np", "64", "lmp", "-in", "in.rhodo"},
		Resources: ResourceRequest{CPU: 64, MemoryGiB: 128, ShmGiB: shm},
		Lifecycle: Lifecycle{TTL: "2h"},
	}
}

// dockerRunLines returns every emitted `sudo docker run` invocation line.
func dockerRunLines(w string) []string {
	var out []string
	for _, l := range strings.Split(w, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "sudo docker run ") {
			out = append(out, strings.TrimSpace(l))
		}
	}
	return out
}

// #1111: the wrapper passes --shm-size exactly when resources.shm_gib is set,
// and before the image (after it, the flag would be the container's argv).
func TestGenerateWrapper_ShmSizeExactlyWhenSet(t *testing.T) {
	for _, tc := range []struct {
		name string
		shm  *float64
		want string // "" = no --shm-size anywhere
	}{
		{"unset", nil, ""},
		{"whole GiB", shmPtr(32), "--shm-size=32g"},
		{"fractional GiB", shmPtr(0.5), "--shm-size=0.5g"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := shmSpec(tc.shm)
			w := genWrapper(spec, "b", "us-east-1", false, testRunID)
			runs := dockerRunLines(w)
			if len(runs) != 1 {
				t.Fatalf("want exactly one docker run line, got %d:\n%s", len(runs), w)
			}
			run := runs[0]
			if tc.want == "" {
				if strings.Contains(w, "--shm-size") {
					t.Errorf("--shm-size emitted with shm_gib unset:\n%s", run)
				}
				return
			}
			if n := strings.Count(w, "--shm-size"); n != 1 {
				t.Errorf("--shm-size appears %d times in the wrapper, want 1", n)
			}
			flag := strings.Index(run, " "+tc.want+" ")
			img := strings.Index(run, " "+shQuote(spec.Container)+" ")
			if flag < 0 {
				t.Fatalf("docker run line lacks %q:\n%s", tc.want, run)
			}
			if img < 0 || flag > img {
				t.Errorf("%q must precede the image on the docker run line:\n%s", tc.want, run)
			}
		})
	}
}

// The same property, executed: the generated wrapper runs under bash with a
// stand-in docker that records each argv word of every call, and the `run`
// call's argv must carry --shm-size before the image and the user's argv after
// it.
func TestGenerateWrapper_ShmSizeReachesDockerRunArgv(t *testing.T) {
	skipOnWindows(t, "runs the generated Linux wrapper under bash")
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	for _, tc := range []struct {
		name string
		shm  *float64
		want string
	}{
		{"set", shmPtr(16), "--shm-size=16g"},
		{"unset", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := shmSpec(tc.shm)
			w := genWrapper(spec, "results-bucket", "us-east-1", false, testRunID)
			binDir := runtimeStandInBin(t)
			writeFakeExe(t, filepath.Join(binDir, "systemctl"), "#!/bin/bash\nexit 0\n")
			// One file per call, one argv word per line.
			writeFakeExe(t, filepath.Join(binDir, "docker"), "#!/bin/bash\nif [ \"$1\" = run ]; then printf '%s\\n' \"$@\" > \"$DOCKER_RUN\"; fi\nexit 0\n")
			writeFakeSudo(t, binDir)
			runArgv := filepath.Join(t.TempDir(), "run.argv")
			scriptPath := filepath.Join(t.TempDir(), "wrapper.sh")
			if err := os.WriteFile(scriptPath, []byte(w), 0o600); err != nil {
				t.Fatal(err)
			}
			completionPath := CompletionRecordPath()
			_ = os.Remove(completionPath)
			t.Cleanup(func() { _ = os.Remove(completionPath) })
			cmd := exec.Command(bash, scriptPath)
			cmd.Env = []string{"PATH=" + binDir, "HOME=" + t.TempDir(), "DOCKER_RUN=" + runArgv}
			out, _ := cmd.CombinedOutput()
			data, err := os.ReadFile(runArgv)
			if err != nil {
				t.Fatalf("docker run was never called: %v\noutput:\n%s", err, out)
			}
			argv := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
			img, flag := -1, -1
			for i, a := range argv {
				if a == spec.Container && img < 0 {
					img = i
				}
				if strings.HasPrefix(a, "--shm-size") {
					flag = i
					if a != tc.want {
						t.Errorf("argv word %q, want %q", a, tc.want)
					}
				}
			}
			if img < 0 {
				t.Fatalf("image not in docker run argv: %q", argv)
			}
			if got := strings.Join(argv[img+1:], " "); got != strings.Join(spec.Command, " ") {
				t.Errorf("container argv = %q, want the spec's command %q", got, strings.Join(spec.Command, " "))
			}
			switch {
			case tc.want == "" && flag >= 0:
				t.Errorf("--shm-size passed with shm_gib unset: %q", argv)
			case tc.want != "" && flag < 0:
				t.Errorf("--shm-size missing from docker run argv: %q", argv)
			case tc.want != "" && flag > img:
				t.Errorf("--shm-size is after the image, so it is the container's argv: %q", argv)
			}
		})
	}
}
