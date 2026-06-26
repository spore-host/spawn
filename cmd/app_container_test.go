package cmd

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestContainerInitCommand(t *testing.T) {
	tests := []struct {
		name      string
		image     string
		gpu       bool
		wantParts []string
		notWant   string
	}{
		{
			name:      "gpu app gets --gpus all",
			image:     "public.ecr.aws/spore-host/paraview:5.13.2",
			gpu:       true,
			wantParts: []string{"docker run", "--rm", "--gpus all", "-e DISPLAY=:0", "/tmp/.X11-unix:/tmp/.X11-unix", "public.ecr.aws/spore-host/paraview:5.13.2"},
		},
		{
			name:      "cpu app omits --gpus",
			image:     "public.ecr.aws/spore-host/igv:2.17.4",
			gpu:       false,
			wantParts: []string{"docker run", "--rm", "-e DISPLAY=:0", "public.ecr.aws/spore-host/igv:2.17.4"},
			notWant:   "--gpus",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := containerInitCommand(tt.image, tt.gpu)
			for _, p := range tt.wantParts {
				if !strings.Contains(got, p) {
					t.Errorf("containerInitCommand() = %q, missing %q", got, p)
				}
			}
			if tt.notWant != "" && strings.Contains(got, tt.notWant) {
				t.Errorf("containerInitCommand() = %q, should not contain %q", got, tt.notWant)
			}
		})
	}
}

// TestBuildContainerDCVUserData asserts the container user-data pre-pulls the
// image and uses the container run as the DCV session init — the #290 launch
// path. Decodes the base64 to inspect the script.
func TestBuildContainerDCVUserData(t *testing.T) {
	const image = "public.ecr.aws/spore-host/paraview:5.13.2"
	enc := buildContainerDCVUserData(image, true, "console")
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatalf("user-data is not valid base64: %v", err)
	}
	script := string(raw)

	for _, want := range []string{
		"docker pull " + image, // pre-pull before session start
		"--gpus all",           // GPU passthrough into the container
		"dcv create-session",   // still creates the DCV session
		"--init",               // container run is the session init
		"docker run",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("container user-data missing %q\n---\n%s", want, script)
		}
	}
}

// TestBuildDCVUserData_LegacyUnchanged guards that the non-container path still
// bakes the launch_command as init and does NOT pull a container.
func TestBuildDCVUserData_LegacyUnchanged(t *testing.T) {
	enc := buildDCVUserData("/opt/igv/igv.sh", "console")
	raw, _ := base64.StdEncoding.DecodeString(enc)
	script := string(raw)
	if !strings.Contains(script, "/opt/igv/igv.sh") {
		t.Error("legacy user-data should bake the launch_command")
	}
	if strings.Contains(script, "docker pull") {
		t.Error("legacy user-data must not pre-pull a container")
	}
}
