package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// These RUN scripts/efa-pin.sh, under bash with real gpg, against a local
// origin (file://) and a stand-in `go` for `efapin show`/`set`. AWS's private
// EFA key is not ours, so the script is run with a throwaway key's fingerprint
// in place of the pinned one (TestPinScriptPinsTheSameFingerprint holds the
// pinned one). verify is what the release runs before building anything; bump
// is what the weekly workflow runs.

var (
	fprLine    = regexp.MustCompile(`(?m)^readonly EFA_KEY_FPR=[0-9A-F]{40}$`)
	originLine = regexp.MustCompile(`(?m)^readonly ORIGIN=\S+$`)
)

type gpgHome struct {
	t   *testing.T
	dir string
}

// newGPGHome is a throwaway GNUPGHOME under /tmp: gpg-agent's socket lives in
// it, and macOS's t.TempDir() paths are longer than a socket path may be.
func newGPGHome(t *testing.T) *gpgHome {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the release and the weekly workflow run efa-pin.sh on ubuntu-latest")
	}
	for _, tool := range []string{"bash", "gpg", "gpgconf", "curl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	dir, err := os.MkdirTemp("/tmp", "efapin-gpg")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	g := &gpgHome{t: t, dir: dir}
	t.Cleanup(func() {
		_ = g.cmd("gpgconf", "--kill", "all").Run() // no agent may outlive the test
		_ = os.RemoveAll(dir)
	})
	return g
}

func (g *gpgHome) cmd(name string, args ...string) *exec.Cmd {
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	c := exec.Command(name, args...) //nolint:gosec // gpg with test arguments
	c.Env = append(os.Environ(), "GNUPGHOME="+g.dir)
	return c
}

func (g *gpgHome) gpg(args ...string) []byte {
	g.t.Helper()
	out, err := g.cmd("gpg", append([]string{"--batch", "--pinentry-mode", "loopback", "--passphrase", ""}, args...)...).Output()
	if err != nil {
		g.t.Fatalf("gpg %v: %v", args, err)
	}
	return out
}

func (g *gpgHome) newKey(uid string) string {
	g.t.Helper()
	g.gpg("--quick-gen-key", uid, "ed25519", "sign", "never")
	for _, l := range strings.Split(string(g.gpg("--with-colons", "--fingerprint", uid)), "\n") {
		if f := strings.Split(l, ":"); f[0] == "fpr" && len(f) > 9 {
			return f[9]
		}
	}
	g.t.Fatalf("no fingerprint for %s", uid)
	return ""
}

// sign writes file.sig, a detached binary signature by fpr.
func (g *gpgHome) sign(fpr, file string) {
	g.t.Helper()
	g.gpg("--yes", "--local-user", fpr, "--output", file+".sig", "--detach-sign", file)
}

type pinSandbox struct {
	t          *testing.T
	g          *gpgHome
	dir        string
	pinned     string // the fingerprint the sandbox's script pins
	other      string
	pinVersion string
	pinSum     string
}

func newPinSandbox(t *testing.T, script []byte) *pinSandbox {
	t.Helper()
	g := newGPGHome(t)
	sb := &pinSandbox{t: t, g: g, dir: t.TempDir(), pinVersion: "1.51.0"}
	sb.pinned = g.newKey("efapin test <pinned@example.invalid>")
	sb.other = g.newKey("efapin test <other@example.invalid>")
	if n := len(fprLine.FindAllIndex(script, -1)); n != 1 {
		t.Fatalf("efa-pin.sh has %d `readonly EFA_KEY_FPR=` lines, want 1", n)
	}
	if n := len(originLine.FindAllIndex(script, -1)); n != 1 {
		t.Fatalf("efa-pin.sh has %d `readonly ORIGIN=` lines, want 1", n)
	}
	s := fprLine.ReplaceAll(script, []byte("readonly EFA_KEY_FPR="+sb.pinned))
	s = originLine.ReplaceAll(s, []byte("readonly ORIGIN=file://"+filepath.Join(sb.dir, "origin")))
	for _, d := range []string{"scripts/efapin", "origin", "bin"} {
		if err := os.MkdirAll(filepath.Join(sb.dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sb.write("scripts/efa-pin.sh", s, 0o755)
	sb.write("scripts/efapin/aws-efa-installer.key", g.gpg("--armor", "--export", sb.pinned), 0o644)
	return sb
}

func (sb *pinSandbox) write(rel string, b []byte, mode os.FileMode) {
	sb.t.Helper()
	if err := os.WriteFile(filepath.Join(sb.dir, filepath.FromSlash(rel)), b, mode); err != nil {
		sb.t.Fatal(err)
	}
}

// publish puts aws-efa-installer-<v>.tar.gz at the origin, signed by signer,
// and returns its sha256.
func (sb *pinSandbox) publish(v string, body []byte, signer string) string {
	sb.t.Helper()
	p := filepath.Join(sb.dir, "origin", "aws-efa-installer-"+v+".tar.gz")
	if err := os.WriteFile(p, body, 0o644); err != nil {
		sb.t.Fatal(err)
	}
	sb.g.sign(signer, p)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// run runs the sandbox's efa-pin.sh with args. The stand-in go answers `efapin
// show` with the sandbox's pin and records `efapin set`'s arguments in set.log.
func (sb *pinSandbox) run(args ...string) (int, string) {
	sb.t.Helper()
	sb.write("bin/go", []byte(`#!/bin/bash
case " $* " in
  *" ./efapin show "*) printf 'efaInstallerVersion=%s\nefaInstallerSHA256=%s\n' "$PIN_VERSION" "$PIN_SUM" ;;
  *" ./efapin set "*) echo "$*" >> "$SB/set.log" ;;
  *) echo "unexpected go $*" >&2; exit 1 ;;
esac
`), 0o755)
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	c := exec.Command("bash", append([]string{filepath.Join(sb.dir, "scripts", "efa-pin.sh")}, args...)...) //nolint:gosec // the sandbox's script
	c.Env = append(os.Environ(), "PATH="+filepath.Join(sb.dir, "bin")+":"+os.Getenv("PATH"),
		"TMPDIR=/tmp", "SB="+sb.dir, "PIN_VERSION="+sb.pinVersion, "PIN_SUM="+sb.pinSum, "GITHUB_OUTPUT=")
	out, err := c.CombinedOutput()
	rc := 0
	if ee, ok := err.(*exec.ExitError); ok {
		rc = ee.ExitCode()
	} else if err != nil {
		sb.t.Fatalf("efa-pin.sh: %v\n%s", err, out)
	}
	return rc, string(out)
}

func pinScript(t *testing.T) []byte { return readFile(t, repoFile(t, "scripts/efa-pin.sh")) }

// The release's check: only a tarball signed by the pinned key AND with the
// pinned sha256 passes.
func TestVerifyRefusesAnythingButThePinnedSignedTarball(t *testing.T) {
	tgz := []byte("stand-in EFA installer 1.51.0\n")
	for name, c := range map[string]struct {
		setup func(sb *pinSandbox)
		ok    bool
	}{
		"signed by the pinned key, the pinned sha256": {func(sb *pinSandbox) { sb.pinSum = sb.publish("1.51.0", tgz, sb.pinned) }, true},
		"signed by another key":                       {func(sb *pinSandbox) { sb.pinSum = sb.publish("1.51.0", tgz, sb.other) }, false},
		"altered after signing": {func(sb *pinSandbox) {
			sb.pinSum = sb.publish("1.51.0", tgz, sb.pinned)
			sb.write("origin/aws-efa-installer-1.51.0.tar.gz", []byte("stand-in EFA installer 1.51.0, altered\n"), 0o644)
		}, false},
		"validly signed, not the pinned sha256": {func(sb *pinSandbox) {
			sb.publish("1.51.0", tgz, sb.pinned)
			sb.pinSum = strings.Repeat("ab", 32)
		}, false},
		"no signature published": {func(sb *pinSandbox) {
			sb.pinSum = sb.publish("1.51.0", tgz, sb.pinned)
			if err := os.Remove(filepath.Join(sb.dir, "origin", "aws-efa-installer-1.51.0.tar.gz.sig")); err != nil {
				t.Fatal(err)
			}
		}, false},
		"the key file holds a second key": {func(sb *pinSandbox) {
			sb.pinSum = sb.publish("1.51.0", tgz, sb.pinned)
			sb.write("scripts/efapin/aws-efa-installer.key", sb.g.gpg("--armor", "--export", sb.pinned, sb.other), 0o644)
		}, false},
	} {
		t.Run(name, func(t *testing.T) {
			sb := newPinSandbox(t, pinScript(t))
			c.setup(sb)
			rc, out := sb.run("verify")
			if (rc == 0) != c.ok {
				t.Errorf("verify exited %d, want success=%v:\n%s", rc, c.ok, out)
			}
		})
	}
}

// The weekly bump pins a sha256 only for a tarball signed by the pinned key,
// and only for a newer version.
func TestBumpPinsOnlyASignedNewerTarball(t *testing.T) {
	tgz := []byte("stand-in EFA installer 1.52.0\n")
	t.Run("signed by the pinned key", func(t *testing.T) {
		sb := newPinSandbox(t, pinScript(t))
		sb.pinSum = strings.Repeat("cd", 32)
		sum := sb.publish("1.52.0", tgz, sb.pinned)
		if rc, out := sb.run("bump", "--version", "1.52.0"); rc != 0 {
			t.Fatalf("bump exited %d:\n%s", rc, out)
		}
		set, _ := os.ReadFile(filepath.Join(sb.dir, "set.log")) //nolint:gosec // the sandbox's own log
		if !strings.Contains(string(set), "--version 1.52.0 --sha256 "+sum) {
			t.Errorf("efapin set got %q, want --version 1.52.0 --sha256 %s", set, sum)
		}
	})
	for name, setup := range map[string]func(sb *pinSandbox){
		"signed by another key": func(sb *pinSandbox) { sb.publish("1.52.0", tgz, sb.other) },
		"altered after signing": func(sb *pinSandbox) {
			sb.publish("1.52.0", tgz, sb.pinned)
			sb.write("origin/aws-efa-installer-1.52.0.tar.gz", []byte("altered\n"), 0o644)
		},
	} {
		t.Run(name, func(t *testing.T) {
			sb := newPinSandbox(t, pinScript(t))
			sb.pinSum = strings.Repeat("cd", 32)
			setup(sb)
			rc, out := sb.run("bump", "--version", "1.52.0")
			if rc == 0 {
				t.Errorf("bump succeeded:\n%s", out)
			}
			if set, err := os.ReadFile(filepath.Join(sb.dir, "set.log")); err == nil { //nolint:gosec // the sandbox's own log
				t.Errorf("efapin set ran for a tarball that is not validly signed: %q", set)
			}
		})
	}
	t.Run("not newer", func(t *testing.T) {
		sb := newPinSandbox(t, pinScript(t))
		sb.pinSum = strings.Repeat("cd", 32)
		if rc, out := sb.run("bump", "--version", "1.50.0"); rc != 0 || !strings.Contains(out, "is current") {
			t.Errorf("bump to an older version: exit %d:\n%s", rc, out)
		}
		if _, err := os.Stat(filepath.Join(sb.dir, "set.log")); err == nil {
			t.Error("efapin set ran for an older version")
		}
	})
}

// The signer check stands on its own (rule 2): with the one-key check taken
// out of the sandbox's copy and both keys in the key file, a signature that
// gpg calls good but that the other key made must still be refused, by the
// VALIDSIG fingerprint test.
func TestVerifyRequiresThePinnedSigner(t *testing.T) {
	keyCheck := regexp.MustCompile(`(?m)^if \[ "\$fprs" != "\$EFA_KEY_FPR" \]; then$`)
	src := pinScript(t)
	if n := len(keyCheck.FindAllIndex(src, -1)); n != 1 {
		t.Fatalf("efa-pin.sh has %d one-key checks, want 1", n)
	}
	sb := newPinSandbox(t, keyCheck.ReplaceAll(src, []byte("if false; then")))
	sb.write("scripts/efapin/aws-efa-installer.key", sb.g.gpg("--armor", "--export", sb.pinned, sb.other), 0o644)
	sb.pinSum = sb.publish("1.51.0", []byte("stand-in\n"), sb.other)
	if rc, out := sb.run("verify"); rc == 0 {
		t.Errorf("a tarball signed by a key other than the pinned one verified:\n%s", out)
	}
	sb.pinSum = sb.publish("1.51.0", []byte("stand-in\n"), sb.pinned)
	if rc, out := sb.run("verify"); rc != 0 {
		t.Errorf("the pinned signer's tarball failed (the sandbox is broken):\n%s", out)
	}
}
