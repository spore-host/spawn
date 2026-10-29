package sporedinstall

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// #1412: every install path installs the spored of the spawn that rendered it,
// from spawn/versions/<version>/, and never falls back to the unversioned
// latest. These EXECUTE BucketInstallFuncName, the one function every call site
// calls, against a fixture serving the objects at real bucket key layouts.

const pinTestVersion = "9.9.9"

// copyTo publishes the fixture's binary, .sha256 and .sig under prefix too (a
// path like "/regional/spawn/versions/9.9.9").
func (f *fixture) copyTo(prefix string) {
	for _, suf := range []string{"", ".sha256", ".sig"} {
		f.bodies[prefix+"/"+f.binary+suf] = f.bodies["/"+f.binary+suf]
	}
}

// runFromBuckets runs BucketInstallFuncName with the version pinned by
// PinScript(pin), the fixture's "/regional" and "/fallback" as the two
// buckets, and signature verification on. It returns the exit status, the
// output, whether spored was installed, and every path requested.
func (f *fixture) runFromBuckets(pin string) (int, string, bool, []string) {
	f.t.Helper()
	script := strings.ReplaceAll(VerifiedInstallFunc(), Dest, f.destPath)
	script = strings.ReplaceAll(script, SigningKeyPath, f.keyPath)
	// Retries on a 404 are the installer's business, not this test's; skip the waits.
	prelude := "sleep() { :; }\n"
	if _, err := exec.LookPath("sha256sum"); err != nil {
		prelude += "sha256sum() { shasum -a 256 \"$@\"; }\n"
	}
	full := fmt.Sprintf("%s%s\nSPORED_SIG_VERIFY=1\n%s\n%s %q %q %q\n", prelude, script, PinScript(pin),
		BucketInstallFuncName, f.binary, f.srv.URL+"/regional", f.srv.URL+"/fallback")
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd := exec.Command("bash", "-c", full) //nolint:gosec // this package's own generated script
	cmd.Env = append(os.Environ(), f.env...)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		code = 1
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
	}
	_, statErr := os.Stat(f.destPath)
	f.mu.Lock()
	defer f.mu.Unlock()
	return code, string(out), statErr == nil, append([]string(nil), f.requested...)
}

// latestPrefixes are every unversioned source a dev build reads.
var latestPrefixes = []string{"/regional/spawn", "/regional", "/fallback/spawn", "/fallback"}

func publishLatest(f *fixture) {
	for _, p := range latestPrefixes {
		f.copyTo(p)
	}
}

// unversioned returns the requests outside spawn/versions/<pinTestVersion>/.
func unversioned(paths []string) []string {
	var out []string
	for _, p := range paths {
		if !strings.Contains(p, "/spawn/versions/"+pinTestVersion+"/") {
			out = append(out, p)
		}
	}
	return out
}

func TestAPinnedInstallFetchesOnlyItsOwnVersion(t *testing.T) {
	requireBash(t)
	for name, where := range map[string]string{
		"regional": "/regional/spawn/versions/" + pinTestVersion,
		"fallback": "/fallback/spawn/versions/" + pinTestVersion,
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			publishLatest(f)
			f.copyTo(where)
			code, out, installed, paths := f.runFromBuckets(pinTestVersion)
			if code != 0 || !installed {
				t.Fatalf("the pinned spored at %s did not install (exit %d):\n%s", where, code, out)
			}
			if bad := unversioned(paths); len(bad) > 0 {
				t.Errorf("a pinned install read unversioned objects %v; it must read only spawn/versions/%s/ (#1412)", bad, pinTestVersion)
			}
			for _, want := range []string{where + "/" + f.binary, where + "/" + f.binary + ".sig", where + "/" + f.binary + ".sha256"} {
				if !contains(paths, want) {
					t.Errorf("the pinned install never fetched %s; requests: %v", want, paths)
				}
			}
		})
	}
}

// A missing or unverifiable versioned spored fails the install, exactly as a
// bad signature does, even with a good latest spored in both buckets: a
// versioned build never silently runs another release's spored (#1412).
func TestAPinnedInstallNeverFallsBackToLatest(t *testing.T) {
	requireBash(t)
	cases := map[string]func(f *fixture){
		"no versioned object at all": func(f *fixture) {},
		"versioned binary with no signature": func(f *fixture) {
			f.copyTo("/regional/spawn/versions/" + pinTestVersion)
			delete(f.bodies, "/regional/spawn/versions/"+pinTestVersion+"/"+f.binary+".sig")
		},
		"versioned binary with no checksum": func(f *fixture) {
			f.copyTo("/regional/spawn/versions/" + pinTestVersion)
			delete(f.bodies, "/regional/spawn/versions/"+pinTestVersion+"/"+f.binary+".sha256")
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			publishLatest(f)
			mutate(f)
			code, out, installed, paths := f.runFromBuckets(pinTestVersion)
			if code == 0 || installed {
				t.Fatalf("installed (exit %d, installed=%v) with no verifiable spored %s published; "+
					"a pinned launch must fail closed (#1412):\n%s", code, installed, pinTestVersion, out)
			}
			if bad := unversioned(paths); len(bad) > 0 {
				t.Errorf("fell back to unversioned objects %v (#1412)", bad)
			}
		})
	}
}

// A dev build of spawn has no published spored: it installs the latest, from
// the same sources in the same order as before #1412.
func TestAnUnpinnedInstallFetchesLatest(t *testing.T) {
	requireBash(t)
	f := newFixture(t)
	f.copyTo("/fallback")
	code, out, installed, paths := f.runFromBuckets("")
	if code != 0 || !installed {
		t.Fatalf("a dev build did not install the latest spored (exit %d):\n%s", code, out)
	}
	// The sources whose .sig was probed, in order, each once however often the
	// probe was retried.
	var probes []string
	for _, p := range paths {
		if src, ok := strings.CutSuffix(p, "/"+f.binary+".sig"); ok && (len(probes) == 0 || probes[len(probes)-1] != src) {
			probes = append(probes, src)
		}
	}
	if got, want := strings.Join(probes, " "), strings.Join(latestPrefixes, " "); got != want {
		t.Errorf("sources probed %q, want %q", got, want)
	}
}

func TestPinScriptQuotesAndPins(t *testing.T) {
	requireBash(t)
	for _, v := range []string{"", "0.148.0", "it's $(not) run"} {
		// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
		out, err := exec.Command("bash", "-c", PinScript(v)+`printf %s "$`+PinVar+`"`).CombinedOutput() //nolint:gosec // this package's own generated script
		if err != nil || string(out) != v {
			t.Errorf("PinScript(%q) sets %s to %q (err %v)", v, PinVar, out, err)
		}
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
