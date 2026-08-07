package userdata

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/spore-host/spawn/pkg/aws"
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
	// private-ECR ref (see ecrImageAccount). Ignored for a public image.
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

	if host := ecrImageAccountHost(cfg.Image, cfg.Region); host.registry != "" {
		fmt.Fprintf(&b, "echo 'spawn: authenticating to ECR (%s)...'\n", host.registry)
		fmt.Fprintf(&b, "aws ecr get-login-password --region %s | docker login --username AWS --password-stdin %s 2>&1 || echo 'WARNING: ECR login failed; private pull will fail'\n",
			shQuote(host.region), shQuote(host.registry))
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

// ecrAccountRe extracts the 12-digit account ID from a private-ECR image host
// (<account>.dkr.ecr.<region>.amazonaws.com/<repo>[:tag]). Duplicated from
// pkg/taskproto/wrapper.go and cmd/app_byo.go rather than imported: pkg/userdata
// must stay free of both the CLI (cmd) and the task-execution protocol
// (taskproto) as dependencies — this leaf's only job is generating scripts.
var ecrAccountRe = regexp.MustCompile(`^(\d{12})\.dkr\.ecr\.([a-z0-9-]+)\.amazonaws\.com/`)

type ecrHost struct {
	registry string
	region   string
}

// ecrImageAccountHost returns the registry host and region to authenticate
// against when image is a private-ECR ref, or a zero ecrHost (registry=="") if
// it isn't (a public image, e.g. docker.io/nginx, needs no login). The image's
// own embedded region takes precedence over the caller-supplied Region, since
// a cross-region ECR pull must authenticate against ITS region, not the
// instance's.
func ecrImageAccountHost(image, region string) ecrHost {
	m := ecrAccountRe.FindStringSubmatch(image)
	if m == nil {
		return ecrHost{}
	}
	if i := strings.IndexByte(image, '/'); i >= 0 {
		imageRegion := m[2]
		if imageRegion != "" {
			region = imageRegion
		}
		return ecrHost{registry: image[:i], region: region}
	}
	return ecrHost{}
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
