package tasklaunch

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"io"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/launcher"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/taskproto"
)

// #1231: container_docker_socket puts the socket mount on the docker run line
// of the user data the launch sends, costs it little, and a container spec using it stays under EC2's
// 16 KiB limit. Measured through the real launch assembly, not the wrapper.
func TestDockerSocketUserDataCost(t *testing.T) {
	spec := func(sock bool) *taskproto.TaskSpec {
		return &taskproto.TaskSpec{
			TaskID:                "cromwell-hc",
			Container:             "broadinstitute/cromwell:86",
			Command:               []string{"java", "-jar", "/app/cromwell.jar", "run", "/work/hc.wdl"},
			Resources:             taskproto.ResourceRequest{CPU: 2, MemoryGiB: 4},
			Inputs:                []taskproto.Manifest{{Source: "s3://b/hc.wdl", Destination: "/work/hc.wdl"}},
			Outputs:               []taskproto.Manifest{{Source: "/work/out/", Destination: "s3://b/out/"}},
			Lifecycle:             taskproto.Lifecycle{TTL: "1h", OnComplete: "terminate"},
			ContainerDockerSocket: sock,
		}
	}
	without, with := measuredUserData(t, spec(false)), measuredUserData(t, spec(true))
	t.Logf("user data (gzip): %d bytes without the socket, %d with", without, with)
	for _, sock := range []bool{false, true} {
		if got := dockerRunHasSocket(t, spec(sock)); got != sock {
			t.Errorf("container_docker_socket=%v: socket mount on the user data's docker run line = %v", sock, got)
		}
	}
	if with-without > 200 {
		t.Errorf("container_docker_socket costs %d bytes of user data, want <= 200", with-without)
	}
	if with > aws.MaxUserDataBytes {
		t.Errorf("a container spec with the socket is %d bytes, over the %d limit", with, aws.MaxUserDataBytes)
	}
}

// dockerRunHasSocket decodes the user data the launch would send and reports
// whether a `docker run` line in it mounts the host Docker socket.
func dockerRunHasSocket(t *testing.T, spec *taskproto.TaskSpec) bool {
	t.Helper()
	cfg, opts, err := buildTaskLaunch(spec, userDataTestSized, "us-east-1", preflightProfile,
		taskproto.EffectiveResultsPrefix(spec, preflightAccountID, "us-east-1"), preflightRunID, "")
	if err != nil {
		t.Fatalf("buildTaskLaunch: %v", err)
	}
	enc, err := launcher.BuildUserData(cfg, opts)
	if err != nil {
		t.Fatalf("BuildUserData: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	text, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(text), "\n") {
		if strings.Contains(l, "docker run ") && strings.Contains(l, " -v /var/run/docker.sock:/var/run/docker.sock ") {
			return true
		}
	}
	return false
}
