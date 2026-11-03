package userdata

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/egressproxy"
)

// The #1515 phase 4 gates. With MPIConfig.EgressProxyAddr set, the EFA
// installer's download and run and the apt-get calls this script makes go
// through the egress proxy, each by itself; the regional S3 mirror, AL2023's
// S3 package repositories and the --mpi-command do not. Without it the
// script is what it always was.
//
// The sandbox runs the whole rendered member script; the stand-in curl,
// apt-get, yum and efa_installer.sh record the proxy variables they ran with
// and their arguments (mpi_efa_openmpi_test.go, mpi_sandbox_test.go).

const (
	testProxyAddr = "10.0.1.5:3128"
	testProxyURL  = "http://10.0.1.5:3128"
	testAptProxy  = "-o Acquire::http::Proxy=http://10.0.1.5:3128/ -o Acquire::https::Proxy=http://10.0.1.5:3128/"
)

func testNoProxy(region string) string { return strings.Join(aws.EgressProxyNoProxy(region), ",") }

// proxyEnv is one line of a stand-in's env log: the http_proxy, https_proxy
// and no_proxy it ran with.
type proxyEnv struct{ http, https, no string }

func readProxyEnvs(t *testing.T, r sandboxRun, rel string) ([]string, []proxyEnv) {
	t.Helper()
	log, _ := r.read(t, rel)
	var keys []string
	var envs []proxyEnv
	for _, l := range strings.Split(strings.TrimSpace(log), "\n") {
		if l == "" {
			continue
		}
		f := strings.Split(l, "|")
		if len(f) == 4 { // curl: the URL first
			keys, f = append(keys, f[0]), f[1:]
		}
		if len(f) != 3 {
			t.Fatalf("%s: malformed line %q", rel, l)
		}
		envs = append(envs, proxyEnv{f[0], f[1], f[2]})
	}
	return keys, envs
}

// viaProxy reports whether curl, run with e, fetches rawURL through the
// proxy, by curl's own rule: the scheme's proxy variable is set and no_proxy
// does not name the host.
func viaProxy(t *testing.T, e proxyEnv, rawURL string) bool {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	p := e.http
	if u.Scheme == "https" {
		p = e.https
	}
	return p != "" && !egressproxy.NoProxyBypasses(e.no, u.Hostname())
}

// mpiCommandEnvs are the environments the --mpi-command saw, one per rank.
func mpiCommandEnvs(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && strings.HasPrefix(info.Name(), "mpi-command.env.") {
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			out[strings.TrimPrefix(p, root)] = string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

var proxyVar = regexp.MustCompile(`(?im)^(https?|no|all|ftp)_proxy=`)

func TestEFAThroughTheEgressProxy(t *testing.T) {
	for name, o := range map[string]sandboxOpts{
		"AL2023, a yum base":      {},
		"Ubuntu, an apt-get base": {noMPI: true, apt: true},
		"ubuntu:24.04 container":  {noMPI: true, container: true, ubuntu: true},
	} {
		t.Run(name, func(t *testing.T) {
			good := efaTarball(t, efaInstallerScript(true))
			pinEFATarball(t, good)
			o.cfg = MPIConfig{EgressProxyAddr: testProxyAddr, MPICommand: `env > /tmp/mpi-command.env.$OMPI_COMM_WORLD_RANK`}
			// The mirror lacks it, so both sources are fetched.
			r := efaCohortServing(t, true, o, map[string][]byte{efaOriginURL: good})
			if r.rc != 0 {
				t.Fatalf("rank 0 exited %d with --efa through the proxy", r.rc)
			}
			if got := whichMpirun(t, r); got != efaMarker {
				t.Errorf("rank 0 ran mpirun %q, want the EFA installer's", got)
			}
			noProxy := testNoProxy("us-east-1")

			// curl: the mirror direct, the origin through the proxy.
			urls, envs := readProxyEnvs(t, r, "curl-env.log")
			if strings.Join(urls, " ") != efaMirrorURL+" "+efaOriginURL {
				t.Fatalf("curl fetched %q, want the mirror, then the origin", urls)
			}
			if viaProxy(t, envs[0], efaMirrorURL) {
				t.Errorf("the regional mirror %s went through the proxy (env %+v); it must go direct, through the S3 endpoint", efaMirrorURL, envs[0])
			}
			if !viaProxy(t, envs[1], efaOriginURL) || envs[1].https != testProxyURL {
				t.Errorf("the EFA origin %s did not go through the proxy %s (env %+v)", efaOriginURL, testProxyURL, envs[1])
			}

			// The installer: proxy variables and the derived no_proxy, for its
			// own dnf or apt-get.
			_, inst := readProxyEnvs(t, r, "efa-env.log")
			if len(inst) != 1 || inst[0] != (proxyEnv{testProxyURL, testProxyURL, noProxy}) {
				t.Fatalf("efa_installer.sh ran with %+v, want http(s)_proxy %s and no_proxy %s", inst, testProxyURL, noProxy)
			}
			for _, h := range []string{"al2023-repos-us-east-1-de612dc2.s3.dualstack.us-east-1.amazonaws.com", "al2023-repos-us-east-1-de612dc2.s3.us-east-1.amazonaws.com"} {
				if viaProxy(t, inst[0], "https://"+h+"/core/mirrors/2023.9/x86_64/mirror.list") {
					t.Errorf("the installer's dnf would send AL2023's repository %s to the proxy, which refuses it", h)
				}
			}
			for _, u := range []string{"http://us-east-1.ec2.archive.ubuntu.com/ubuntu/", "http://security.ubuntu.com/ubuntu/", "http://us-east-1.ec2.ports.ubuntu.com/ubuntu-ports/"} {
				if !viaProxy(t, inst[0], u) {
					t.Errorf("the installer's apt-get would fetch %s without the proxy", u)
				}
			}

			// spawn's own apt-get: the Acquire options, on every call.
			if log, ok := r.read(t, "apt.log"); o.apt || o.ubuntu {
				lines := strings.Split(strings.TrimSpace(log), "\n")
				if !ok || len(lines) == 0 {
					t.Fatal("no apt-get ran on the apt base")
				}
				for _, l := range lines {
					if !strings.HasPrefix(l, "apt-get "+testAptProxy+" ") {
						t.Errorf("apt-get %q does not carry %q", l, testAptProxy)
					}
				}
			}
			// spawn's own yum/dnf is never proxied.
			if _, envs := readProxyEnvs(t, r, "yum-env.log"); len(envs) != 0 {
				for _, e := range envs {
					if e != (proxyEnv{}) {
						t.Errorf("yum ran with proxy variables %+v", e)
					}
				}
			}

			// The --mpi-command, on every rank, saw no proxy variable.
			got := mpiCommandEnvs(t, r.root)
			if len(got) != 3 {
				t.Fatalf("--mpi-command environments %v, want one per rank (3)", got)
			}
			for f, env := range got {
				if m := proxyVar.FindString(env); m != "" {
					t.Errorf("the --mpi-command saw %s... (%s)", m, f)
				}
			}
			// Nothing persistent names the proxy.
			for _, f := range []string{"etc/environment", "etc/profile.d/mpi.sh", "etc/profile.d/efa.sh"} {
				if b, _ := r.read(t, f); strings.Contains(b, "10.0.1.5") || proxyVar.MatchString(b) {
					t.Errorf("%s names the proxy:\n%s", f, b)
				}
			}
		})
	}
}

// The MPI apt-get install (no --efa) goes through the proxy too, by options.
func TestMPIAptThroughTheEgressProxy(t *testing.T) {
	r := efaCohort(t, false, sandboxOpts{noMPI: true, apt: true, cfg: MPIConfig{EgressProxyAddr: testProxyAddr}})
	if r.rc != 0 {
		t.Fatalf("rank 0 exited %d", r.rc)
	}
	// One install for the sandbox's shared PATH: the peers then find mpirun.
	if u, i := aptCalls(t, r, "update"), aptCalls(t, r, "install"); u != 1 || i != 1 {
		t.Fatalf("apt-get update %d, install %d times, want one each", u, i)
	}
	log, _ := r.read(t, "apt.log")
	for _, l := range strings.Split(strings.TrimSpace(log), "\n") {
		if !strings.HasPrefix(l, "apt-get "+testAptProxy+" ") {
			t.Errorf("apt-get %q does not carry %q", l, testAptProxy)
		}
	}
}

// What a proxied render adds, and nothing else: removing these from it gives
// the unproxied render, byte for byte. With them it is #1515's script; without
// EgressProxyAddr it is the script as it was before (TestNoEgressProxyRendersUnchanged).
func stripProxy(script string) string {
	script = strings.ReplaceAll(script, "spawn_px ", "")
	script = strings.ReplaceAll(script, " "+testAptProxy, "")
	script = regexp.MustCompile(`\n# Through the egress proxy, for these commands only \(#1515\): see mpi\.go\.\nspawn_px\(\) \{ [^\n]* "\$@"; \}`).ReplaceAllString(script, "")
	return script
}

func TestNoEgressProxyRendersUnchanged(t *testing.T) {
	for name, cfg := range map[string]MPIConfig{
		"efa rank 0":     {EFAEnabled: true, MPICommand: "hostname"},
		"efa peer":       {EFAEnabled: true, JobArrayIndex: 1},
		"no efa rank 0":  {MPICommand: "hostname"},
		"skip install":   {SkipInstall: true},
		"no mirror":      {EFAEnabled: true, Region: "af-south-1"},
		"completion etc": {EFAEnabled: true, CompletionFile: "/data/done", ReadyGate: "/run/spawn/mpi-ready"},
	} {
		t.Run(name, func(t *testing.T) {
			if cfg.Region == "" {
				cfg.Region = "us-east-1"
			}
			cfg.JobArrayID, cfg.JobArraySize = "arr-1", 2
			plain, err := GenerateMPIUserData(cfg)
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range []string{"spawn_px", "_proxy", "Acquire::http", "10.0.1.5", "#1515"} {
				if strings.Contains(plain, w) {
					t.Errorf("without a proxy the script contains %q", w)
				}
			}
			cfg.EgressProxyAddr = testProxyAddr
			proxied, err := GenerateMPIUserData(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if got := stripProxy(proxied); got != plain {
				t.Errorf("the proxied script differs from the plain one by more than the proxy:\n%s", lineDiff(plain, got))
			}
			if cfg.EFAEnabled && !strings.Contains(proxied, "spawn_px() { http_proxy=") {
				t.Error("an --efa script with a proxy defines no spawn_px")
			}
		})
	}
}

// lineDiff is the first differing line of a and b, for a failure message.
func lineDiff(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(al) || i < len(bl); i++ {
		var x, y string
		if i < len(al) {
			x = al[i]
		}
		if i < len(bl) {
			y = bl[i]
		}
		if x != y {
			return "line " + strconv.Itoa(i+1) + ":\n- " + x + "\n+ " + y
		}
	}
	return ""
}

// Only an IP:port reaches the script, which carries it unquoted.
func TestEgressProxyAddrIsValidated(t *testing.T) {
	for _, bad := range []string{"proxy.internal:3128", "10.0.1.5", "10.0.1.5:3128; rm -rf /", "$(id):3128", "10.0.1.5:0"} {
		if _, err := GenerateMPIUserData(MPIConfig{Region: "us-east-1", JobArrayID: "a", JobArraySize: 2, EFAEnabled: true, EgressProxyAddr: bad}); err == nil {
			t.Errorf("EgressProxyAddr %q was rendered", bad)
		}
	}
	if _, err := GenerateMPIUserData(MPIConfig{Region: "us-east-1; id", JobArrayID: "a", JobArraySize: 2, EFAEnabled: true, EgressProxyAddr: testProxyAddr}); err == nil {
		t.Error("a region that is not a region name was rendered into no_proxy")
	}
}
