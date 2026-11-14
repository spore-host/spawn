package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// These RUN scripts/installer-pins-dcv.sh under bash against a local origin
// (file://), with gpg, rpmkeys, rpmdb and `go` (installerpin show) stood in
// for: the weekly workflow's DCV job, without AWS. Amazon's DCV key is not
// ours, so what is checked is the script's own logic: every RPM must verify,
// a server RPM must be there, the outputs carry the tarballs' real sha256s and
// the sorted fingerprints, and an already-pinned version is left alone.

var originLine = regexp.MustCompile(`(?m)^readonly ORIGIN=\S+$`)

const (
	testDCV    = "2025.0-20103"
	fprLow     = "0123456789ABCDEF0123456789ABCDEF01234567"
	fprHigh    = "FEDCBA9876543210FEDCBA9876543210FEDCBA98"
	goodRPMOut = "digests signatures OK"
)

type dcvRun struct {
	out    string
	err    error
	output map[string]string
}

func runDCVScript(t *testing.T, pinned string, rpms map[string]string, args ...string) dcvRun {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the workflow runs installer-pins-dcv.sh on ubuntu-latest")
	}
	for _, tool := range []string{"bash", "curl", "tar"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	root := t.TempDir()
	src, err := os.ReadFile("../installer-pins-dcv.sh")
	if err != nil {
		t.Fatal(err)
	}
	origin := filepath.Join(root, "origin")
	if !originLine.Match(src) {
		t.Fatal("no `readonly ORIGIN=` line to point at the local origin")
	}
	script := originLine.ReplaceAll(src, []byte("readonly ORIGIN=file://"+origin))
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "scripts", "installer-pins-dcv.sh"), script, 0o755); err != nil { //nolint:gosec // the script under test
		t.Fatal(err)
	}
	// The origin: the key and one tarball per arch holding rpms.
	servers := filepath.Join(origin, "2025.0", "Servers")
	if err := os.MkdirAll(servers, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(origin, "NICE-GPG-KEY"), []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	sums := map[string]string{}
	for _, arch := range []string{"x86_64", "aarch64"} {
		stage := filepath.Join(root, "stage-"+arch)
		top := filepath.Join(stage, "nice-dcv-"+testDCV)
		if err := os.MkdirAll(top, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, body := range rpms {
			if err := os.WriteFile(filepath.Join(top, name), []byte(arch+body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		tgz := filepath.Join(servers, "nice-dcv-"+testDCV+"-amzn2023-"+arch+".tgz")
		// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
		if out, err := exec.Command("tar", "-czf", tgz, "-C", stage, "nice-dcv-"+testDCV).CombinedOutput(); err != nil { //nolint:gosec // test fixture
			t.Fatalf("tar: %v %s", err, out)
		}
		b, _ := os.ReadFile(tgz)
		sums[arch] = fmt.Sprintf("%x", sha256.Sum256(b))
	}
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	stubs := map[string]string{
		"go": "#!/bin/bash\nprintf 'dcvVersion=" + testDCV + "\\ndcvVersionMajorMinor=2025.0\\ndcvPinVersion=" + pinned + "\\n'\n",
		// The key file holds two primary keys, high one first; a subkey's fpr
		// must not count.
		"gpg": "#!/bin/bash\nprintf 'pub:-:255:22:A:1:::-:::scESC:\\nfpr:::::::::" + fprHigh + ":\\nsub:-:255:18:B:1::::::e:\\nfpr:::::::::" +
			strings.Repeat("9", 40) + ":\\npub:-:255:22:C:1:::-:::scESC:\\nfpr:::::::::" + fprLow + ":\\n'\n",
		"rpmdb": "#!/bin/bash\nexit 0\n",
		// rpmkeys -K: OK unless the package's name says it is bad.
		"rpmkeys": "#!/bin/bash\nfor a; do f=$a; done\nif [ \"$3\" = --import ]; then exit 0; fi\ncase $f in *bad*) echo \"$f: digests SIGNATURES NOT OK\"; exit 1 ;; *) echo \"$f: " + goodRPMOut + "\" ;; esac\n",
	}
	for n, body := range stubs {
		if err := os.WriteFile(filepath.Join(bin, n), []byte(body), 0o755); err != nil { //nolint:gosec // test stand-ins must be executable
			t.Fatal(err)
		}
	}
	ghOut := filepath.Join(root, "github-output")
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd := exec.Command("bash", append([]string{filepath.Join(root, "scripts", "installer-pins-dcv.sh")}, args...)...) //nolint:gosec // the script under test
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin:/usr/local/bin:/opt/homebrew/bin", "HOME=" + root, "GITHUB_OUTPUT=" + ghOut, "TMPDIR=" + root}
	out, err := cmd.CombinedOutput()
	r := dcvRun{out: string(out), err: err, output: map[string]string{}}
	b, _ := os.ReadFile(ghOut)
	for _, l := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			r.output[k] = v // the last write wins, as on GitHub
		}
	}
	r.output["_want_x86_64"], r.output["_want_aarch64"] = sums["x86_64"], sums["aarch64"]
	return r
}

func TestDCVPinScriptPinsVerifiedTarballs(t *testing.T) {
	r := runDCVScript(t, "", map[string]string{"nice-dcv-server-1.rpm": "s", "nice-xdcv-1.rpm": "x"})
	if r.err != nil {
		t.Fatalf("%v\n%s", r.err, r.out)
	}
	for k, want := range map[string]string{
		"changed": "true", "version": testDCV,
		"sha256_x86_64": r.output["_want_x86_64"], "sha256_aarch64": r.output["_want_aarch64"],
		"key_fingerprints": fprLow + " " + fprHigh,
	} {
		if r.output[k] != want {
			t.Errorf("output %s = %q, want %q\n%s", k, r.output[k], want, r.out)
		}
	}
}

func TestDCVPinScriptRefuses(t *testing.T) {
	for name, rpms := range map[string]map[string]string{
		"an RPM that does not verify": {"nice-dcv-server-1.rpm": "s", "nice-xdcv-bad-1.rpm": "x"},
		"no server RPM":               {"nice-xdcv-1.rpm": "x"},
		"no RPMs":                     {"README": "x"},
	} {
		t.Run(name, func(t *testing.T) {
			r := runDCVScript(t, "", rpms)
			if r.err == nil || r.output["changed"] == "true" {
				t.Errorf("the script pinned it (err %v, changed %q)\n%s", r.err, r.output["changed"], r.out)
			}
		})
	}
}

func TestDCVPinScriptLeavesAPinnedVersionAlone(t *testing.T) {
	r := runDCVScript(t, testDCV, map[string]string{"nice-dcv-server-1.rpm": "s"})
	if r.err != nil || r.output["changed"] != "false" || !strings.Contains(r.out, "pinned already") {
		t.Errorf("err %v, changed %q\n%s", r.err, r.output["changed"], r.out)
	}
	r = runDCVScript(t, testDCV, map[string]string{"nice-dcv-server-1.rpm": "s"}, "--force")
	if r.err != nil || r.output["changed"] != "true" {
		t.Errorf("--force did not re-verify: err %v, changed %q\n%s", r.err, r.output["changed"], r.out)
	}
}
