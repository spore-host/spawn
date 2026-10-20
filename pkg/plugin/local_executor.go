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

// BufferingPushClient records pushes in memory, and is the ONLY implementation —
// deliberately, because buffering is how delivery works here, not a stand-in for
// it.
//
// `spawn plugin install` runs local provision steps, collects what they push,
// and hands the whole map to spored in the install request
// (cmd/plugin.go: remotePluginInstall → SSH tunnel → POST
// 127.0.0.1:7777/v1/plugins/install). That guarantees the values are present
// before the remote configure phase runs, which delivering them one at a time
// could not.
//
// Saying so plainly because the previous wording — "records pushes in memory
// instead of delivering them to a remote runtime" — read as a stub, and #810
// concluded from it that "pushed plugin data never reaches an instance". It does.
// There were two other PushClient implementations (an SSM transport and an SSH
// tunnel in pkg/plugin) that nothing constructed; they duplicated the tunnel and
// token helpers cmd/plugin.go already has, and #810 removed them. The receiver
// they targeted is live and reached by the path above.
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
	// envPassthrough names controller environment variables (from the spec's
	// local.env_passthrough) that steps are allowed to read; only these are
	// injected into the otherwise-minimal step environment.
	envPassthrough []string
}

// NewLocalExecutor creates a LocalExecutor.  push may be nil if no push steps
// are expected (e.g., when only running deprovision).
func NewLocalExecutor(push PushClient) *LocalExecutor {
	return &LocalExecutor{push: push}
}

// WithEnvPassthrough returns e configured to inject the named controller
// environment variables into local step environments (see LocalBlock.
// EnvPassthrough). Returns the same executor for chaining.
func (e *LocalExecutor) WithEnvPassthrough(names []string) *LocalExecutor {
	e.envPassthrough = names
	return e
}

// CheckLocalConditions verifies the spec's conditions.local on the controller
// before anything is installed. Condition commands are rendered against tmplCtx
// (config is resolved by then; instance and outputs may not be) and run with
// the caller's environment, unlike local steps: a condition's job is often to
// check that a variable the plugin needs is set (e.g. tailscale's
// `test -n "$TS_API_CLIENT_SECRET"`), and it only reports an exit status.
func (e *LocalExecutor) CheckLocalConditions(ctx context.Context, conditions []Condition, tmplCtx TemplateContext) error {
	return CheckConditions(ctx, conditions, tmplCtx, LocalConditionHost())
}

// LocalConditionHost is the controller as a ConditionHost.
func LocalConditionHost() ConditionHost {
	return ConditionHost{
		GOOS: runtime.GOOS,
		RunCommand: func(ctx context.Context, script string) error {
			cmd := exec.CommandContext(ctx, "sh", "-c", script) // nosemgrep: dangerous-exec-command -- plugin condition defined by plugin author
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				if msg := strings.TrimSpace(stderr.String()); msg != "" {
					return fmt.Errorf("%w: %s", err, msg)
				}
				return err
			}
			return nil
		},
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

// RunDeprovision runs local teardown steps: a spec's local.deprovision, and its
// local.reconcile (replayed by `spawn start` after the instance's IP changes,
// see cmd/plugin.go reconcileAllLocalPlugins). Only "run" steps are supported.
func (e *LocalExecutor) RunDeprovision(ctx context.Context, steps []Step, tmplCtx TemplateContext) error {
	for i, step := range steps {
		if step.Type != "run" {
			return fmt.Errorf("step[%d]: unsupported deprovision step type %q", i, step.Type)
		}
		rendered, err := RenderShellStep(step, tmplCtx)
		if err != nil {
			return fmt.Errorf("step[%d] render: %w", i, err)
		}
		// Teardown steps capture nothing: there is no later step or record to
		// hand an output to, so the validator rejects capture here.
		if _, err := e.runLocal(ctx, rendered); err != nil {
			return fmt.Errorf("step[%d] run %q: %w", i, rendered.Run, err)
		}
	}
	return nil
}

// runCapture runs a step and extracts its capture: values from the JSON it
// prints on stdout.
func (e *LocalExecutor) runCapture(ctx context.Context, step Step) (map[string]string, error) {
	stdout, err := e.runLocal(ctx, step)
	if err != nil {
		return nil, err
	}
	return captureOutputs(step.Capture, stdout)
}

// runLocal executes a run step's shell command and returns its stdout.
func (e *LocalExecutor) runLocal(ctx context.Context, step Step) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", step.Run) // nosemgrep: dangerous-exec-command -- plugin step defined by plugin author
	// Initialize with a minimal safe environment to avoid inheriting parent
	// credentials (AWS_*, tokens, …). PATH and HOME are NOT secrets and are
	// required so provision tools (mutagen, globus-cli) are discoverable — in
	// particular the parent PATH is preserved so Homebrew locations like
	// /opt/homebrew/bin (Apple Silicon) are found; we fall back to a standard
	// PATH only when the parent has none.
	pathEnv := os.Getenv("PATH")
	if pathEnv == "" {
		pathEnv = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	cmd.Env = []string{
		"PATH=" + pathEnv,
		"HOME=" + os.Getenv("HOME"),
	}
	// Inject only the explicitly opted-in controller variables (spec
	// local.env_passthrough), resolved from spawn's own environment. A name that
	// isn't set on the controller is skipped silently — a local condition should
	// be the one to require it with a helpful message.
	for _, name := range e.envPassthrough {
		if !validLocalEnvKey(name) {
			return nil, fmt.Errorf("invalid env_passthrough key %q", name)
		}
		if v, ok := os.LookupEnv(name); ok {
			cmd.Env = append(cmd.Env, name+"="+v)
		}
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
	return stdout.Bytes(), nil
}

// captureOutputs extracts each capture (varname -> path) from stdout as JSON.
func captureOutputs(capture map[string]string, stdout []byte) (map[string]string, error) {
	captures := make(map[string]string)
	if len(capture) == 0 {
		return captures, nil
	}

	// Parse stdout as JSON for capture expressions.
	var jsonData interface{} // nosemgrep: go.lang.security.deserialization.unsafe-deserialization-interface.go-unsafe-deserialization-interface
	if err := json.Unmarshal(stdout, &jsonData); err != nil {
		return nil, fmt.Errorf("capture: stdout is not valid JSON: %w", err)
	}

	for varName, path := range capture {
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
