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

	"github.com/spore-host/spawn/pkg/launcher"
)

// Before GoReleaser, the release checks that the AWS CLI zips the Linux
// bootstrap installs on an image with none (stock Ubuntu) are what the AWS CLI
// team signed: each pinned sha256 is that of a zip at awscli.amazonaws.com
// whose detached signature verifies against the committed key. It uploads
// nothing: the #1494 regional mirror is retired (#1515), as spawn does not
// mirror third-party artifacts. These tests RUN the workflow's step, and
// through it scripts/verify-awscli-pin.sh, under bash, with real gpg and
// stand-ins for go (`spawn awscli-pin`), curl (awscli.amazonaws.com) and aws
// (which must never be called). The AWS CLI team's private key is not ours, so
// the script is run with a throwaway signing key's fingerprint in place of the
// pinned one; TestVerifyScriptPinsTheAWSCLITeamKey holds the pinned one.

const (
	verifyPinStepName = "Verify the pinned AWS CLI at its origin"
	awsCLITeamFPR     = "FB5DB77FD5C118B80511ADA8A6310ACC4672475C"
	verifyPinScript   = "scripts/verify-awscli-pin.sh"
)

var verifyFPRLine = regexp.MustCompile(`(?m)^readonly AWSCLI_KEY_FPR=([0-9A-F]{40})$`)

// gpgHome is a throwaway GNUPGHOME holding signing keys. Under /tmp, not
// t.TempDir(): gpg-agent's socket lives in it, and macOS's temp paths are
// longer than a socket path may be.
type gpgHome struct {
	t   *testing.T
	dir string
}

func newGPGHome(t *testing.T) *gpgHome {
	t.Helper()
	// scripts/verify-awscli-pin.sh runs only on the Linux release runner, and
	// Windows has no /tmp (Git for Windows' gpg is on PATH there, so the
	// LookPath below does not skip it).
	if runtime.GOOS == "windows" {
		t.Skip("gpg runs here as on the Linux release runner; not on Windows")
	}
	for _, tool := range []string{"gpg", "gpgconf"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	dir, err := os.MkdirTemp("/tmp", "spawn-gpg")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	g := &gpgHome{t: t, dir: dir}
	t.Cleanup(func() {
		// No agent may outlive the test.
		_ = g.cmd("gpgconf", "--kill", "all").Run()
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

func (g *gpgHome) run(args ...string) string {
	g.t.Helper()
	out, err := g.cmd("gpg", append([]string{"--batch", "--pinentry-mode", "loopback", "--passphrase", ""}, args...)...).Output()
	if err != nil {
		g.t.Fatalf("gpg %v: %v", args, err)
	}
	return string(out)
}

// newKey makes a signing key and returns its fingerprint.
func (g *gpgHome) newKey(uid string) string {
	g.t.Helper()
	g.run("--quick-gen-key", uid, "ed25519", "sign", "never")
	for _, l := range strings.Split(g.run("--with-colons", "--fingerprint", uid), "\n") {
		if f := strings.Split(l, ":"); f[0] == "fpr" && len(f) > 9 {
			return f[9]
		}
	}
	g.t.Fatalf("no fingerprint for %s", uid)
	return ""
}

// pinSandbox is a release sandbox set up to run the AWS CLI pin check.
type pinSandbox struct {
	*relSandbox
	gpg   *gpgHome
	fpr   string            // the key the sandbox's script pins
	other string            // a second key, not the pinned one
	zips  map[string][]byte // official file name -> the pinned bytes
}

// testZip is a stand-in installer zip's bytes for arch at version.
func testZip(arch, version string) []byte {
	return []byte("PK fake AWS CLI " + version + " for " + arch + "\n")
}

// zipName is the official file name of a pin's zip at awscli.amazonaws.com.
func zipName(p launcher.AWSCLIPin) string {
	return "awscli-exe-linux-" + p.Arch + "-" + p.Version + ".zip"
}

func newPinSandbox(t *testing.T) *pinSandbox {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the release steps run on ubuntu-latest under bash")
	}
	g := newGPGHome(t)
	sb := &pinSandbox{relSandbox: newRelSandbox(t, 0, fakeBody), gpg: g, zips: map[string][]byte{}}
	sb.fpr = g.newKey("spawn test <pinned@example.invalid>")
	sb.other = g.newKey("spawn test <other@example.invalid>")

	// The script, pinning the test key instead of the AWS CLI team's, and that
	// key as the committed key file.
	src, err := os.ReadFile(verifyPinScript)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(verifyFPRLine.FindAllIndex(src, -1)); n != 1 {
		t.Fatalf("%s has %d `readonly AWSCLI_KEY_FPR=` lines, want 1", verifyPinScript, n)
	}
	for _, d := range []string{"scripts/awsclipin", "origin"} {
		if err := os.MkdirAll(filepath.Join(sb.dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sb.write(verifyPinScript, verifyFPRLine.ReplaceAllString(string(src), "readonly AWSCLI_KEY_FPR="+sb.fpr))
	sb.write("scripts/awsclipin/aws-cli-team.asc", g.run("--armor", "--export", sb.fpr))

	// The origin: each pinned zip, signed by the pinned key.
	var lines []string
	for _, p := range launcher.AWSCLIPins() {
		b := testZip(p.Arch, p.Version)
		sum := sha256.Sum256(b)
		sb.zips[zipName(p)] = b
		sb.origin(zipName(p), b, sb.fpr)
		lines = append(lines, strings.Join([]string{p.Arch, p.Version, hex.EncodeToString(sum[:])}, " "))
	}
	sb.write("pins", strings.Join(lines, "\n")+"\n")
	sb.write("bin/go", `#!/bin/bash
[ "$1 $2 $3" = "run . awscli-pin" ] || { echo "unexpected go $*" >&2; exit 64; }
cat "$FAKE_PINS"
`)
	// curl -o OUT URL: awscli.amazonaws.com from origin/; anything else is
	// refused, and a missing object is 22.
	sb.write("bin/curl", `#!/bin/bash
o=""; u=""; prev=""
for a; do [ "$prev" = -o ] && o="$a"; case "$a" in https://*) u="$a" ;; esac; prev="$a"; done
echo "$u" >> "$HOME/curl-calls"
case "$u" in
  https://awscli.amazonaws.com/*) f="$HOME/origin/${u#https://awscli.amazonaws.com/}" ;;
  *) echo "curl: unexpected $u" >&2; exit 6 ;;
esac
[ -f "$f" ] || { echo "curl: (22) 404 $u" >&2; exit 22; }
cp "$f" "$o"
`)
	// aws: the check needs no AWS at all; any call is recorded and fails.
	sb.write("bin/aws", `#!/bin/bash
echo "$*" >> "$HOME/aws-calls"
echo "unexpected aws $*" >&2; exit 1
`)
	return sb
}

// origin publishes name and its detached signature by key at
// awscli.amazonaws.com.
func (sb *pinSandbox) origin(name string, b []byte, key string) {
	sb.t.Helper()
	p := filepath.Join(sb.dir, "origin", name)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		sb.t.Fatal(err)
	}
	_ = os.Remove(p + ".sig")
	if key != "" {
		sb.gpg.run("--yes", "-u", key, "--detach-sign", "-o", p+".sig", p)
	}
}

func (sb *pinSandbox) verifyDir() string { return filepath.Join(sb.dir, "rt", "awscli-verify") }

func (sb *pinSandbox) runCheck() (string, error) {
	sb.t.Helper()
	return sb.run(verifyPinStepName, "", "FAKE_PINS="+filepath.Join(sb.dir, "pins"), "AWSCLI_VERIFY_DIR="+sb.verifyDir())
}

// noAWS fails the test if the step made any AWS call: the retired mirror's
// upload, prune or read-back would.
func (sb *pinSandbox) noAWS() {
	sb.t.Helper()
	if calls, err := os.ReadFile(filepath.Join(sb.dir, "aws-calls")); err == nil {
		sb.t.Errorf("the AWS CLI pin check called aws (it must upload nothing):\n%s", calls)
	}
	if _, err := os.Stat(sb.mirror); err == nil {
		ents, _ := os.ReadDir(sb.mirror)
		if len(ents) != 0 {
			sb.t.Errorf("the AWS CLI pin check wrote to the stand-in buckets: %v", ents)
		}
	}
}

// TestReleaseVerifiesThePinnedAWSCLI: with every pinned zip validly signed at
// the origin, the step passes, fetches exactly the pinned zips and their
// signatures from awscli.amazonaws.com, makes no AWS call (nothing is
// mirrored) and removes what it downloaded.
func TestReleaseVerifiesThePinnedAWSCLI(t *testing.T) {
	sb := newPinSandbox(t)
	out, err := sb.runCheck()
	if err != nil {
		t.Fatalf("the check failed on validly signed pins: %v\n%s", err, out)
	}
	if len(sb.zips) != 2 {
		t.Fatalf("the bootstrap pins %d zips, want 2 (x86_64, aarch64)", len(sb.zips))
	}
	calls, _ := os.ReadFile(filepath.Join(sb.dir, "curl-calls"))
	var want []string
	for name := range sb.zips {
		want = append(want, "https://awscli.amazonaws.com/"+name, "https://awscli.amazonaws.com/"+name+".sig")
	}
	got := strings.Fields(string(calls))
	if len(got) != len(want) {
		t.Errorf("fetched %v, want exactly %v", got, want)
	}
	for _, u := range want {
		if !strings.Contains(string(calls), u+"\n") {
			t.Errorf("did not fetch %s", u)
		}
	}
	sb.noAWS()
	if _, err := os.Stat(sb.verifyDir()); err == nil {
		t.Errorf("%s was left behind", sb.verifyDir())
	}
}

// TestReleaseRefusesAnUnverifiedAWSCLIPin: the check passes only when each
// pinned zip's signature verifies against the committed key, made by the
// pinned fingerprint, AND it has the pinned sha256. Otherwise the step fails
// as a workflow error, before GoReleaser.
func TestReleaseRefusesAnUnverifiedAWSCLIPin(t *testing.T) {
	aarch := func() launcher.AWSCLIPin {
		for _, p := range launcher.AWSCLIPins() {
			if p.Arch == "aarch64" {
				return p
			}
		}
		t.Fatal("no aarch64 pin")
		return launcher.AWSCLIPin{}
	}()
	name := zipName(aarch)
	for tc, spoil := range map[string]func(*pinSandbox){
		"signed by another key": func(sb *pinSandbox) { sb.origin(name, sb.zips[name], sb.other) },
		"no signature":          func(sb *pinSandbox) { sb.origin(name, sb.zips[name], "") },
		"zip altered after signing": func(sb *pinSandbox) {
			sb.write("origin/"+name, string(sb.zips[name])+"x")
		},
		"validly signed but not the pinned sha256": func(sb *pinSandbox) {
			sb.origin(name, testZip("aarch64", "different build"), sb.fpr)
		},
		"not at the origin": func(sb *pinSandbox) {
			if err := os.Remove(filepath.Join(sb.dir, "origin", name)); err != nil {
				sb.t.Fatal(err)
			}
		},
		"key file holds a second key": func(sb *pinSandbox) {
			sb.write("scripts/awsclipin/aws-cli-team.asc", sb.gpg.run("--armor", "--export", sb.fpr, sb.other))
		},
	} {
		t.Run(tc, func(t *testing.T) {
			sb := newPinSandbox(t)
			spoil(sb)
			out, err := sb.runCheck()
			if err == nil {
				t.Fatalf("the check passed\n%s", out)
			}
			if !strings.Contains(out, "::error::") {
				t.Errorf("the refusal is not a workflow error\n%s", out)
			}
			sb.noAWS()
		})
	}
}

// TestReleaseAWSCLIPinRequiresThePinnedSigner tests the VALIDSIG fingerprint
// check where it lives (rule 2). Through the script it is unreachable while
// the one-primary-key check holds, so this disables that check in the
// sandbox's copy, imports a second key, and signs with it: gpg calls the
// signature good, and the fingerprint check alone must refuse it.
func TestReleaseAWSCLIPinRequiresThePinnedSigner(t *testing.T) {
	sb := newPinSandbox(t)
	const keyCheck = `if [ "$fprs" != "$AWSCLI_KEY_FPR" ]; then`
	src, err := os.ReadFile(filepath.Join(sb.dir, verifyPinScript))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(src), keyCheck) != 1 {
		t.Fatalf("%s no longer has the key check %q this test disables", verifyPinScript, keyCheck)
	}
	sb.write(verifyPinScript, strings.Replace(string(src), keyCheck, "if false; then", 1))
	sb.write("scripts/awsclipin/aws-cli-team.asc", sb.gpg.run("--armor", "--export", sb.fpr, sb.other))
	name := zipName(launcher.AWSCLIPins()[0])
	sb.origin(name, sb.zips[name], sb.other)
	out, err := sb.runCheck()
	if err == nil {
		t.Fatalf("a zip signed by a key other than the pinned one passed\n%s", out)
	}
	if !strings.Contains(out, "no VALIDSIG by "+sb.fpr) {
		t.Errorf("the refusal is not the fingerprint check's\n%s", out)
	}
}

// TestReleaseVerifiesTheAWSCLIPinBeforeTheCLI: the check runs before GoReleaser
// publishes a spawn carrying the pin, unconditionally, and invokes the script
// (anchored, so a comment or an echo naming it cannot satisfy this). The
// retired mirror steps, and anything else that would upload the AWS CLI to
// the spawn-binaries buckets, are gone (#1515).
func TestReleaseVerifiesTheAWSCLIPinBeforeTheCLI(t *testing.T) {
	j := releaseJobWithStep(t)
	re := regexp.MustCompile(`(?m)\A\s*scripts/verify-awscli-pin\.sh\s*\z`)
	if s := j.step(t, verifyPinStepName); !re.MatchString(s.Run) {
		t.Errorf("step %q runs %q, want %s", verifyPinStepName, s.Run, re)
	}
	check := j.index(func(s releaseStep) bool { return s.Name == verifyPinStepName })
	gorel := j.index(func(s releaseStep) bool { return strings.HasPrefix(s.Uses, "goreleaser/goreleaser-action@") })
	if len(check) != 1 || len(gorel) != 1 {
		t.Fatalf("found %d check and %d GoReleaser steps, want 1 each", len(check), len(gorel))
	}
	if check[0] > gorel[0] {
		t.Errorf("the AWS CLI pin check is step %d, after GoReleaser at %d", check[0], gorel[0])
	}
	if j.Steps[check[0]].If != "" {
		t.Errorf("step %q is conditional (%q); the check is not optional", verifyPinStepName, j.Steps[check[0]].If)
	}
	for _, s := range j.Steps {
		if strings.Contains(s.Run, "mirror-awscli") || strings.Contains(s.Run, "spawn/awscli") {
			t.Errorf("step %q still publishes the retired AWS CLI mirror (#1515):\n%s", s.Name, s.Run)
		}
	}
	if _, err := os.Stat("scripts/mirror-awscli.sh"); err == nil {
		t.Error("scripts/mirror-awscli.sh, the retired AWS CLI mirror (#1494), is back")
	}
}

// TestVerifyScriptPinsTheAWSCLITeamKey: the script accepts signatures only by
// the AWS CLI team's key, as the AWS CLI install guide publishes its
// fingerprint, and the committed key file is that key.
func TestVerifyScriptPinsTheAWSCLITeamKey(t *testing.T) {
	src, err := os.ReadFile(verifyPinScript)
	if err != nil {
		t.Fatal(err)
	}
	m := verifyFPRLine.FindAllSubmatch(src, -1)
	if len(m) != 1 || string(m[0][1]) != awsCLITeamFPR {
		t.Fatalf("%s pins %q, want exactly one `readonly AWSCLI_KEY_FPR=%s`", verifyPinScript, m, awsCLITeamFPR)
	}
	if !regexp.MustCompile(`(?m)^readonly KEY_FILE=scripts/awsclipin/aws-cli-team\.asc$`).Match(src) {
		t.Errorf("%s does not read the committed key scripts/awsclipin/aws-cli-team.asc", verifyPinScript)
	}
	g := newGPGHome(t)
	g.run("--import", "scripts/awsclipin/aws-cli-team.asc")
	var fprs []string
	want := false
	for _, l := range strings.Split(g.run("--with-colons", "--fixed-list-mode", "--list-keys"), "\n") {
		f := strings.Split(l, ":")
		switch {
		case f[0] == "pub":
			want = true
		case f[0] == "fpr" && want && len(f) > 9:
			fprs = append(fprs, f[9])
			want = false
		}
	}
	if strings.Join(fprs, " ") != awsCLITeamFPR {
		t.Errorf("scripts/awsclipin/aws-cli-team.asc holds primary keys %v, want exactly %s", fprs, awsCLITeamFPR)
	}
}
