package userdata

import (
	"strings"
	"testing"
)

// TestGenerateContainerUserData_RequiresImage verifies the required-field check.
func TestGenerateContainerUserData_RequiresImage(t *testing.T) {
	if _, err := GenerateContainerUserData(ContainerConfig{}); err == nil {
		t.Error("want error for missing Image")
	}
}

// TestGenerateContainerUserData_PublicImageSkipsLogin verifies a public (non-ECR)
// image pulls anonymously — no docker login, no aws ecr call.
func TestGenerateContainerUserData_PublicImageSkipsLogin(t *testing.T) {
	script, err := GenerateContainerUserData(ContainerConfig{Image: "nginx:latest", Region: "us-east-1"})
	if err != nil {
		t.Fatalf("GenerateContainerUserData: %v", err)
	}
	if strings.Contains(script, "ecr get-login-password") {
		t.Error("public image must not trigger an ECR login")
	}
	if !strings.Contains(script, "docker pull 'nginx:latest'") {
		t.Errorf("script must pull the image; got:\n%s", script)
	}
	if !strings.Contains(script, "docker run --rm 'nginx:latest'") {
		t.Errorf("script must run the image with no CMD override; got:\n%s", script)
	}
}

// TestGenerateContainerUserData_PrivateECRLogsIn verifies a private-ECR image
// (12-digit account prefix) triggers a docker login against the RIGHT registry
// host and region before pulling — spore-host#392's cross-account grant story
// depends on authenticating before the pull, not after.
func TestGenerateContainerUserData_PrivateECRLogsIn(t *testing.T) {
	image := "123456789012.dkr.ecr.us-west-2.amazonaws.com/myimage:v1"
	script, err := GenerateContainerUserData(ContainerConfig{Image: image, Region: "us-east-1"})
	if err != nil {
		t.Fatalf("GenerateContainerUserData: %v", err)
	}
	// The image's OWN region (us-west-2) must win over the caller-supplied
	// Region (us-east-1) — a cross-region ECR pull authenticates against its
	// own region, not the launching instance's.
	if !strings.Contains(script, "aws ecr get-login-password --region 'us-west-2'") {
		t.Errorf("must authenticate against the image's OWN region (us-west-2), not the caller's Region; got:\n%s", script)
	}
	if !strings.Contains(script, "docker login --username AWS --password-stdin '123456789012.dkr.ecr.us-west-2.amazonaws.com'") {
		t.Errorf("must log in against the image's registry host; got:\n%s", script)
	}
	// The login must precede the pull.
	loginIdx := strings.Index(script, "docker login")
	pullIdx := strings.Index(script, "docker pull")
	if loginIdx < 0 || pullIdx < 0 || loginIdx > pullIdx {
		t.Errorf("docker login must precede docker pull; got:\n%s", script)
	}
}

// TestGenerateContainerUserData_GPUInference verifies --gpus all is passed only
// for a recognized GPU instance type, and omitted otherwise or when
// InstanceType is unset.
func TestGenerateContainerUserData_GPUInference(t *testing.T) {
	cases := []struct {
		name         string
		instanceType string
		wantGPUFlag  bool
	}{
		{"gpu type", "g6e.2xlarge", true},
		{"cpu type", "c7i.2xlarge", false},
		{"no instance type", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			script, err := GenerateContainerUserData(ContainerConfig{Image: "nginx:latest", InstanceType: c.instanceType})
			if err != nil {
				t.Fatalf("GenerateContainerUserData: %v", err)
			}
			got := strings.Contains(script, "docker run --rm --gpus all ")
			if got != c.wantGPUFlag {
				t.Errorf("instanceType=%q: --gpus all present = %v, want %v\n%s", c.instanceType, got, c.wantGPUFlag, script)
			}
		})
	}
}

// TestGenerateContainerUserData_CommandOverride verifies a non-empty Command is
// appended as the docker run CMD override, each argument individually quoted.
func TestGenerateContainerUserData_CommandOverride(t *testing.T) {
	script, err := GenerateContainerUserData(ContainerConfig{
		Image:   "nginx:latest",
		Command: []string{"echo", "it's a test"},
	})
	if err != nil {
		t.Fatalf("GenerateContainerUserData: %v", err)
	}
	if !strings.Contains(script, "docker run --rm 'nginx:latest' 'echo' 'it'\\''s a test'") {
		t.Errorf("script must append the quoted CMD override; got:\n%s", script)
	}
}

// TestGenerateContainerUserData_NoDCVWiring verifies this headless path never
// touches DISPLAY/XAUTHORITY — that's cmd/app.go's DCV-session-specific
// containerRunWrapper, a different code path entirely (spore-host#353's whole
// point: a consumer that wants a container running headlessly, not into a GUI
// session's display).
func TestGenerateContainerUserData_NoDCVWiring(t *testing.T) {
	script, err := GenerateContainerUserData(ContainerConfig{Image: "nginx:latest"})
	if err != nil {
		t.Fatalf("GenerateContainerUserData: %v", err)
	}
	for _, forbidden := range []string{"DISPLAY", "XAUTHORITY", "X11", "dcv"} {
		if strings.Contains(script, forbidden) {
			t.Errorf("headless container script must not reference %q (DCV-only concern); got:\n%s", forbidden, script)
		}
	}
}

// TestGenerateContainerUserData_InstallsDockerOnDemand verifies the script
// installs Docker only if absent, waits for the daemon, and doesn't hard-fail
// the whole boot on an install/pull hiccup (matches cmd/app.go's existing
// warn-not-abort DCV container path).
func TestGenerateContainerUserData_InstallsDockerOnDemand(t *testing.T) {
	script, err := GenerateContainerUserData(ContainerConfig{Image: "nginx:latest"})
	if err != nil {
		t.Fatalf("GenerateContainerUserData: %v", err)
	}
	if !strings.Contains(script, "command -v docker") {
		t.Error("script must check for an existing docker before installing")
	}
	if !strings.Contains(script, "systemctl enable --now docker") {
		t.Error("script must ensure the docker service is running")
	}
	if !strings.Contains(script, "WARNING: docker pull failed") {
		t.Error("a failed pull must warn, not abort the whole user-data (set -e safety)")
	}
}
