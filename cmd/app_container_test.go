package cmd

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestContainerRunWrapper(t *testing.T) {
	tests := []struct {
		name      string
		image     string
		gpu       bool
		wantParts []string
		notWant   string
	}{
		{
			name:  "gpu app gets --gpus all and session display",
			image: "public.ecr.aws/f8g1e7l5/paraview:5.13.2",
			gpu:   true,
			// Must use the DCV session's $DISPLAY/$XAUTHORITY, not a hardcoded :0 (#263).
			wantParts: []string{"docker run", "--rm", "--gpus all", `DISP="${DISPLAY:-:0}"`, `XAUTHORITY:-`, `-e DISPLAY="$DISP"`, "/tmp/.X11-unix:/tmp/.X11-unix", "public.ecr.aws/f8g1e7l5/paraview:5.13.2"},
		},
		{
			name:      "cpu app omits --gpus",
			image:     "public.ecr.aws/f8g1e7l5/igv:2.17.4",
			gpu:       false,
			wantParts: []string{"docker run", "--rm", `-e DISPLAY="$DISP"`, "public.ecr.aws/f8g1e7l5/igv:2.17.4"},
			notWant:   "--gpus",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := containerRunWrapper(tt.image, tt.gpu)
			for _, p := range tt.wantParts {
				if !strings.Contains(got, p) {
					t.Errorf("containerRunWrapper() missing %q\n---\n%s", p, got)
				}
			}
			if tt.notWant != "" && strings.Contains(got, tt.notWant) {
				t.Errorf("containerRunWrapper() should not contain %q", tt.notWant)
			}
			// Must NOT hardcode the host :0 display as the literal target (#263 regression guard).
			if strings.Contains(got, "-e DISPLAY=:0 ") {
				t.Errorf("containerRunWrapper() hardcodes DISPLAY=:0 — must use the session $DISPLAY (#263)")
			}
		})
	}
}

// TestBuildContainerDCVUserData asserts the container user-data pre-pulls the
// image and uses the container run as the DCV session init — the #290 launch
// path. Decodes the base64 to inspect the script.
func TestBuildContainerDCVUserData(t *testing.T) {
	const image = "public.ecr.aws/f8g1e7l5/paraview:5.13.2"
	enc := buildContainerDCVUserData(image, true, false, "us-east-1", "console")
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatalf("user-data is not valid base64: %v", err)
	}
	script := string(raw)

	for _, want := range []string{
		"docker pull " + image,              // pre-pull before session start
		"--gpus all",                        // GPU passthrough into the container
		"dcv create-session",                // still creates the DCV session
		`--init "` + containerRunPath + `"`, // session init is the installed wrapper (#263)
		"chmod +x " + containerRunPath,      // wrapper is written + chmod'd
		`-e DISPLAY="$DISP"`,                // wrapper passes the session display, not :0
		"systemctl enable spored",           // spored started via systemd, not 'spored monitor' (#264)
	} {
		if !strings.Contains(script, want) {
			t.Errorf("container user-data missing %q\n---\n%s", want, script)
		}
	}
	// #264 regression guard: must not INVOKE the removed 'spored monitor'
	// subcommand (a passing mention in a comment is fine).
	if strings.Contains(script, "spored monitor >") || strings.Contains(script, "/spored monitor") {
		t.Error("user-data invokes removed 'spored monitor' subcommand (#264)")
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
