package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
)

// localEnvKeyRe matches valid POSIX environment variable names.
var localEnvKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validLocalEnvKey(k string) bool { return localEnvKeyRe.MatchString(k) }

// PushClient delivers a key/value to the remote plugin runtime.
type PushClient interface {
	Push(ctx context.Context, pluginName, key, value string) error
}

// BufferingPushClient records pushes in memory instead of delivering them to a
// remote runtime. The unified `spawn plugin install` flow uses it so local
// provision can capture and "push" values that are then handed to spored in the
// install request (see cmd/plugin.go), guaranteeing the values are present
// before the remote configure phase runs.
type BufferingPushClient struct {
	values map[string]string
}

// NewBufferingPushClient creates an empty BufferingPushClient.
func NewBufferingPushClient() *BufferingPushClient {
	return &BufferingPushClient{values: make(map[string]string)}
}

// Push records the value under key. pluginName is ignored — a client instance is
// scoped to a single plugin install.
func (c *BufferingPushClient) Push(_ context.Context, _ /*pluginName*/, key, value string) error {
	c.values[key] = value
	return nil
}

// Values returns the buffered pushes.
func (c *BufferingPushClient) Values() map[string]string { return c.values }

// LocalExecutor runs local plugin lifecycle steps on the controller machine.
type LocalExecutor struct {
	push PushClient
}

// NewLocalExecutor creates a LocalExecutor.  push may be nil if no push steps
// are expected (e.g., when only running deprovision).
func NewLocalExecutor(push PushClient) *LocalExecutor {
	return &LocalExecutor{push: push}
}

// CheckLocalConditions verifies all local pre-flight conditions before install.
func (e *LocalExecutor) CheckLocalConditions(conditions []Condition) error {
	for _, cond := range conditions {
		if err := e.checkCondition(cond); err != nil {
			msg := cond.Message
			if msg == "" {
				msg = fmt.Sprintf("condition %q failed", cond.Type)
			}
			return fmt.Errorf("%s: %w", msg, err)
		}
	}
	return nil
}

func (e *LocalExecutor) checkCondition(cond Condition) error {
	switch cond.Type {
	case "command":
		cmd := exec.Command("sh", "-c", cond.Run) // nosemgrep: dangerous-exec-command -- plugin condition defined by plugin author
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("command %q: %w", cond.Run, err)
		}
		return nil
	case "platform":
		if cond.OS != "" && runtime.GOOS != cond.OS {
			return fmt.Errorf("requires OS %q, running on %q", cond.OS, runtime.GOOS)
		}
		return nil
	default:
		return fmt.Errorf("unknown condition type %q", cond.Type)
	}
}

// RunProvision runs the local provision steps, updating tmplCtx.Outputs as
// captures accumulate.  Returns the final outputs map.
func (e *LocalExecutor) RunProvision(ctx context.Context, pluginName string, steps []Step, tmplCtx TemplateContext) (map[string]string, error) {
	for i, step := range steps {
		var (
			rendered Step
			err      error
		)
		if step.Type == "run" {
			rendered, err = RenderShellStep(step, tmplCtx)
		} else {
			rendered, err = RenderStep(step, tmplCtx)
		}
		if err != nil {
			return nil, fmt.Errorf("step[%d] render: %w", i, err)
		}

		switch rendered.Type {
		case "run":
			captures, err := e.runCapture(ctx, rendered)
			if err != nil {
				return nil, fmt.Errorf("step[%d] run %q: %w", i, rendered.Run, err)
			}
			for k, v := range captures {
				tmplCtx.Outputs[k] = v
			}

		case "push":
			if e.push == nil {
				return nil, fmt.Errorf("step[%d]: push step requires a push client", i)
			}
			value, err := Render(rendered.Value, tmplCtx)
			if err != nil {
				return nil, fmt.Errorf("step[%d] push value: %w", i, err)
			}
			if err := e.push.Push(ctx, pluginName, rendered.Key, value); err != nil {
				return nil, fmt.Errorf("step[%d] push %s: %w", i, rendered.Key, err)
			}

		default:
			return nil, fmt.Errorf("step[%d]: unsupported local step type %q", i, rendered.Type)
		}
	}
	return tmplCtx.Outputs, nil
}

// RunDeprovision runs the local deprovision steps.
func (e *LocalExecutor) RunDeprovision(ctx context.Context, steps []Step, tmplCtx TemplateContext) error {
	for i, step := range steps {
		if step.Type != "run" {
			return fmt.Errorf("step[%d]: unsupported deprovision step type %q", i, step.Type)
		}
		rendered, err := RenderShellStep(step, tmplCtx)
		if err != nil {
			return fmt.Errorf("step[%d] render: %w", i, err)
		}
		if _, err := e.runCapture(ctx, rendered); err != nil {
			return fmt.Errorf("step[%d] run %q: %w", i, rendered.Run, err)
		}
	}
	return nil
}

// runCapture executes a shell command and extracts captured values from JSON stdout.
func (e *LocalExecutor) runCapture(ctx context.Context, step Step) (map[string]string, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", step.Run) // nosemgrep: dangerous-exec-command -- plugin step defined by plugin author
	// Initialize with a minimal safe environment to avoid inheriting parent credentials.
	cmd.Env = []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=" + os.Getenv("HOME"),
	}
	for k, v := range step.Env {
		if !validLocalEnvKey(k) {
			return nil, fmt.Errorf("invalid env var key %q", k)
		}
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w\nstderr: %s", err, strings.TrimSpace(stderr.String()))
	}

	captures := make(map[string]string)
	if len(step.Capture) == 0 {
		return captures, nil
	}

	// Parse stdout as JSON for capture expressions.
	var jsonData interface{} // nosemgrep: go.lang.security.deserialization.unsafe-deserialization-interface.go-unsafe-deserialization-interface
	if err := json.Unmarshal(stdout.Bytes(), &jsonData); err != nil {
		return nil, fmt.Errorf("capture: stdout is not valid JSON: %w", err)
	}

	for varName, path := range step.Capture {
		value, err := extractJSONPath(jsonData, path)
		if err != nil {
			return nil, fmt.Errorf("capture %q via path %q: %w", varName, path, err)
		}
		captures[varName] = value
	}

	return captures, nil
}

// extractJSONPath extracts a value from a JSON structure using a simple
// dot-separated path (leading dot stripped). Supports only object keys.
// Examples: ".setup_key" → value at root["setup_key"]
func extractJSONPath(data interface{}, path string) (string, error) {
	// Strip leading dot for jq-style paths.
	path = strings.TrimPrefix(path, ".")

	if path == "" {
		return fmt.Sprintf("%v", data), nil
	}

	parts := strings.SplitN(path, ".", 2)
	key := parts[0]
	rest := ""
	if len(parts) > 1 {
		rest = parts[1]
	}

	obj, ok := data.(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("expected JSON object at %q, got %T", key, data)
	}
	val, ok := obj[key]
	if !ok {
		return "", fmt.Errorf("key %q not found", key)
	}
	if rest == "" {
		return fmt.Sprintf("%v", val), nil
	}
	return extractJSONPath(val, rest)
}
