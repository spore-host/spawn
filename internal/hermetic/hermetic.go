// Package hermetic makes a unit test that reaches the network FAIL instead of
// quietly passing (#968).
//
// TestNewAgent_StillActivates called the real agent.NewAgent, which built a real
// DNS client and a real DynamoDB registry from the AMBIENT credentials. On a
// laptop with a profile configured, every `make check` sent a real Route53 UPSERT
// through the spore.host DNS API, plus EC2 CreateTags and DynamoDB PutItem. In CI,
// with no credentials, the same calls failed — and the test passed anyway,
// because NewAgent logs those failures as warnings. A test that passes whether or
// not it is isolated cannot tell you that it is not.
//
// The environment this package installs:
//
//   - strips every AWS_* variable (profiles, keys, role/web-identity and
//     container-credential settings, per-service endpoint overrides) and sets
//     obviously fake static credentials, AWS_CONFIG_FILE and
//     AWS_SHARED_CREDENTIALS_FILE to /dev/null, and AWS_EC2_METADATA_DISABLED —
//     so nothing can authenticate as a real principal even if a request escapes;
//   - points AWS_ENDPOINT_URL at a loopback recorder, which catches AWS SDK
//     clients whatever HTTP client they were built with;
//   - points HTTP_PROXY/HTTPS_PROXY at the same recorder, with NO_PROXY reduced
//     to loopback, which catches everything built on net/http's default proxy
//     handling — in particular the spore.host DNS client (pkg/dns), which POSTs
//     to a Lambda Function URL with a plain http.Client and is not an SDK client
//     at all — plus IMDS and child processes such as the aws CLI. Go never
//     proxies loopback, so httptest servers and the Substrate emulator keep
//     working.
//
// Any request the recorder sees is refused (HTTP 403) and fails the run, named.
//
// Two entry points share it: test/hermetic, a `go test -exec` wrapper that
// `make check`, CI and scripts/nested-modules.sh run every short test under, and
// Main, for a TestMain in a package whose code builds real clients by default
// (pkg/agent), so that package is hermetic even under a plain `go test`.
package hermetic

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
)

// EnvMarker is set to 1 in a hermetic environment.
const EnvMarker = "SPAWN_HERMETIC_TEST"

const loopback = "localhost,127.0.0.1,::1"

// Env returns base with every AWS_* and proxy variable removed and the hermetic
// settings applied. recorderURL is the loopback recorder's base URL.
func Env(base []string, recorderURL string) []string {
	var env []string
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if !dropped(k) {
			env = append(env, kv)
		}
	}
	return append(env, settings(recorderURL)...)
}

func dropped(k string) bool {
	u := strings.ToUpper(k)
	return strings.HasPrefix(u, "AWS_") ||
		u == "HTTP_PROXY" || u == "HTTPS_PROXY" || u == "ALL_PROXY" || u == "NO_PROXY"
}

func settings(recorderURL string) []string {
	return []string{
		"AWS_ACCESS_KEY_ID=hermetic-test-key-id",
		"AWS_SECRET_ACCESS_KEY=hermetic-test-not-a-real-secret",
		"AWS_CONFIG_FILE=/dev/null",
		"AWS_SHARED_CREDENTIALS_FILE=/dev/null",
		"AWS_EC2_METADATA_DISABLED=true",
		"AWS_ENDPOINT_URL=" + recorderURL,
		"HTTP_PROXY=" + recorderURL,
		"HTTPS_PROXY=" + recorderURL,
		"http_proxy=" + recorderURL,
		"https_proxy=" + recorderURL,
		// Loopback only. Go never proxies loopback whatever this says, but child
		// processes (curl in an installer script under test) do, and their
		// httptest targets are local by construction.
		"NO_PROXY=" + loopback,
		"no_proxy=" + loopback,
		EnvMarker + "=1",
	}
}

// Recorder is a loopback HTTP server that records and refuses every request.
type Recorder struct {
	URL string

	srv  *http.Server
	mu   sync.Mutex
	seen []string
}

// Start listens on a loopback port.
func Start() (*Recorder, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	r := &Recorder{URL: "http://" + ln.Addr().String()}
	r.srv = &http.Server{Handler: r}
	go func() { _ = r.srv.Serve(ln) }()
	return r, nil
}

// Close stops the server.
func (r *Recorder) Close() { _ = r.srv.Close() }

var (
	// Credential=AKID/20260101/us-west-2/dynamodb/aws4_request -> dynamodb
	sigV4Service = regexp.MustCompile(`Credential=[^/]+/\d+/[^/]+/([^/]+)/aws4_request`)
	queryAction  = regexp.MustCompile(`(?:^|&)Action=([A-Za-z0-9]+)`)
)

func (r *Recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	d := describe(req)
	r.mu.Lock()
	r.seen = append(r.seen, d)
	r.mu.Unlock()
	http.Error(w, "hermetic test environment: network access refused (#968)", http.StatusForbidden)
}

// describe renders a request so the failure says what was attempted: the
// CONNECT target for proxied HTTPS, else the URL plus the AWS service and
// operation when they can be read off the request.
func describe(req *http.Request) string {
	if req.Method == http.MethodConnect {
		return "HTTPS CONNECT " + req.Host
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s", req.Method, req.URL.String())
	if req.URL.Host == "" {
		b.WriteString(" (via AWS_ENDPOINT_URL)")
	}
	if m := sigV4Service.FindStringSubmatch(req.Header.Get("Authorization")); m != nil {
		fmt.Fprintf(&b, " service=%s", m[1])
	}
	if t := req.Header.Get("X-Amz-Target"); t != "" {
		fmt.Fprintf(&b, " target=%s", t)
	}
	if req.Body != nil {
		body, _ := io.ReadAll(io.LimitReader(req.Body, 4096))
		if m := queryAction.FindStringSubmatch(string(body)); m != nil {
			fmt.Fprintf(&b, " action=%s", m[1])
		}
	}
	return b.String()
}

// Requests returns what the recorder has seen, sorted.
func (r *Recorder) Requests() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]string(nil), r.seen...)
	sort.Strings(out)
	return out
}

// Report writes the failure for a run that reached the recorder and reports
// whether there was anything to report.
func (r *Recorder) Report(w io.Writer, who string) bool {
	reqs := r.Requests()
	if len(reqs) == 0 {
		return false
	}
	fmt.Fprintf(w, "\nHERMETIC GATE FAILED (#968): %s made %d network request(s) that would have left this machine.\n", who, len(reqs))
	fmt.Fprintln(w, "A unit test must not reach AWS or the spore.host DNS API: with real credentials these are")
	fmt.Fprintln(w, "real control-plane calls. Inject a fake (see the newDNSClient/newPeerRegistry/newTagPutter")
	fmt.Fprintln(w, "seams in pkg/agent), or point the client at an httptest server or the Substrate emulator.")
	for _, q := range reqs {
		fmt.Fprintf(w, "  - %s\n", q)
	}
	return true
}

// Main is a TestMain body: it installs the hermetic environment in this process,
// runs the tests, and fails the binary if any of them reached the recorder.
//
//	func TestMain(m *testing.M) { hermetic.Main(m) }
func Main(m *testing.M) {
	os.Exit(Run(m))
}

// Run is Main without the os.Exit, for a TestMain that has its own setup.
func Run(m *testing.M) int {
	rec, err := Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hermetic: %v\n", err)
		return 2
	}
	defer rec.Close()
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); dropped(k) {
			_ = os.Unsetenv(k)
		}
	}
	for _, kv := range settings(rec.URL) {
		k, v, _ := strings.Cut(kv, "=")
		_ = os.Setenv(k, v)
	}
	code := m.Run()
	if rec.Report(os.Stderr, "this test binary") {
		return 1
	}
	return code
}
