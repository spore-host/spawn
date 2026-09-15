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

// TestDCVInstalledAtBoot asserts the user-data installs Amazon DCV at boot on
// the AWS DLAMI base (spore-host#286/#389): the install is idempotent-guarded,
// pulls the correct pinned version from the DCV CloudFront layout, and includes
// the virtual-session (nice-xdcv) and GPU-GL (nice-dcv-gl) packages. This is the
// change that lets `spawn app launch` work without an owned/shared "DCV base AMI".
func TestDCVInstalledAtBoot(t *testing.T) {
	enc := buildContainerDCVUserData("public.ecr.aws/f8g1e7l5/paraview:5.13.2", true, false, "us-east-1", "console")
	raw, _ := base64.StdEncoding.DecodeString(enc)
	script := string(raw)

	for _, want := range []string{
		"command -v dcv",                        // idempotent guard — skip if already present
		"nice-dcv-" + dcvVersion + "-amzn2023-", // correct pinned tarball for AL2023
		"d1uj6qtbmh3dt5.cloudfront.net/" + dcvVersionMajorMinor + "/Servers", // official DCV download layout
		"nice-dcv-server-", // the DCV server package
		"nice-xdcv-",       // virtual-session X server (dcv create-session --type virtual)
		"nice-dcv-gl-",     // GPU-accelerated OpenGL (guarded to x86_64)
		"NICE-GPG-KEY",     // package signature verification
	} {
		if !strings.Contains(script, want) {
			t.Errorf("boot DCV install missing %q\n---\n%s", want, script)
		}
	}
	// The install must precede the cert step and dcvserver start (the cert step
	// chowns dcv: and restarts dcvserver — both need DCV already installed).
	installAt := strings.Index(script, "command -v dcv")
	certAt := strings.Index(script, "DCV_CERT_DIR")
	startAt := strings.Index(script, "systemctl start dcvserver")
	if installAt < 0 || certAt < 0 || startAt < 0 || !(installAt < certAt && installAt < startAt) {
		t.Errorf("DCV install must come before cert install and dcvserver start (install=%d cert=%d start=%d)", installAt, certAt, startAt)
	}
}

// TestBuildDesktopDCVUserData asserts a bare desktop session (#591) installs a
// desktop environment + DCV, runs the desktop init (not an app), and pulls no
// container.
func TestBuildDesktopDCVUserData(t *testing.T) {
	enc := buildDesktopDCVUserData("console")
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatalf("user-data is not valid base64: %v", err)
	}
	script := string(raw)
	if strings.Contains(script, "%!") {
		t.Fatalf("format-verb leak in desktop user-data:\n%s", script)
	}
	for _, want := range []string{
		"command -v dcv",                        // DCV still installed at boot
		`groupinstall -y "Desktop"`,             // desktop environment install
		desktopInitPath,                         // desktop launcher written
		"--init " + `"` + desktopInitPath + `"`, // DCV session init is the desktop, not an app
		"gnome-session",                         // full desktop preferred
		"xterm",                                 // bare terminal fallback
		"dcv create-session",                    // still a DCV session
	} {
		if !strings.Contains(script, want) {
			t.Errorf("desktop user-data missing %q\n---\n%s", want, script)
		}
	}
	if strings.Contains(script, "docker pull") {
		t.Error("desktop session must not pull a container")
	}
}

// TestBuildWebUserData asserts a web-UI app (#590) does NOT install DCV, pulls
// the container and publishes its port on localhost, drops the TLS cert for the
// spored proxy, and starts spored — with no DCV session created.
func TestBuildWebUserData(t *testing.T) {
	enc := buildWebUserData("public.ecr.aws/x/code-server:latest", 8080, false, false, "us-east-1")
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatalf("user-data is not valid base64: %v", err)
	}
	script := string(raw)
	if strings.Contains(script, "%!") {
		t.Fatalf("format-verb leak in web user-data:\n%s", script)
	}
	for _, want := range []string{
		"docker pull public.ecr.aws/x/code-server:latest",
		"docker run -d --restart unless-stopped -p 127.0.0.1:8080:8080", // localhost publish
		"/etc/spore/webproxy/cert.pem",                                  // TLS cert for the proxy
		"systemctl enable spored",                                       // spored runs the proxy + handshake
	} {
		if !strings.Contains(script, want) {
			t.Errorf("web user-data missing %q\n---\n%s", want, script)
		}
	}
	// A web app must NOT install DCV or create a DCV session.
	for _, notWant := range []string{"command -v dcv", "dcv create-session", "nice-dcv"} {
		if strings.Contains(script, notWant) {
			t.Errorf("web user-data must not contain %q (no DCV for web apps)", notWant)
		}
	}
}

// TestBuildWebUserData_PrivateLogin asserts a private image adds an ECR login.
func TestBuildWebUserData_PrivateLogin(t *testing.T) {
	enc := buildWebUserData("111111111111.dkr.ecr.us-east-1.amazonaws.com/app:1", 8888, true, true, "us-east-1")
	raw, _ := base64.StdEncoding.DecodeString(enc)
	script := string(raw)
	if !strings.Contains(script, "ecr get-login-password") {
		t.Error("private web image should authenticate to ECR before pull")
	}
	if !strings.Contains(script, "--gpus all") {
		t.Error("gpu web app should pass --gpus all")
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
