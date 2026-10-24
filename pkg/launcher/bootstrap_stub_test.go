package launcher

import (
	"bytes"
	"compress/gzip"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"testing"

	"github.com/spore-host/spawn/internal/shellmin"
	"github.com/spore-host/spawn/pkg/plugin"
	"github.com/spore-host/spawn/pkg/sporedinstall"
	"mvdan.cc/sh/v3/syntax"
)

// These are the gates for #1274: a release build's user data carries a stub in
// place of the static bootstrap body, and fetches, verifies and runs the body
// that release published.

const testRelease = "9.9.9"

// stubConfigs are the launch shapes the stub must reproduce, one for each kind
// of per-launch content and its position relative to the body.
func stubConfigs() map[string]BootstrapConfig {
	return map[string]BootstrapConfig{
		"plain": {Username: "ec2-user"},
		"key and command": {
			Username: "ec2-user", PublicKey: []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample u@h"),
			Command: "echo hi", CompletionFile: "/data/done",
		},
		"task with flush hook": {
			Username: "ec2-user", Command: "/opt/task/wrapper.sh",
			TaskFlushScript: "#!/bin/bash\necho flush\n",
		},
		"plugins": {
			Username: "ec2-user",
			Plugins:  []plugin.Declaration{{Ref: "github:spore-host/spore-plugins/tailscale", Config: map[string]string{"k": "v"}}},
		},
		"storage and gates": {
			Username: "ec2-user", Command: "ls /efs", StorageScript: "mount -t efs fs-1 /efs\n",
			ReadyMountPoints: []string{"/efs"}, ReadyGates: []string{MPIReadyGate},
		},
		"container":        {Username: "ec2-user", ContainerScript: "docker run hello\n"},
		"custom user data": {Username: "ec2-user", CustomUserData: "#!/bin/bash\necho custom\n"},
	}
}

// hoistedTree parses src as bash and prints it back minified with every
// top-level function definition moved, in order, ahead of the other commands.
// Defining a function runs nothing, so where a definition sits changes what a
// script does only if something before it calls the function; the test below
// checks that nothing does.
func hoistedTree(t *testing.T, name, src string) string {
	t.Helper()
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(src), name)
	if err != nil {
		t.Fatalf("%s does not parse as bash: %v", name, err)
	}
	var funcs, rest []*syntax.Stmt
	for _, st := range f.Stmts {
		if _, ok := st.Cmd.(*syntax.FuncDecl); ok {
			funcs = append(funcs, st)
		} else {
			rest = append(rest, st)
		}
	}
	f.Stmts = append(funcs, rest...)
	var b strings.Builder
	if err := syntax.NewPrinter(syntax.Minify(true)).Print(&b, f); err != nil {
		t.Fatalf("print %s: %v", name, err)
	}
	return b.String()
}

// TestStubWithThePublishedBodyIsTheInlineBootstrap is the behaviour gate. The
// stub's fetch-and-verify block is replaced by the body the release publishes
// (BootstrapBody, which `spawn bootstrap-body` prints), and the result must be
// the inline bootstrap for the same launch, command for command: same
// per-launch content, on the same side of the body, in the same order.
func TestStubWithThePublishedBodyIsTheInlineBootstrap(t *testing.T) {
	body := BootstrapBody()
	fetch := bodyStub(testRelease, body)
	for name, cfg := range stubConfigs() {
		t.Run(name, func(t *testing.T) {
			stub, err := buildLinuxBootstrap(cfg, testRelease, body)
			if err != nil {
				t.Fatal(err)
			}
			inline, err := buildLinuxBootstrap(cfg, "", body)
			if err != nil {
				t.Fatal(err)
			}
			if n := strings.Count(stub, fetch); n != 1 {
				t.Fatalf("the stub carries the fetch-and-verify block %d times, want once", n)
			}
			if strings.Contains(stub, shellmin.Strip(linuxBootstrapBody)) {
				t.Fatal("the stub still carries the static body inline")
			}
			spliced := strings.Replace(stub, fetch, body, 1)
			if got, want := hoistedTree(t, "stub+body", spliced), hoistedTree(t, "inline", inline); got != want {
				t.Errorf("the stub plus the published body does not do what the inline bootstrap does\n--- stub+body\n%s\n--- inline\n%s", spliced, inline)
			}

			// The hoisting above is only sound if no command ahead of the body
			// calls one of the body's functions.
			head := stub[:strings.Index(stub, fetch)]
			for _, fn := range []string{sporedinstall.FuncName, sporedinstall.CurlRetryFuncName, sporedinstall.RelabelFuncName, "spawn_is_spot_replacement", "parse_duration"} {
				if strings.Contains(head, fn) {
					t.Errorf("%s is used before the body defines it", fn)
				}
			}
		})
	}
}

// TestStubIsTheDefault: with a release version and no opt-out, every
// BuildLinuxBootstrap gets the stub; InlineBody, a dev build and a build with
// no signing key all get the body inline.
func TestStubIsTheDefault(t *testing.T) {
	prev := CallerVersion
	t.Cleanup(func() { CallerVersion = prev })

	CallerVersion = "0.143.0"
	s, err := BuildLinuxBootstrap(BootstrapConfig{Username: "ec2-user"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s, bodyStub("0.143.0", BootstrapBody())) || strings.Contains(s, "systemctl start spored") {
		t.Error("a release build's default bootstrap is not the stub")
	}

	s, err = BuildLinuxBootstrap(BootstrapConfig{Username: "ec2-user", InlineBody: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(s, "SPAWN_BS_VERSION=") || !strings.Contains(s, "systemctl start spored") {
		t.Error("InlineBody did not put the body inline")
	}

	CallerVersion = "dev"
	s, err = BuildLinuxBootstrap(BootstrapConfig{Username: "ec2-user"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(s, "SPAWN_BS_VERSION=") {
		t.Error("a dev build fetches a body no release published")
	}
}

func TestPublishedBodyVersion(t *testing.T) {
	spawnMain := func(v string) *debug.BuildInfo {
		return &debug.BuildInfo{Main: debug.Module{Path: spawnModulePath, Version: v}}
	}
	asDep := func(v string, replaced bool) *debug.BuildInfo {
		d := &debug.Module{Path: spawnModulePath, Version: v}
		if replaced {
			d.Replace = &debug.Module{Path: "../spawn"}
		}
		return &debug.BuildInfo{Main: debug.Module{Path: "github.com/spore-host/lagotto", Version: "v0.70.0"}, Deps: []*debug.Module{d}}
	}
	cases := []struct {
		name   string
		caller string
		info   *debug.BuildInfo
		keyed  bool
		want   string
	}{
		{"release CLI", "0.143.0", spawnMain("(devel)"), true, "0.143.0"},
		{"v prefix", "v0.143.0", nil, true, "0.143.0"},
		{"dev CLI", "dev", spawnMain("v0.143.0"), true, ""},
		{"dirty CLI", "0.143.0+dirty", nil, true, ""},
		{"pseudo-version CLI", "0.143.1-0.20261023000000-0123456789ab", nil, true, ""},
		{"go install of a tag", "", spawnMain("v0.143.0"), true, "0.143.0"},
		{"plain go build", "", spawnMain("(devel)"), true, ""},
		{"library built against a release", "", asDep("v0.143.0", false), true, "0.143.0"},
		{"library built against a replaced spawn", "", asDep("v0.143.0", true), true, ""},
		{"library built against a pseudo-version", "", asDep("v0.143.1-0.20261023000000-0123456789ab", false), true, ""},
		{"no build info", "", nil, true, ""},
		{"no signing key to verify with", "0.143.0", nil, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := publishedBodyVersion(tc.caller, tc.info, tc.keyed); got != tc.want {
				t.Errorf("publishedBodyVersion(%q) = %q, want %q", tc.caller, got, tc.want)
			}
		})
	}
}

// stubGzipBudget is how big the stub-only user data of a bare launch may be
// once gzipped (#1274). The point of the stub is that the bootstrap stops
// eating a task's 16 KiB; raising this is a decision, not an accident.
const stubGzipBudget = 3 * 1024

func gzipLen(t *testing.T, s string) int {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Len()
}

func TestStubBootstrapFitsItsBudget(t *testing.T) {
	stub, err := buildLinuxBootstrap(BootstrapConfig{Username: "ec2-user"}, "0.143.0", BootstrapBody())
	if err != nil {
		t.Fatal(err)
	}
	inline, err := buildLinuxBootstrap(BootstrapConfig{Username: "ec2-user"}, "", BootstrapBody())
	if err != nil {
		t.Fatal(err)
	}
	if n := gzipLen(t, stub); n > stubGzipBudget {
		t.Errorf("the bare stub bootstrap is %d bytes gzipped, over its %d-byte budget (#1274)", n, stubGzipBudget)
	}
	t.Logf("bare launch: stub %d bytes raw, %d gzipped (budget %d); inline %d raw, %d gzipped; published body %d raw",
		len(stub), gzipLen(t, stub), stubGzipBudget, len(inline), gzipLen(t, inline), len(BootstrapBody()))
}

// TestEveryBootstrapCallerGetsTheStub finds every BuildLinuxBootstrap call and
// every InlineBody setting in the repo's non-test Go source: the stub is the
// default, so the only way a caller can lose it is by opting out, and no caller
// does. Discovered, not listed: a new launcher is covered without editing this.
func TestEveryBootstrapCallerGetsTheStub(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	var calls int
	var optOuts []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "node_modules", "vendor", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				if calleeName(n.Fun) == "BuildLinuxBootstrap" {
					calls++
				}
			case *ast.KeyValueExpr:
				if id, ok := n.Key.(*ast.Ident); ok && id.Name == "InlineBody" {
					optOuts = append(optOuts, fset.Position(n.Pos()).String())
				}
			case *ast.AssignStmt:
				for _, l := range n.Lhs {
					if sel, ok := l.(*ast.SelectorExpr); ok && sel.Sel.Name == "InlineBody" {
						optOuts = append(optOuts, fset.Position(n.Pos()).String())
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The CLI launch paths (through buildUserData), burst, pool and Provision,
	// plus the public wrapper's own call: fewer means the walk has broken.
	if calls < 4 {
		t.Fatalf("found %d BuildLinuxBootstrap calls; the source walk has broken", calls)
	}
	for _, o := range optOuts {
		t.Errorf("%s opts a launcher out of the verified-stub bootstrap (InlineBody); every launcher gets the stub (#1274)", o)
	}
}

func calleeName(e ast.Expr) string {
	switch f := e.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

// stubServer is a stand-in for IMDS and the spawn-binaries buckets.
type stubServer struct {
	srv  *httptest.Server
	mu   sync.Mutex
	objs map[string][]byte // "<region>/<key>" -> content
	hits map[string]int
}

func newStubServer(t *testing.T, region string) *stubServer {
	t.Helper()
	s := &stubServer{objs: map[string][]byte{}, hits: map[string]int{}}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.hits[r.URL.Path]++
		body, ok := s.objs[strings.TrimPrefix(r.URL.Path, "/b/")]
		s.mu.Unlock()
		switch {
		case r.URL.Path == "/imds/latest/api/token":
			body, ok = []byte("tok"), true
		case r.URL.Path == "/imds/latest/meta-data/placement/region":
			body, ok = []byte(region), true
		case r.URL.Path == "/imds/latest/meta-data/instance-id":
			body, ok = []byte("i-0test"), true
		}
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// A script and its signature for curl inside a test; HTML escaping would corrupt both.
		// nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter
		_, _ = w.Write(body)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *stubServer) put(region, key string, b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objs[region+"/"+key] = b
}

func (s *stubServer) hit(region, key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits["/b/"+region+"/"+key]
}

// awsMode is how the stand-in aws CLI behaves in a boot sandbox.
type awsMode int

const (
	awsTerminates awsMode = iota // terminate-instances succeeds
	awsFails                     // every call fails (no permission, no egress)
	awsAbsent                    // no aws CLI on the AMI
)

// bootSandbox runs generated user data under bash with a PATH holding only
// the tools it needs and stand-ins for everything that would touch the
// machine or AWS: sleep is counted, logger, aws, shutdown and poweroff are
// recorded. Nothing real can be terminated, shut down or called.
type bootSandbox struct {
	t   *testing.T
	dir string
	srv *stubServer
}

func newBootSandbox(t *testing.T, srv *stubServer, aws awsMode, standIns map[string]string) *bootSandbox {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Linux-only: executes the Linux user data under bash")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	for _, d := range []string{bin, filepath.Join(dir, "tmp"), filepath.Join(dir, "etc", "spawn")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, tool := range []string{"bash", "curl", "openssl", "base64", "cut", "rm", "mkdir", "cat", "uname", "mktemp", "awk", "chmod", "mv", "dirname", "tr", "sed", "grep", "head", "cp", "shasum", "perl", "env"} {
		if p, err := exec.LookPath(tool); err == nil {
			if err := os.Symlink(p, filepath.Join(bin, tool)); err != nil {
				t.Fatal(err)
			}
		} else if tool == "bash" || tool == "curl" || tool == "openssl" || tool == "base64" {
			t.Skipf("%s not available", tool)
		}
	}
	standInSha256sum(t, bin)
	rec := func(name string) string {
		return "#!/bin/bash\necho \"$*\" >> " + filepath.Join(dir, name) + "\n"
	}
	writeExec(t, filepath.Join(bin, "sleep"), rec("sleeps"))
	writeExec(t, filepath.Join(bin, "logger"), rec("journal"))
	writeExec(t, filepath.Join(bin, "shutdown"), rec("shutdowns"))
	writeExec(t, filepath.Join(bin, "poweroff"), rec("shutdowns"))
	switch aws {
	case awsTerminates:
		writeExec(t, filepath.Join(bin, "aws"), rec("aws-calls")+"exit 0\n")
	case awsFails:
		writeExec(t, filepath.Join(bin, "aws"), rec("aws-calls")+"exit 255\n")
	}
	for name, body := range standIns {
		_ = os.Remove(filepath.Join(bin, name))
		writeExec(t, filepath.Join(bin, name), body)
	}
	return &bootSandbox{t: t, dir: dir, srv: srv}
}

func (sb *bootSandbox) run(script string) (string, error) {
	sb.t.Helper()
	rw := strings.NewReplacer(
		"/etc/spawn", filepath.Join(sb.dir, "etc", "spawn"),
		"/tmp/", filepath.Join(sb.dir, "tmp")+"/",
		"/dev/console", filepath.Join(sb.dir, "console"),
		"/usr/local/bin/spored", filepath.Join(sb.dir, "spored"),
		"http://169.254.169.254", sb.srv.srv.URL+"/imds",
		"https://spawn-binaries-", sb.srv.srv.URL+"/b/",
		".s3.amazonaws.com", "",
	)
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd := exec.Command(filepath.Join(sb.dir, "bin", "bash"), "-c", rw.Replace(script)) //nolint:gosec // this package's own generated script
	cmd.Env = []string{"PATH=" + filepath.Join(sb.dir, "bin"), "HOME=" + sb.dir}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (sb *bootSandbox) read(name string) string { return string(readOr(filepath.Join(sb.dir, name))) }

const selfTerminateCall = "ec2 terminate-instances --region eu-west-1 --instance-ids i-0test"

// assertRetired checks that a bootstrap that could not get spored running
// said why everywhere a person would look, and then terminated the instance
// it runs on (or, with no way to terminate, shut it down).
func assertRetired(t *testing.T, sb *bootSandbox, out string, err error, reason string, aws awsMode) {
	t.Helper()
	if err == nil {
		t.Errorf("the bootstrap exited 0 after a fatal failure\n%s", out)
	}
	for where, b := range map[string]string{"stderr": out, "console": sb.read("console"), "journal": sb.read("journal")} {
		if !strings.Contains(b, reason) {
			t.Errorf("the abort (%q) did not reach the %s: %q", reason, where, b)
		}
	}
	calls := sb.read("aws-calls")
	switch aws {
	case awsTerminates:
		if strings.Count(calls, selfTerminateCall) != 1 {
			t.Errorf("the instance did not terminate itself once; aws calls: %q", calls)
		}
		if sb.read("shutdowns") != "" {
			t.Errorf("shut down after a successful terminate: %q", sb.read("shutdowns"))
		}
	case awsFails:
		if n := strings.Count(calls, selfTerminateCall); n != 3 {
			t.Errorf("the self-terminate was tried %d times, want 3 (bounded retry): %q", n, calls)
		}
		fallthrough
	case awsAbsent:
		if !strings.Contains(sb.read("shutdowns"), "-h now") {
			t.Errorf("with no working terminate, the instance was not shut down; shutdowns: %q", sb.read("shutdowns"))
		}
		if !strings.Contains(out, "STOPS") {
			t.Errorf("the shutdown fallback does not say it only stops the instance\n%s", out)
		}
	}
}

// TestStubRunsOnlyAVerifiedBody executes the real stub under bash, against a
// stand-in bucket and IMDS and the real curl and openssl, with a test signing
// key in place of the spore.host one. The body runs only when it is the exact
// body this build expects AND carries a valid signature; in every other case
// the bootstrap runs nothing from it, says why, and terminates the instance,
// which would otherwise bill with no spored to enforce its TTL.
func TestStubRunsOnlyAVerifiedBody(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	prevKey := sporedinstall.SigningKeyPEM
	t.Cleanup(func() { sporedinstall.SigningKeyPEM = prevKey })
	sporedinstall.SigningKeyPEM = strings.TrimSpace(string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})))
	sign := func(b []byte) []byte {
		sum := sha256.Sum256(b)
		sig, err := ecdsa.SignASN1(rand.Reader, priv, sum[:])
		if err != nil {
			t.Fatal(err)
		}
		return []byte(base64.StdEncoding.EncodeToString(sig) + "\n")
	}

	// The body this build expects, and the user data that expects it. $HOME is
	// the sandbox: the fetched body is not rewritten as the script is.
	body := []byte("echo body > \"$HOME/BODY_RAN\"\n")
	other := []byte("echo other > \"$HOME/BODY_RAN\"\n")
	key := BootstrapBodyKey(testRelease)
	script, err := buildLinuxBootstrap(BootstrapConfig{
		Username: "tester", CustomUserData: "echo post > /tmp/POST_RAN\n",
	}, testRelease, string(body))
	if err != nil {
		t.Fatal(err)
	}
	const reason = "no verified bootstrap body for spawn " + testRelease

	cases := map[string]struct {
		regional, fallback [2][]byte // body, sig; nil = 404
		wantRun            bool
		aws                awsMode
	}{
		"verified regional body runs":                {[2][]byte{body, sign(body)}, [2][]byte{}, true, awsTerminates},
		"tampered regional falls back to us-east-1":  {[2][]byte{other, sign(body)}, [2][]byte{body, sign(body)}, true, awsTerminates},
		"tampered body":                              {[2][]byte{other, sign(body)}, [2][]byte{other, sign(body)}, false, awsTerminates},
		"bad signature":                              {[2][]byte{body, sign(other)}, [2][]byte{body, sign(other)}, false, awsTerminates},
		"another release's body, validly signed":     {[2][]byte{other, sign(other)}, [2][]byte{other, sign(other)}, false, awsTerminates},
		"no signature published":                     {[2][]byte{body, nil}, [2][]byte{body, nil}, false, awsTerminates},
		"nothing published":                          {[2][]byte{}, [2][]byte{}, false, awsTerminates},
		"signature that is not base64 DER at all":    {[2][]byte{body, []byte("not a signature")}, [2][]byte{}, false, awsTerminates},
		"empty regional object, us-east-1 good":      {[2][]byte{{}, {}}, [2][]byte{body, sign(body)}, true, awsTerminates},
		"good body, signature by a key spawn lacks":  {[2][]byte{body, otherKeySig(t, body)}, [2][]byte{}, false, awsTerminates},
		"regional good even when us-east-1 tampered": {[2][]byte{body, sign(body)}, [2][]byte{other, sign(other)}, true, awsTerminates},
		"tampered, and the terminate fails":          {[2][]byte{other, sign(body)}, [2][]byte{}, false, awsFails},
		"tampered, and no aws CLI on the AMI":        {[2][]byte{other, sign(body)}, [2][]byte{}, false, awsAbsent},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := newStubServer(t, "eu-west-1")
			for region, obj := range map[string][2][]byte{"eu-west-1": tc.regional, "us-east-1": tc.fallback} {
				if obj[0] != nil {
					srv.put(region, key, obj[0])
				}
				if obj[1] != nil {
					srv.put(region, key+".sig", obj[1])
				}
			}
			sb := newBootSandbox(t, srv, tc.aws, nil)
			out, err := sb.run(script)

			ran := fileExists(filepath.Join(sb.dir, "BODY_RAN"))
			post := fileExists(filepath.Join(sb.dir, "tmp", "POST_RAN"))
			if tc.wantRun {
				if err != nil || !ran || !post {
					t.Fatalf("a verified body did not run: err=%v body=%v post=%v\n%s", err, ran, post, out)
				}
				if got := sb.read("BODY_RAN"); got != "body\n" {
					t.Errorf("the body that ran wrote %q, want the expected body's output", got)
				}
				if c := sb.read("aws-calls"); c != "" {
					t.Errorf("a verified boot called aws: %q", c)
				}
				return
			}
			if ran {
				t.Errorf("an unverified body ran\n%s", out)
			}
			if post {
				t.Errorf("the launch's own user data ran after the body failed verification\n%s", out)
			}
			if fileExists(filepath.Join(sb.dir, "etc", "spawn", "bootstrap-body.sh")) {
				t.Error("the unverified body was left on disk")
			}
			assertRetired(t, sb, out, err, reason, tc.aws)
			// Both sources were tried before giving up.
			if srv.hit("us-east-1", key) == 0 {
				t.Error("the us-east-1 fallback was never tried")
			}
		})
	}

	t.Run("a missing object is retried before falling back", func(t *testing.T) {
		srv := newStubServer(t, "eu-west-1")
		srv.put("us-east-1", key, body)
		srv.put("us-east-1", key+".sig", sign(body))
		sb := newBootSandbox(t, srv, awsTerminates, nil)
		if out, err := sb.run(script); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if n := srv.hit("eu-west-1", key); n < 2 {
			t.Errorf("the regional body was fetched %d times; a failed fetch must be retried", n)
		}
	})
}

// TestInlineBootstrapRetiresAnInstanceWithoutSpored is the same leak in the
// inline bootstrap (#1274): its aborts before spored starts (no spored could
// be installed, or the architecture has no spored) exited 1 and left the
// instance running with nothing enforcing its TTL. Runs the real inline
// bootstrap against an empty bucket.
func TestInlineBootstrapRetiresAnInstanceWithoutSpored(t *testing.T) {
	script, err := buildLinuxBootstrap(BootstrapConfig{
		Username: "tester", CustomUserData: "echo post > /tmp/POST_RAN\n",
	}, "", BootstrapBody())
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		uname  string
		reason string
		aws    awsMode
	}{
		"no spored to install":           {"x86_64", "spored install failed", awsTerminates},
		"no spored, terminate fails":     {"x86_64", "spored install failed", awsFails},
		"no spored for the architecture": {"sparc64", "unsupported architecture sparc64", awsTerminates},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := newStubServer(t, "eu-west-1")
			sb := newBootSandbox(t, srv, tc.aws, map[string]string{"uname": "#!/bin/bash\necho " + tc.uname + "\n"})
			out, err := sb.run(script)
			if fileExists(filepath.Join(sb.dir, "tmp", "POST_RAN")) || strings.Contains(out, "spored installation complete") {
				t.Errorf("the bootstrap carried on without spored\n%s", out)
			}
			assertRetired(t, sb, out, err, tc.reason, tc.aws)
		})
	}
}

// standInSha256sum puts a sha256sum into binDir: the real one where there is
// one (Linux), else shasum's (macOS), so the gate runs for whoever edits it.
func standInSha256sum(t *testing.T, binDir string) {
	t.Helper()
	if p, err := exec.LookPath("sha256sum"); err == nil {
		if err := os.Symlink(p, filepath.Join(binDir, "sha256sum")); err != nil {
			t.Fatal(err)
		}
		return
	}
	sh, err := exec.LookPath("shasum")
	if err != nil {
		t.Skip("neither sha256sum nor shasum available")
	}
	writeExec(t, filepath.Join(binDir, "sha256sum"), "#!/bin/bash\nexec "+sh+" -a 256 \"$@\"\n")
}

func otherKeySig(t *testing.T, b []byte) []byte {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	sig, err := ecdsa.SignASN1(rand.Reader, k, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return []byte(base64.StdEncoding.EncodeToString(sig) + "\n")
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func readOr(p string) []byte { b, _ := os.ReadFile(p); return b }
