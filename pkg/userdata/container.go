package userdata

import (
	"fmt"
	"strings"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/ecrref"
)

// ContainerConfig parameterizes a headless "provision a host and docker run
// <image>" launch (spore-host#353). It has no DISPLAY/XAUTHORITY/X11 wiring —
// that machinery is DCV-session-specific (cmd/app.go's containerRunWrapper) and
// stays there; this is the plain, non-interactive counterpart for a library
// consumer that just wants a container running headlessly.
type ContainerConfig struct {
	// Image is the container image ref to run, e.g.
	// "123456789012.dkr.ecr.us-east-1.amazonaws.com/myimage:latest" or a public
	// ref like "nginx:latest". Required.
	Image string
	// Region is the AWS region, used for the ECR login when Image resolves to a
	// private-ECR ref (see pkg/ecrref.AuthHost). Ignored for a public image.
	Region string
	// InstanceType, if set, is used to auto-detect whether to pass --gpus all to
	// docker run (via aws.DetectGPUInstance) — the same inference Provision
	// already applies to AMI selection, so a caller doesn't separately track
	// "is this a GPU type?" Leave empty to never pass --gpus.
	InstanceType string
	// Command, if non-empty, is the container's argv (docker run's trailing
	// CMD override). Empty runs the image's own ENTRYPOINT/CMD unmodified.
	Command []string
}

// GenerateContainerUserData returns a boot-time shell fragment that installs
// Docker if absent, authenticates to a private ECR registry if the image needs
// it, pulls the image, and runs it — headlessly, no DCV/X11. Intended for
// launcher.Options.ContainerScript, which Provision splices into the bootstrap
// after StorageScript and before CustomUserData (mirroring the ordering
// GenerateStorageUserData already relies on: mounts before workload).
//
// Errors only on a missing required field; the returned script itself only
// warns (not aborts) on a failed pull/login, matching cmd/app.go's existing
// DCV container path — a transient registry blip shouldn't fail the whole
// boot when spored's own retry/health story can recover.
func GenerateContainerUserData(cfg ContainerConfig) (string, error) {
	if strings.TrimSpace(cfg.Image) == "" {
		return "", fmt.Errorf("container user-data: Image is required")
	}

	var b strings.Builder
	fmt.Fprintf(&b, "\n# Headless container run (spore-host#353): %s\n", cfg.Image)
	b.WriteString("if ! command -v docker >/dev/null 2>&1; then\n")
	b.WriteString("  echo 'spawn: installing docker...'\n")
	b.WriteString("  (dnf install -y docker || apt-get install -y docker.io) 2>&1 || echo 'WARNING: docker install failed'\n")
	b.WriteString("fi\n")
	b.WriteString("systemctl enable --now docker 2>&1 || echo 'WARNING: docker service failed to start'\n")
	b.WriteString("for _i in $(seq 1 30); do docker info >/dev/null 2>&1 && break; sleep 2; done\n")

	if host, region, ok := ecrref.AuthHost(cfg.Image, cfg.Region); ok {
		fmt.Fprintf(&b, "echo 'spawn: authenticating to ECR (%s)...'\n", host)
		fmt.Fprintf(&b, "aws ecr get-login-password --region %s | docker login --username AWS --password-stdin %s 2>&1 || echo 'WARNING: ECR login failed; private pull will fail'\n",
			shQuote(region), shQuote(host))
	}

	fmt.Fprintf(&b, "docker pull %s || echo 'WARNING: docker pull failed'\n", shQuote(cfg.Image))

	gpuFlag := ""
	if cfg.InstanceType != "" && aws.DetectGPUInstance(cfg.InstanceType) {
		gpuFlag = "--gpus all "
	}
	cmd := quoteArgv(cfg.Command)
	if cmd != "" {
		cmd = " " + cmd
	}
	fmt.Fprintf(&b, "docker run --rm %s%s%s\n", gpuFlag, shQuote(cfg.Image), cmd)

	return b.String(), nil
}

// shQuote wraps s in single quotes, escaping embedded single quotes — the safe
// form for shell interpolation. Mirrors pkg/taskproto/wrapper.go's helper
// rather than security.ShellEscape, whose strconv.Quote-based double-quote
// output does not neutralize $ or backticks inside a shell double-quoted
// context.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// quoteArgv renders argv as space-separated shell-quoted words (docker run's
// trailing CMD override), or "" if empty (run the image's own ENTRYPOINT/CMD).
func quoteArgv(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = shQuote(a)
	}
	return strings.Join(parts, " ")
}
