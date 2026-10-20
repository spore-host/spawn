package cmd

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/security"
)

// spored's plugin push API listens on the instance's loopback only, with a
// per-start bearer token in /var/lib/spored/push.token
// (pkg/pluginruntime/pushapi.go). It is never reachable from off the box, so
// every call reaches it from ON the box: either through an SSH port-forward,
// or — for an instance with no SSH path (keyless, SSM-only, no public IP) — by
// running curl on the instance over SSM SendCommand, the machinery `spawn
// extend` uses for the same reason (#1113). The token never leaves the
// instance on the SSM path (#855).

// pluginTransportFor is the transport plugin commands use; a var so tests can
// substitute a recording one.
var pluginTransportFor = pluginPushTransport

const (
	pushAPILoopback  = "http://127.0.0.1:7777"
	pushAPITokenPath = "/var/lib/spored/push.token"
)

// pushTransport makes one push-API call: method and path (e.g.
// "/v1/plugins/install") with an optional JSON body. It returns the HTTP
// status and body. It authenticates itself; callers never see the token.
type pushTransport interface {
	call(ctx context.Context, method, path string, body []byte) (status int, respBody []byte, err error)
	via() string
}

// errPushUnreachable marks a failure to REACH the push API at all — no SSH,
// no tunnel, connection refused — as opposed to an answer from it. Only this
// kind of failure may be retried over another transport: an answered request
// was received, and repeating an install would run it twice.
var errPushUnreachable = errors.New("push API unreachable")

// httpPush calls the push API directly at baseURL with a known token: inside
// an SSH tunnel, or a test server.
type httpPush struct {
	client  *http.Client
	baseURL string
	token   string
}

func (h httpPush) via() string { return "HTTP" }

func (h httpPush) call(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.baseURL+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.client.Do(req)
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return 0, nil, fmt.Errorf("%w: %v", errPushUnreachable, err)
		}
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, out, err
}

// sshPush reaches the push API through an SSH port-forward to host
// (user@ip), reading the token over SSH. It is what plugin commands always
// used.
type sshPush struct {
	host    string
	keyPath string
}

func (s sshPush) via() string { return "SSH" }

func (s sshPush) call(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	token, err := sshReadToken(ctx, s.host, s.keyPath)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %v", errPushUnreachable, err)
	}
	var status int
	var out []byte
	_, err = withSSHTunnel(ctx, s.host, s.keyPath, func() (*http.Response, error) {
		var cerr error
		status, out, cerr = httpPush{client: http.DefaultClient, baseURL: pushAPILoopback, token: token}.call(ctx, method, path, body)
		return nil, cerr
	})
	return status, out, err
}

// ssmPushChunk is the most base64 a single SendCommand carries. SSM documents
// are capped at 64 KB and the per-parameter limit is not documented, so this
// stays well under; a larger request body is staged in several commands.
const ssmPushChunk = 16 << 10

// ssmPush reaches the push API by running curl on the instance over SSM.
type ssmPush struct {
	region     string
	instanceID string
	run        func(ctx context.Context, region, instanceID, script string) (*aws.SSMRunResult, error)
	// nonce names the on-instance staging files; random in production.
	nonce func() string
}

func (s ssmPush) via() string { return "SSM" }

func randomNonce() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ssmPushScripts renders the SendCommand scripts for one call: zero or more
// that stage the base64 body in a root-only file, then one that decodes it,
// reads the token on the instance and makes the loopback request. The last
// prints "SPAWN_PUSH_STATUS <code> <bytes>" and then the response body.
func ssmPushScripts(nonce, method, path string, body []byte) []string {
	stage := "/run/spawn-push-" + nonce
	enc := base64.StdEncoding.EncodeToString(body)
	var chunks []string
	for len(enc) > ssmPushChunk {
		chunks = append(chunks, enc[:ssmPushChunk])
		enc = enc[ssmPushChunk:]
	}
	chunks = append(chunks, enc)

	var scripts []string
	for _, c := range chunks[:len(chunks)-1] {
		scripts = append(scripts, fmt.Sprintf("umask 077\nprintf '%%s' %s >> %s\n", security.ShellQuote(c), stage+".b64"))
	}
	data := ""
	if body != nil {
		data = ` --data-binary @"$F.req" -H 'Content-Type: application/json'`
	}
	scripts = append(scripts, fmt.Sprintf(`set -u
umask 077
F=%s
cleanup() { rm -f "$F.b64" "$F.req" "$F.hdr" "$F.resp"; }
printf '%%s' %s >> "$F.b64"
if ! base64 -d < "$F.b64" > "$F.req"; then echo "spawn-push: could not decode the request" >&2; cleanup; exit 2; fi
if ! TOKEN=$(cat %s); then echo "spawn-push: cannot read the push token (is spored running?)" >&2; cleanup; exit 2; fi
printf 'Authorization: Bearer %%s\n' "$TOKEN" > "$F.hdr"
CODE=$(curl -sS --noproxy '*' --max-time 60 -o "$F.resp" -w '%%{http_code}' -X %s -H @"$F.hdr"%s %s)
RC=$?
if [ "$RC" -ne 0 ]; then echo "spawn-push: curl to the push API failed (exit $RC)" >&2; cleanup; exit 3; fi
echo "SPAWN_PUSH_STATUS $CODE $(wc -c < "$F.resp" | tr -d ' ')"
cat "$F.resp"
cleanup
`, stage, security.ShellQuote(chunks[len(chunks)-1]), pushAPITokenPath, method, data, security.ShellQuote(pushAPILoopback+path)))
	return scripts
}

// parseSSMPushOutput reads the final script's stdout back into a status and
// body. SSM truncates StandardOutputContent (24,000 characters), so the body
// length is carried on the status line and a short body is an error, not a
// silently partial answer.
func parseSSMPushOutput(stdout string) (int, []byte, error) {
	head, rest, _ := strings.Cut(stdout, "\n")
	f := strings.Fields(head)
	if len(f) != 3 || f[0] != "SPAWN_PUSH_STATUS" {
		return 0, nil, fmt.Errorf("unexpected SSM output: %q", truncateForError(stdout))
	}
	code, err := strconv.Atoi(f[1])
	if err != nil || code == 0 {
		return 0, nil, fmt.Errorf("%w: push API did not answer on the instance (HTTP %q)", errPushUnreachable, f[1])
	}
	n, err := strconv.Atoi(f[2])
	if err != nil {
		return 0, nil, fmt.Errorf("unexpected SSM output: %q", head)
	}
	if len(rest) < n {
		return code, nil, fmt.Errorf("push API response (%d bytes) was truncated by SSM to %d", n, len(rest))
	}
	return code, []byte(rest[:n]), nil
}

func truncateForError(s string) string {
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

func (s ssmPush) call(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	nonce := randomNonce
	if s.nonce != nil {
		nonce = s.nonce
	}
	scripts := ssmPushScripts(nonce(), method, path, body)
	var res *aws.SSMRunResult
	for i, script := range scripts {
		r, err := s.run(ctx, s.region, s.instanceID, script)
		if err != nil {
			return 0, nil, fmt.Errorf("%w: SSM: %v", errPushUnreachable, err)
		}
		if r == nil || r.Status != "Success" || r.ResponseCode != 0 {
			detail := "no result"
			if r != nil {
				detail = fmt.Sprintf("status %s, exit %d: %s", r.Status, r.ResponseCode, strings.TrimSpace(r.Stderr))
			}
			return 0, nil, fmt.Errorf("%w: SSM command %d/%d on %s: %s", errPushUnreachable, i+1, len(scripts), s.instanceID, detail)
		}
		res = r
	}
	return parseSSMPushOutput(res.Stdout)
}

// fallbackPush tries SSH, and moves to SSM — for this call and every later
// one — when SSH cannot reach the push API. An ANSWER over SSH, error or not,
// is returned as is.
type fallbackPush struct {
	primary, secondary pushTransport
	usingSecondary     bool
	sshErr             error
}

func (f *fallbackPush) via() string {
	if f.usingSecondary {
		return f.secondary.via()
	}
	return f.primary.via()
}

func (f *fallbackPush) call(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	if !f.usingSecondary {
		status, out, err := f.primary.call(ctx, method, path, body)
		if err == nil || !errors.Is(err, errPushUnreachable) {
			return status, out, err
		}
		f.sshErr = err
		f.usingSecondary = true
	}
	status, out, err := f.secondary.call(ctx, method, path, body)
	if err != nil && f.sshErr != nil {
		return status, out, fmt.Errorf("SSH: %v; SSM: %w", f.sshErr, err)
	}
	return status, out, err
}

// pluginPushTransport picks how to reach instance's push API. A value spawn
// cannot resolve is a raw host (user@host), reachable only by SSH, as before.
// A resolved instance with a public IP is tried over SSH first, falling back
// to SSM; one without a public IP goes straight to SSM (#855). Before, plugin
// commands needed SSH unconditionally, so a keyless SSM-only instance — the
// Windows-free half of what spawn launches keyless — could not take a plugin.
func pluginPushTransport(ctx context.Context, instance string) (pushTransport, error) {
	return choosePushTransport(ctx, resolveInstanceViaSpawn(ctx, instance), instance, pluginSSHUser, pluginKeyPath, newSSMPushRunner)
}

func newSSMPushRunner(ctx context.Context, region string) (func(ctx context.Context, region, instanceID, script string) (*aws.SSMRunResult, error), error) {
	client, err := aws.NewClientWithRegion(ctx, region)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, region, instanceID, script string) (*aws.SSMRunResult, error) {
		return client.RunShellScript(ctx, region, instanceID, script, 90*time.Second)
	}, nil
}

func choosePushTransport(ctx context.Context, inst *aws.InstanceInfo, instance, user, keyPath string,
	ssmRunner func(ctx context.Context, region string) (func(ctx context.Context, region, instanceID, script string) (*aws.SSMRunResult, error), error)) (pushTransport, error) {
	if inst == nil || inst.InstanceID == "" {
		return sshPush{host: instance, keyPath: keyPath}, nil
	}
	run, err := ssmRunner(ctx, inst.Region)
	if err != nil {
		return nil, fmt.Errorf("aws client for SSM: %w", err)
	}
	ssm := ssmPush{region: inst.Region, instanceID: inst.InstanceID, run: run}
	if inst.PublicIP == "" {
		return ssm, nil
	}
	if user == "" {
		user = "ec2-user"
	}
	return &fallbackPush{primary: sshPush{host: user + "@" + inst.PublicIP, keyPath: keyPath}, secondary: ssm}, nil
}
