package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/plugin"
)

// #855: `spawn plugin install` reached spored's loopback push API only over
// an SSH port-forward, so an instance with no SSH path — keyless, SSM-only, no
// public IP — could not take a plugin. It now runs the request on the
// instance over SSM SendCommand when there is no public IP or SSH cannot
// reach the API, and keeps SSH when SSH works.

// ssmSandbox executes the SSM scripts for real, under sh, with the
// instance's absolute paths rewritten into a temp dir and the loopback URL
// pointed at a test spored. What reaches the test server is what spored
// would receive.
type ssmSandbox struct {
	dir     string
	rw      *strings.Replacer
	scripts []string
}

func newSSMSandbox(t *testing.T, sporedURL string) *ssmSandbox {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("runs the Linux SSM script under sh")
	}
	for _, tool := range []string{"sh", "curl", "base64"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "push.token"), []byte("tok-on-instance\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &ssmSandbox{dir: dir, rw: strings.NewReplacer(
		"/run/spawn-push-", filepath.Join(dir, "spawn-push-"),
		pushAPITokenPath, filepath.Join(dir, "push.token"),
		pushAPILoopback, sporedURL,
	)}
}

func (sb *ssmSandbox) run(_ context.Context, _, _, script string) (*aws.SSMRunResult, error) {
	sb.scripts = append(sb.scripts, script)
	cmd := exec.Command("sh", "-c", sb.rw.Replace(script)) //nolint:gosec // nosemgrep
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	res := &aws.SSMRunResult{Status: "Success", Stdout: stdout.String(), Stderr: stderr.String()}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.Status, res.ResponseCode = "Failed", int32(ee.ExitCode())
	} else if err != nil {
		return nil, err
	}
	return res, nil
}

// recordingSpored is a push API that records what it received.
type recordingSpored struct {
	method, path, auth string
	body               []byte
	status             int
	answer             string
}

func (r *recordingSpored) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.method, r.path, r.auth = req.Method, req.URL.Path, req.Header.Get("Authorization")
		r.body, _ = io.ReadAll(req.Body)
		w.WriteHeader(r.status)
		_, _ = io.WriteString(w, r.answer) // nosemgrep
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testSpec(t *testing.T, extra int) *plugin.PluginSpec {
	t.Helper()
	spec := &plugin.PluginSpec{Name: "demo", Version: "1.0.0", Description: strings.Repeat("d", extra)}
	spec.Remote.Install = []plugin.Step{{Type: "run", Run: "echo hi"}}
	return spec
}

func keylessInstance() *aws.InstanceInfo {
	// No public IP: SSM-only.
	return &aws.InstanceInfo{InstanceID: "i-0keyless", Region: "us-west-2", State: "running"}
}

func TestPluginInstallOnAKeylessInstanceGoesOverSSMWithTheSamePayload(t *testing.T) {
	// Both a body that fits one SendCommand and one that has to be staged
	// across several.
	for name, extra := range map[string]int{"small": 0, "chunked": 3 * ssmPushChunk} {
		t.Run(name, func(t *testing.T) {
			spored := &recordingSpored{status: http.StatusAccepted, answer: `{"status":"installing"}`}
			sb := newSSMSandbox(t, spored.server(t).URL)

			push, err := choosePushTransport(context.Background(), keylessInstance(), "i-0keyless", "", "",
				func(context.Context, string) (func(context.Context, string, string, string) (*aws.SSMRunResult, error), error) {
					return sb.run, nil
				})
			if err != nil {
				t.Fatal(err)
			}
			if push.via() != "SSM" {
				t.Fatalf("keyless instance with no public IP: transport %s, want SSM", push.via())
			}

			spec := testSpec(t, extra)
			cfg := map[string]string{"k": "v"}
			pushed := map[string]string{"secret": "s3cr3t"}
			prov := &plugin.Provenance{CommitSHA: "abc"}
			if err := remotePluginInstall(context.Background(), push, spec, cfg, pushed, prov); err != nil {
				t.Fatalf("install over SSM: %v", err)
			}
			want, err := pluginInstallPayload(spec, cfg, pushed, prov)
			if err != nil {
				t.Fatal(err)
			}
			if spored.method != http.MethodPost || spored.path != "/v1/plugins/install" {
				t.Errorf("spored got %s %s", spored.method, spored.path)
			}
			if !bytes.Equal(spored.body, want) {
				t.Errorf("payload over SSM differs from the SSH path's (%d vs %d bytes)", len(spored.body), len(want))
			}
			if spored.auth != "Bearer tok-on-instance" {
				t.Errorf("Authorization = %q; want the token read ON the instance", spored.auth)
			}
			if extra > 0 && len(sb.scripts) < 2 {
				t.Errorf("a %d-byte request went in %d SendCommand(s); it must be staged in chunks", len(want), len(sb.scripts))
			}
			for i, s := range sb.scripts {
				if len(s) > 2*ssmPushChunk {
					t.Errorf("SendCommand %d is %d bytes, over the chunk budget", i, len(s))
				}
				if strings.Contains(s, "tok-on-instance") {
					t.Errorf("SendCommand %d carries the push token; it must stay on the instance", i)
				}
			}
			if left, _ := filepath.Glob(filepath.Join(sb.dir, "spawn-push-*")); len(left) != 0 {
				t.Errorf("staging files left on the instance: %v", left)
			}
		})
	}
}

func TestPluginPushOverSSMReportsTheAnswer(t *testing.T) {
	spored := &recordingSpored{status: http.StatusOK, answer: `{"name":"demo","status":"running"}`}
	sb := newSSMSandbox(t, spored.server(t).URL)
	push := ssmPush{region: "r", instanceID: "i", run: sb.run}
	st, err := remotePluginStatus(context.Background(), push, "i", "demo")
	if err != nil || st.Status != plugin.StatusRunning {
		t.Fatalf("status over SSM = %+v, %v", st, err)
	}
	if spored.method != http.MethodGet || spored.path != "/v1/plugins/demo/status" {
		t.Errorf("spored got %s %s", spored.method, spored.path)
	}
	// A non-2xx answer is an answer, carried through, not a transport error.
	spored.status, spored.answer = http.StatusPreconditionFailed, `{"error":"docker missing"}`
	if err := pluginPreflightVia(context.Background(), push, []byte(`{}`)); !errors.Is(err, errRemoteCondition) || !strings.Contains(err.Error(), "docker missing") {
		t.Errorf("preflight 412 over SSM = %v", err)
	}
}

func TestParseSSMPushOutputRefusesATruncatedAnswer(t *testing.T) {
	if _, _, err := parseSSMPushOutput("SPAWN_PUSH_STATUS 200 50\n{\"short\":1}"); err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Errorf("truncated body = %v", err)
	}
	if code, body, err := parseSSMPushOutput("SPAWN_PUSH_STATUS 202 2\n{}"); err != nil || code != 202 || string(body) != "{}" {
		t.Errorf("= %d %q %v", code, body, err)
	}
	if _, _, err := parseSSMPushOutput("SPAWN_PUSH_STATUS 000 0\n"); !errors.Is(err, errPushUnreachable) {
		t.Errorf("no HTTP answer = %v, want errPushUnreachable", err)
	}
}

type fakePush struct {
	name   string
	status int
	err    error
	calls  int
}

func (f *fakePush) via() string { return f.name }
func (f *fakePush) call(context.Context, string, string, []byte) (int, []byte, error) {
	f.calls++
	return f.status, nil, f.err
}

func TestPluginPushFallsBackToSSMOnlyWhenSSHCannotReachTheAPI(t *testing.T) {
	ctx := context.Background()

	// SSH cannot reach spored: SSM carries the call, and the later ones.
	ssh := &fakePush{name: "SSH", err: errors.Join(errPushUnreachable, errors.New("Permission denied (publickey)"))}
	ssm := &fakePush{name: "SSM", status: http.StatusAccepted}
	f := &fallbackPush{primary: ssh, secondary: ssm}
	if status, _, err := f.call(ctx, http.MethodPost, "/v1/plugins/install", nil); err != nil || status != http.StatusAccepted {
		t.Fatalf("fallback = %d, %v", status, err)
	}
	_, _, _ = f.call(ctx, http.MethodGet, "/v1/plugins/demo/status", nil)
	if ssh.calls != 1 || ssm.calls != 2 || f.via() != "SSM" {
		t.Errorf("ssh calls %d, ssm calls %d, via %s; want SSH tried once, then SSM", ssh.calls, ssm.calls, f.via())
	}

	// SSH reached spored and it answered (even with an error): no second
	// install over SSM.
	ssh = &fakePush{name: "SSH", status: http.StatusInternalServerError}
	ssm = &fakePush{name: "SSM", status: http.StatusAccepted}
	f = &fallbackPush{primary: ssh, secondary: ssm}
	if err := remotePluginInstall(ctx, f, testSpec(t, 0), nil, nil, nil); err == nil || ssm.calls != 0 {
		t.Errorf("an answered SSH install was retried over SSM (ssm calls %d, err %v)", ssm.calls, err)
	}

	// Both fail: an error naming both, so the command exits non-zero.
	ssh = &fakePush{name: "SSH", err: errors.Join(errPushUnreachable, errors.New("ssh: connect timed out"))}
	ssm = &fakePush{name: "SSM", err: errors.Join(errPushUnreachable, errors.New("InvalidInstanceId"))}
	err := remotePluginInstall(ctx, &fallbackPush{primary: ssh, secondary: ssm}, testSpec(t, 0), nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "SSH:") || !strings.Contains(err.Error(), "SSM:") {
		t.Errorf("both transports failed: err = %v, want one naming both", err)
	}
}

func TestChoosePushTransport(t *testing.T) {
	runner := func(context.Context, string) (func(context.Context, string, string, string) (*aws.SSMRunResult, error), error) {
		return func(context.Context, string, string, string) (*aws.SSMRunResult, error) { return nil, nil }, nil
	}
	ctx := context.Background()
	if p, _ := choosePushTransport(ctx, nil, "me@host", "", "", runner); p.via() != "SSH" {
		t.Errorf("unresolvable raw host: %s, want SSH", p.via())
	}
	withIP := &aws.InstanceInfo{InstanceID: "i-1", Region: "r", PublicIP: "203.0.113.5"}
	p, _ := choosePushTransport(ctx, withIP, "i-1", "", "", runner)
	fb, ok := p.(*fallbackPush)
	if !ok || fb.primary.via() != "SSH" || fb.secondary.via() != "SSM" {
		t.Errorf("public IP: %T %v; want SSH first, SSM as fallback", p, p.via())
	}
	if p, _ := choosePushTransport(ctx, keylessInstance(), "i-0keyless", "", "", runner); p.via() != "SSM" {
		t.Errorf("no public IP: %s, want SSM", p.via())
	}
}
