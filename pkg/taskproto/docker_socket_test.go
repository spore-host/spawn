package taskproto

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The exact words #1231 asks for: the host socket at the same path, and the
// socket's group resolved on the instance at run time.
const (
	dockerSockMount = "-v /var/run/docker.sock:/var/run/docker.sock"
	dockerSockGroup = `--group-add "$(stat -c %g /var/run/docker.sock)"`
)

func dockerSocketSpec(sock bool) *TaskSpec {
	return &TaskSpec{
		TaskID:                "cromwell-hc",
		Container:             "broadinstitute/cromwell:86",
		Command:               []string{"java", "-jar", "/app/cromwell.jar", "run", "/work/hc.wdl"},
		Resources:             ResourceRequest{CPU: 16, MemoryGiB: 64},
		Inputs:                []Manifest{{Source: "s3://b/hc.wdl", Destination: "/work/hc.wdl"}},
		Outputs:               []Manifest{{Source: "/work/out/", Destination: "s3://b/out/"}},
		Lifecycle:             Lifecycle{TTL: "4h"},
		ContainerDockerSocket: sock,
	}
}

// #1231: the socket mount and its group are on the docker run line exactly when
// container_docker_socket is set, before the image (after it they would be the
// container's argv), and --user and the /tmp tmpfs (#1270) are kept either way.
func TestGenerateWrapper_DockerSocketExactlyWhenSet(t *testing.T) {
	for _, sock := range []bool{false, true} {
		spec := dockerSocketSpec(sock)
		w := genWrapper(spec, "b", "us-east-1", false, testRunID)
		runs := dockerRunLines(w)
		if len(runs) != 1 {
			t.Fatalf("sock=%v: want exactly one docker run line, got %d:\n%s", sock, len(runs), w)
		}
		run := runs[0]
		for _, keep := range []string{`--user "$(id -u):$(id -g)"`, "--tmpfs "} {
			if !strings.Contains(run, keep) {
				t.Errorf("sock=%v: docker run lost %q:\n%s", sock, keep, run)
			}
		}
		if !sock {
			if strings.Contains(w, "docker.sock") || strings.Contains(w, "--group-add") {
				t.Errorf("socket or group emitted with container_docker_socket unset:\n%s", run)
			}
			continue
		}
		img := strings.Index(run, " "+shQuote(spec.Container)+" ")
		for _, want := range []string{dockerSockMount, dockerSockGroup} {
			i := strings.Index(run, " "+want+" ")
			if i < 0 {
				t.Errorf("docker run line lacks %q:\n%s", want, run)
			} else if img < 0 || i > img {
				t.Errorf("%q must precede the image on the docker run line:\n%s", want, run)
			}
		}
	}
}

// The same property, executed: the generated wrapper runs under bash with a
// stand-in docker that records the `run` argv and a stand-in stat that answers
// for the socket, so the group is the socket's gid as one argv word.
func TestGenerateWrapper_DockerSocketReachesDockerRunArgv(t *testing.T) {
	skipOnWindows(t, "runs the generated Linux wrapper under bash")
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	for _, sock := range []bool{true, false} {
		spec := dockerSocketSpec(sock)
		spec.Inputs, spec.Outputs = nil, nil // no aws staging needed for this property
		w := genWrapper(spec, "results-bucket", "us-east-1", false, testRunID)
		binDir := runtimeStandInBin(t)
		writeFakeExe(t, filepath.Join(binDir, "systemctl"), "#!/bin/bash\nexit 0\n")
		writeFakeExe(t, filepath.Join(binDir, "docker"), "#!/bin/bash\nif [ \"$1\" = run ]; then printf '%s\\n' \"$@\" > \"$DOCKER_RUN\"; fi\nexit 0\n")
		writeFakeExe(t, filepath.Join(binDir, "stat"), "#!/bin/bash\n[ \"$*\" = '-c %g /var/run/docker.sock' ] && echo 992 && exit 0\nexit 1\n")
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
			t.Fatalf("sock=%v: docker run was never called: %v\noutput:\n%s", sock, err, out)
		}
		argv := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		img := -1
		for i, a := range argv {
			if a == spec.Container {
				img = i
				break
			}
		}
		if img < 0 {
			t.Fatalf("sock=%v: image not in docker run argv: %q", sock, argv)
		}
		pre := " " + strings.Join(argv[:img], " ") + " "
		has := strings.Contains(pre, " -v /var/run/docker.sock:/var/run/docker.sock ") && strings.Contains(pre, " --group-add 992 ")
		if has != sock {
			t.Errorf("sock=%v: socket mount + --group-add 992 before the image = %v; argv %q", sock, has, argv)
		}
		if !sock && strings.Contains(pre, "--group-add") {
			t.Errorf("--group-add passed with container_docker_socket unset: %q", argv)
		}
	}
}

func TestValidate_DockerSocketNeedsAContainer(t *testing.T) {
	spec := dockerSocketSpec(true)
	if err := spec.Validate(); err != nil {
		t.Fatalf("container task with the socket must validate: %v", err)
	}
	spec.Container = ""
	err := spec.Validate()
	if err == nil || !strings.Contains(err.Error(), "container_docker_socket applies only to a container task") {
		t.Fatalf("host task with container_docker_socket: err = %v, want the container-only refusal", err)
	}
}

// The field is spelled container_docker_socket on the wire, and an unset field
// is omitted, so every existing spec marshals as before.
func TestParseSpec_DockerSocketWireName(t *testing.T) {
	spec, err := ParseSpec([]byte(`{"task_id":"t","command":["x"],"container":"img","container_docker_socket":true,"resources":{},"lifecycle":{"ttl":"1h"}}`))
	if err != nil || !spec.ContainerDockerSocket {
		t.Fatalf("ParseSpec = %+v, %v; want container_docker_socket parsed", spec, err)
	}
	if strings.Contains(provenanceSpecJSON(dockerSocketSpec(false)), "docker_socket") {
		t.Error("an unset container_docker_socket must be omitted from the marshalled spec")
	}
	// spored reads the provenance recipe's docker_socket from this copy.
	if !strings.Contains(provenanceSpecJSON(dockerSocketSpec(true)), `"container_docker_socket":true`) {
		t.Error("the provenance spec copy must carry container_docker_socket")
	}
}

// A pooled task shares its worker with other tasks, so the root-equivalent
// socket is refused there (#1231): by ValidatePooled, which every pooled entry
// point calls, and by GeneratePooledJobScript itself, so no pooled job script
// can ever carry the mount.
func TestPooled_DockerSocketRefused(t *testing.T) {
	spec := dockerSocketSpec(true)
	err := spec.ValidatePooled()
	if err == nil || !strings.Contains(err.Error(), "not allowed for a pooled task") {
		t.Fatalf("ValidatePooled = %v, want the pooled refusal", err)
	}
	if err := dockerSocketSpec(false).ValidatePooled(); err != nil {
		t.Errorf("ValidatePooled refused a spec without the socket: %v", err)
	}
	opts := WrapperOptions{ResultsPrefix: "s3://b/tasks", Region: "us-east-1", RunID: testRunID}
	script, err := GeneratePooledJobScript(spec, opts)
	if err == nil || strings.Contains(script, "docker.sock") {
		t.Fatalf("GeneratePooledJobScript with the socket = (%d bytes, %v), want a refusal and no script", len(script), err)
	}
	if _, err := GeneratePooledJobScript(dockerSocketSpec(false), opts); err != nil {
		t.Errorf("GeneratePooledJobScript without the socket: %v", err)
	}
	// The one-instance path still gets it: the refusal is pool-only.
	w, err := GenerateWrapper(spec, opts)
	if err != nil || !strings.Contains(w, dockerSockMount) {
		t.Errorf("GenerateWrapper with the socket: err %v, mount present %v", err, strings.Contains(w, dockerSockMount))
	}
}
