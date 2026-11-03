package launcher

import (
	"crypto/sha256"
	"encoding/hex"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/egressproxy"
)

// The #1515 phase 4 gates for the bootstrap. Its one fetch from outside AWS
// is the AWS CLI zip on an image with none (stock Ubuntu). With EgressProxy
// set, the per-launch part installs it before the body, with the proxy
// variables on that one call: the regional mirror direct (no_proxy names it),
// awscli.amazonaws.com through the proxy, and nothing after it proxied.
// Without EgressProxy the user data is byte-identical to what it was.

var testEgressProxy = &EgressProxy{Addr: "10.0.1.5:3128", Region: "eu-west-1"}

// proxyCurl is a curl that records, for each URL, the proxy variables it was
// run with, then runs the real curl (at $1 of the wrapper's name) without
// them: the sandbox's stand-in bucket is on loopback.
func proxyCurl(t *testing.T) string {
	t.Helper()
	real, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl not available")
	}
	return `#!/bin/bash
for a; do case "$a" in http://* | https://*) u=$a ;; esac; done
printf '%s|%s|%s|%s\n' "$u" "${http_proxy-}" "${https_proxy-}" "${no_proxy-}" >> "$HOME/curl-env.log"
unset http_proxy https_proxy no_proxy
exec ` + real + ` "$@"
`
}

// curlFetch is one URL the boot fetched and the proxy variables it had.
type curlFetch struct{ url, http, https, no string }

func curlFetches(t *testing.T, sb *bootSandbox) []curlFetch {
	t.Helper()
	var out []curlFetch
	for _, l := range strings.Split(strings.TrimSpace(sb.read("curl-env.log")), "\n") {
		f := strings.Split(l, "|")
		if len(f) != 4 {
			continue
		}
		out = append(out, curlFetch{f[0], f[1], f[2], f[3]})
	}
	return out
}

// realHost is the host a sandbox URL stands in for: the sandbox serves the
// AWS CLI origin at /b/awscli/ and each region's spawn-binaries bucket at
// /b/<region>/ (bootSandbox.run).
func realHost(u string) string {
	if m := regexp.MustCompile(`/b/([a-z0-9-]+)/`).FindStringSubmatch(u); m != nil {
		if m[1] == "awscli" {
			return "awscli.amazonaws.com"
		}
		return "spawn-binaries-" + m[1] + ".s3." + m[1] + ".amazonaws.com"
	}
	return "169.254.169.254" // IMDS, the sandbox's /imds
}

func proxied(f curlFetch) bool {
	p := f.http
	if strings.HasPrefix(realHost(f.url), "awscli") || strings.Contains(realHost(f.url), ".s3.") {
		p = f.https // both are https:// on an instance
	}
	return p != "" && !egressproxy.NoProxyBypasses(f.no, realHost(f.url))
}

func TestAWSCLIOriginThroughTheEgressProxy(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	zipBytes := testAWSCLIZip(t)
	sum := sha256.Sum256(zipBytes)
	pin := func(s string) string { return strings.ReplaceAll(s, awsCLISHA256x86_64, hex.EncodeToString(sum[:])) }
	wantNo := strings.Join(aws.EgressProxyNoProxy("eu-west-1"), ",")
	for name, tc := range map[string]struct{ mirror bool }{
		"the mirror lacks it: the origin, through the proxy": {},
		"the mirror has it: the proxy is not used":           {mirror: true},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := BootstrapConfig{Username: "ubuntu", Command: "env > /tmp/COMMAND_ENV", EgressProxy: testEgressProxy,
				CustomUserData: "env > /tmp/USER_DATA_ENV\n"}
			setup := func(s *stubServer) {
				s.put("awscli", awsCLIZipKey, zipBytes)
				if tc.mirror {
					s.put("eu-west-1", x86Pin(t).MirrorKey, zipBytes)
				}
			}
			sb, out, err := fullBoot(t, cfg, "", bootOpts{standIns: map[string]string{"aws": "", "curl": proxyCurl(t)}, edit: pin, setup: setup})
			if err != nil {
				t.Fatalf("the bootstrap failed: %v\n%s", err, out)
			}
			if got := sb.read("awscli-installs"); got != "installed\n" {
				t.Fatalf("the AWS CLI installer ran %q, want once", got)
			}
			var cli []curlFetch
			for _, f := range curlFetches(t, sb) {
				if strings.Contains(f.url, "awscli-exe-linux-") {
					cli = append(cli, f)
					continue
				}
				if f.http != "" || f.https != "" || f.no != "" {
					t.Errorf("curl %s ran with proxy variables (%+v): only the AWS CLI install is proxied", f.url, f)
				}
			}
			want := 2
			if tc.mirror {
				want = 1
			}
			if len(cli) != want {
				t.Fatalf("AWS CLI fetches %+v, want %d", cli, want)
			}
			for _, f := range cli {
				if f.https != "http://10.0.1.5:3128" || f.no != wantNo {
					t.Errorf("the AWS CLI fetch %s ran with %+v, want https_proxy http://10.0.1.5:3128 and no_proxy %s", f.url, f, wantNo)
				}
			}
			if proxied(cli[0]) || realHost(cli[0].url) != "spawn-binaries-eu-west-1.s3.eu-west-1.amazonaws.com" {
				t.Errorf("the first fetch, %s (%s), went through the proxy or is not the regional mirror", cli[0].url, realHost(cli[0].url))
			}
			if !tc.mirror && (!proxied(cli[1]) || realHost(cli[1].url) != "awscli.amazonaws.com") {
				t.Errorf("the origin fetch %s (%s) did not go through the proxy", cli[1].url, realHost(cli[1].url))
			}
			for _, f := range []string{"tmp/COMMAND_ENV", "tmp/USER_DATA_ENV"} {
				env := sb.read(f)
				if env == "" {
					t.Fatalf("%s was not written\n%s", f, out)
				}
				if regexp.MustCompile(`(?im)^(https?|no)_proxy=`).MatchString(env) {
					t.Errorf("%s saw a proxy variable:\n%s", f, env)
				}
			}
		})
	}
}

// The body is published and signed: the proxy never reaches it.
func TestEgressProxyLeavesTheBodyAndAPlainLaunchUnchanged(t *testing.T) {
	body := BootstrapBody()
	for _, w := range []string{"SPAWN_PX_REGION", "_proxy="} {
		if strings.Contains(body, w) {
			t.Errorf("the published body contains %q", w)
		}
	}
	for name, cfg := range map[string]BootstrapConfig{
		"plain":   {Username: "ubuntu", Command: "echo hi"},
		"staged":  staged(BootstrapConfig{Username: "ubuntu", Command: "echo hi"}),
		"plugins": {Username: "ubuntu", Plugins: stubConfigs()["plugins"].Plugins, CustomUserData: "echo x\n"},
	} {
		for _, version := range []string{"", testRelease} {
			plain, err := buildLinuxBootstrap(cfg, version, version, body)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(plain, "_proxy=") || strings.Contains(plain, "SPAWN_PX_REGION") {
				t.Errorf("%s/%q: without a proxy the user data names one", name, version)
			}
			cfg.EgressProxy = testEgressProxy
			proxiedScript, err := buildLinuxBootstrap(cfg, version, version, body)
			cfg.EgressProxy = nil
			if err != nil {
				t.Fatal(err)
			}
			block, err := egressProxyAWSCLI(*testEgressProxy)
			if err != nil {
				t.Fatal(err)
			}
			// What the proxy adds: its install call, and, on the stub path
			// without staged files, the install function it calls.
			stripped := strings.Replace(proxiedScript, block, "", 1)
			if version != "" && cfg.StagedFiles == nil {
				stripped = strings.Replace(stripped, awsCLIInstallFunc(), "", 1)
			}
			if stripped != plain {
				t.Errorf("%s/%q: the proxied user data differs from the plain one by more than the proxy's install", name, version)
			}
			// Before anything that needs the CLI: the staged files' fetch, and
			// the body. After the function it calls is defined.
			at := strings.Index(proxiedScript, block)
			if def := strings.Index(proxiedScript, "spawn_need_aws() {"); def < 0 || def > at {
				t.Errorf("%s/%q: spawn_need_aws is defined at %d, after the egress proxy's call at %d", name, version, def, at)
			}
			for _, later := range []string{`spawn_need_aws "$SPAWN_ST_REGION"`, `spawn_need_aws "$REGION"`, bodyPath} {
				if i := strings.Index(proxiedScript, later); i >= 0 && i < at {
					t.Errorf("%s/%q: %q comes before the egress proxy's AWS CLI install", name, version, later)
				}
			}
		}
	}
	if _, err := buildLinuxBootstrap(BootstrapConfig{Username: "u", EgressProxy: &EgressProxy{Addr: "proxy:3128", Region: "eu-west-1"}}, "", "", body); err == nil {
		t.Error("an egress proxy address that is not IP:port was rendered")
	}
}
