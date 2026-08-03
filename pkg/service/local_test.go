package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// buildFakeService compiles a tiny HTTP service that honors the readiness
// contract, and returns its path.
//
// A compiled Go binary rather than a shell script because the interesting
// behaviors are a real listener on an OS-assigned port and a process that stays
// alive after announcing — and because this mirrors what a real workload is.
func buildFakeService(t *testing.T, src string) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain available to build the fixture")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module fakesvc\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "fakesvc")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	// Build from inside the fixture's own module: `go build <dir>` from the
	// spawn module refuses a directory outside it ("outside main module or its
	// selected dependencies").
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("building fixture: %v\n%s", err, out)
	}
	return bin
}

// fakeServiceSrc is a well-behaved service: binds the requested addr, listens
// BEFORE announcing (so :0 resolves to a reportable port), prints one readiness
// line to stdout, logs to stderr, and serves a trivial value it can be driven to
// change.
const fakeServiceSrc = `package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "listen address")
	token := flag.String("token", "", "access token")
	chatter := flag.Bool("chatter", false, "print to stdout before and after readiness")
	flag.Parse()

	if *chatter {
		fmt.Println("startup chatter, before the listener binds")
	}

	// Listen first: an :0 request only resolves to a concrete port here.
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("server: listening on %s: %v", *addr, err)
	}

	var mu sync.Mutex
	value := 1

	mux := http.NewServeMux()
	mux.HandleFunc("/value", func(w http.ResponseWriter, r *http.Request) {
		if *token != "" && r.Header.Get("X-Token") != *token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(w, "%d", value)
	})
	mux.HandleFunc("/set", func(w http.ResponseWriter, r *http.Request) {
		if *token != "" && r.Header.Get("X-Token") != *token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body struct{ Value int }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		value = body.Value
		mu.Unlock()
		if *chatter {
			fmt.Println("post-readiness output: value is now", body.Value)
		}
		w.WriteHeader(http.StatusNoContent)
	})

	ready := map[string]any{
		"event":      "ready",
		"addr":       ln.Addr().String(),
		"provenance": map[string]string{"sourceHash": "fixture"},
	}
	if *token != "" {
		ready["token"] = *token
	}
	b, _ := json.Marshal(ready)
	fmt.Println(string(b))
	log.Printf("serving on http://%s", ln.Addr().String())

	if err := http.Serve(ln, mux); err != nil {
		os.Exit(1)
	}
}
`

func TestRunLocalReadinessAndDrive(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a binary; skipped in -short mode")
	}
	bin := buildFakeService(t, fakeServiceSrc)

	// --chatter makes the child print to stdout before AND after the readiness
	// line, which is what a real workload does.
	svc, err := RunLocal(context.Background(), LocalOptions{
		Path:        bin,
		Args:        []string{"--chatter"},
		BootTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunLocal: %v", err)
	}
	defer func() { _ = svc.Stop() }()

	// The reported port must be real, not the :0 we asked for.
	if strings.HasSuffix(svc.Addr(), ":0") {
		t.Fatalf("addr %q — :0 must resolve to a concrete port", svc.Addr())
	}

	// Acceptance criterion 2 from the issue: driving the service takes effect.
	base := "http://" + svc.Addr()
	if got := httpGet(t, base+"/value", ""); got != "1" {
		t.Fatalf("initial value = %q, want 1", got)
	}
	httpPost(t, base+"/set", `{"value":42}`, "", http.StatusNoContent)
	if got := httpGet(t, base+"/value", ""); got != "42" {
		t.Fatalf("after drive value = %q, want 42 — the service was not actually driven", got)
	}
}

func TestRunLocalTokenIsCarriedNotLogged(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a binary; skipped in -short mode")
	}
	bin := buildFakeService(t, fakeServiceSrc)

	const token = "6becc7e0d5eebf8e9130cd61759693cb"
	var stdoutSink, stderrSink bytes.Buffer
	svc, err := RunLocal(context.Background(), LocalOptions{
		Path:        bin,
		Args:        []string{"--token", token},
		BootTimeout: 30 * time.Second,
		Stdout:      &stdoutSink,
		Stderr:      &stderrSink,
	})
	if err != nil {
		t.Fatalf("RunLocal: %v", err)
	}
	defer func() { _ = svc.Stop() }()

	if svc.Ready.Token != token {
		t.Errorf("token = %q, want it carried off the readiness line", svc.Ready.Token)
	}
	// The endpoint really is protected, so the token is load-bearing rather than
	// decorative.
	if code := httpGetCode(t, "http://"+svc.Addr()+"/value", ""); code != http.StatusUnauthorized {
		t.Errorf("unauthenticated GET = %d, want 401", code)
	}
	if got := httpGet(t, "http://"+svc.Addr()+"/value", token); got != "1" {
		t.Errorf("authenticated GET = %q, want 1", got)
	}
	// URL() may carry the credential (it's for the user's terminal); Addr() must
	// not, because that's what callers log.
	if !strings.Contains(svc.URL(), token) {
		t.Error("URL() should include the token so the user can actually connect")
	}
	if strings.Contains(svc.Addr(), token) {
		t.Error("Addr() must not carry the credential — it is the loggable form")
	}
}

func TestRunLocalBindFailureIsDiagnosed(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a binary; skipped in -short mode")
	}
	bin := buildFakeService(t, fakeServiceSrc)

	// Occupy a port, then demand it. Observed against the real workload: exit 1,
	// empty stdout, an actionable message on stderr.
	taken := listen(t)

	_, err := RunLocal(context.Background(), LocalOptions{
		Path:        bin,
		AddrValue:   taken,
		BootTimeout: 30 * time.Second,
	})
	if err == nil {
		t.Fatal("RunLocal on an occupied port should fail")
	}
	var re *ReadinessError
	if !errors.As(err, &re) {
		t.Fatalf("err is not a *ReadinessError: %T %v", err, err)
	}
	if re.Cause != FailureExited {
		t.Errorf("Cause = %v, want FailureExited", re.Cause)
	}
	if !strings.Contains(err.Error(), "address already in use") {
		t.Errorf("error should surface the child's own diagnosis:\n%s", err)
	}
}

func TestRunLocalNeverLeaksOnFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a binary; skipped in -short mode")
	}
	// A child that runs forever without announcing readiness. Cost control is
	// existential here: the remote analog of this leak is a billable instance.
	src := `package main

import (
	"flag"
	"time"
)

func main() {
	flag.String("addr", "", "ignored")
	flag.Parse()
	time.Sleep(10 * time.Minute)
}
`
	bin := buildFakeService(t, src)

	start := time.Now()
	_, err := RunLocal(context.Background(), LocalOptions{
		Path:        bin,
		BootTimeout: 800 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("a child that never announces readiness must fail")
	}
	// RunLocal must not return until the child is reaped — otherwise "failed"
	// still leaves a process running.
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("took %s — RunLocal should kill and reap before returning", elapsed)
	}
	var re *ReadinessError
	if errors.As(err, &re) && re.Cause != FailureTimeout {
		t.Errorf("Cause = %v, want FailureTimeout", re.Cause)
	}
	if !pgrepQuiet(t, filepath.Base(bin)) {
		t.Error("the child is still running — RunLocal leaked a process on failure")
	}
}

func TestRunLocalStopIsIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a binary; skipped in -short mode")
	}
	bin := buildFakeService(t, fakeServiceSrc)
	svc, err := RunLocal(context.Background(), LocalOptions{Path: bin, BootTimeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("RunLocal: %v", err)
	}
	if err := svc.Stop(); err != nil {
		t.Errorf("first Stop: %v", err)
	}
	// A second stop, and a stop of an already-dead child, must both be no-ops.
	// A reaper that errors on "already gone" is a reaper people stop trusting.
	if err := svc.Stop(); err != nil {
		t.Errorf("second Stop: %v", err)
	}
	if pgrepQuiet(t, filepath.Base(bin)) == false {
		t.Error("child survived Stop")
	}
}

func TestRunLocalRejectsMissingPath(t *testing.T) {
	if _, err := RunLocal(context.Background(), LocalOptions{}); err == nil {
		t.Error("RunLocal with no Path should fail")
	}
}

func TestRunLocalStdoutKeepsDraining(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a binary; skipped in -short mode")
	}
	bin := buildFakeService(t, fakeServiceSrc)
	var mu sync.Mutex
	sink := &syncBuf{mu: &mu}
	svc, err := RunLocal(context.Background(), LocalOptions{
		Path:        bin,
		Args:        []string{"--chatter"},
		BootTimeout: 30 * time.Second,
		Stdout:      sink,
	})
	if err != nil {
		t.Fatalf("RunLocal: %v", err)
	}
	defer func() { _ = svc.Stop() }()

	// Post-readiness output must reach the caller's writer, which is also what
	// proves the pipe keeps being drained (an undrained pipe blocks the child).
	httpPost(t, "http://"+svc.Addr()+"/set", `{"value":7}`, "", http.StatusNoContent)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(sink.String(), "value is now 7") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("post-readiness stdout never reached the writer:\n%s", sink.String())
}

// --- helpers ---

type syncBuf struct {
	mu *sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func httpGet(t *testing.T, url, token string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("X-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func httpGetCode(t *testing.T, url, token string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("X-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func httpPost(t *testing.T, url, body, token string, wantCode int) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantCode {
		t.Fatalf("POST %s = %d, want %d", url, resp.StatusCode, wantCode)
	}
}

// pgrepQuiet reports whether NO process matching name is running. Best-effort:
// on a platform without pgrep it reports success rather than failing the test
// for an unrelated reason.
func pgrepQuiet(t *testing.T, name string) bool {
	t.Helper()
	if _, err := exec.LookPath("pgrep"); err != nil {
		return true
	}
	// Give the OS a moment to reap.
	time.Sleep(200 * time.Millisecond)
	out, _ := exec.Command("pgrep", "-f", name).CombinedOutput()
	if s := strings.TrimSpace(string(out)); s != "" {
		fmt.Printf("pgrep %s found: %s\n", name, s)
		return false
	}
	return true
}
