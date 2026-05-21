// Package pluginruntime implements the on-instance plugin lifecycle (spored side).
package pluginruntime

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spore-host/spawn/pkg/plugin"
)

// fetchClient is used for all plugin fetch steps; the 5-minute timeout prevents
// a stalled download from hanging the plugin lifecycle indefinitely.
var fetchClient = &http.Client{Timeout: 5 * time.Minute}

// maxFetchBytes caps the response body for fetch steps (2 GiB) to prevent
// a malicious or runaway download from filling the disk.
const maxFetchBytes = 2 << 30

// envKeyRe matches valid POSIX environment variable names.
var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validEnvKey(k string) bool { return envKeyRe.MatchString(k) }

// RemoteExecutor runs plugin steps on the local instance.
type RemoteExecutor struct{}

// NewRemoteExecutor creates a RemoteExecutor.
func NewRemoteExecutor() *RemoteExecutor {
	return &RemoteExecutor{}
}

// RunSteps executes a sequence of steps, returning on the first error.
// "run" steps are rendered with shell-safe escaping to prevent injection.
func (e *RemoteExecutor) RunSteps(ctx context.Context, steps []plugin.Step, tmplCtx plugin.TemplateContext) error {
	for i, step := range steps {
		var (
			rendered plugin.Step
			err      error
		)
		if step.Type == "run" {
			rendered, err = plugin.RenderShellStep(step, tmplCtx)
		} else {
			rendered, err = plugin.RenderStep(step, tmplCtx)
		}
		if err != nil {
			return fmt.Errorf("step[%d] render: %w", i, err)
		}
		if err := e.runStep(ctx, rendered); err != nil {
			return fmt.Errorf("step[%d] type=%s: %w", i, rendered.Type, err)
		}
	}
	return nil
}

func (e *RemoteExecutor) runStep(ctx context.Context, step plugin.Step) error {
	switch step.Type {
	case "run":
		return e.runCommand(ctx, step)
	case "fetch":
		return e.fetch(ctx, step)
	case "extract":
		return e.extract(ctx, step)
	default:
		return fmt.Errorf("unsupported remote step type %q", step.Type)
	}
}

func (e *RemoteExecutor) runCommand(ctx context.Context, step plugin.Step) error {
	script := step.Run
	if step.Background {
		// Detach via nohup so the step returns immediately.
		script = "nohup sh -c " + shellQuote(step.Run) + " </dev/null >/dev/null 2>&1 &"
	}

	cmd := exec.CommandContext(ctx, "sh", "-c", script) // nosemgrep: dangerous-exec-command -- plugin step script, intentional
	// Initialize with a minimal safe environment to avoid inheriting parent credentials.
	cmd.Env = []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=" + os.Getenv("HOME"),
	}
	for k, v := range step.Env {
		if !validEnvKey(k) {
			return fmt.Errorf("invalid env var key %q", k)
		}
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("run %q: %w", step.Run, err)
	}
	return nil
}

// validateFetchURL rejects URLs that would reach private or link-local
// addresses (e.g. EC2 IMDS at 169.254.169.254) or use non-HTTPS schemes.
func validateFetchURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("fetch URL must use https scheme, got %q", u.Scheme)
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsPrivate() {
			return fmt.Errorf("fetch URL must not target private/loopback/link-local address")
		}
	}
	return nil
}

func (e *RemoteExecutor) fetch(ctx context.Context, step plugin.Step) error {
	if step.URL == "" || step.Dest == "" {
		return fmt.Errorf("fetch step requires url and dest")
	}
	if err := validateFetchURL(step.URL); err != nil {
		return fmt.Errorf("fetch step: %w", err)
	}

	// Ensure parent directory exists.
	parent := parentDir(step.Dest)
	if parent != "" {
		if err := os.MkdirAll(parent, 0755); err != nil && !os.IsExist(err) {
			return fmt.Errorf("mkdir %s: %w", parent, err)
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, step.URL, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	resp, err := fetchClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", step.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch %s: HTTP %d", step.URL, resp.StatusCode)
	}

	f, err := os.Create(step.Dest)
	if err != nil {
		return fmt.Errorf("create %s: %w", step.Dest, err)
	}
	defer func() { _ = f.Close() }()

	if _, err := io.Copy(f, io.LimitReader(resp.Body, maxFetchBytes)); err != nil {
		return fmt.Errorf("write %s: %w", step.Dest, err)
	}
	return nil
}

func (e *RemoteExecutor) extract(ctx context.Context, step plugin.Step) error {
	if step.Src == "" || step.Dest == "" {
		return fmt.Errorf("extract step requires src and dest")
	}

	// Require absolute, clean paths to prevent path traversal.
	if !filepath.IsAbs(step.Dest) || step.Dest != filepath.Clean(step.Dest) {
		return fmt.Errorf("extract dest must be an absolute clean path: %q", step.Dest)
	}
	if !filepath.IsAbs(step.Src) || step.Src != filepath.Clean(step.Src) {
		return fmt.Errorf("extract src must be an absolute clean path: %q", step.Src)
	}

	if err := os.MkdirAll(step.Dest, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", step.Dest, err)
	}

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "tar", "--no-overwrite-dir", "-xzf", step.Src, "-C", step.Dest)
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tar extract %s: %w — %s", step.Src, err, stderr.String())
	}
	return nil
}

// parentDir returns the directory component of a file path.
func parentDir(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return ""
}

// shellQuote wraps s in single quotes, escaping any embedded single quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
