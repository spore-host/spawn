package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/launcher"
	"github.com/spore-host/spawn/pkg/sporedinstall"
)

// The release mirrors the AWS CLI zips the Linux bootstrap installs into every
// spawn-binaries-<region> bucket (#1494), so an Ubuntu instance in a private
// subnet, which reaches its regional bucket through an S3 endpoint and not
// awscli.amazonaws.com, can still install it. These tests RUN the workflow's
// steps, and through them scripts/mirror-awscli.sh, under bash, with real gpg
// and stand-ins for go (`spawn awscli-pin`), curl (awscli.amazonaws.com and
// the buckets' regional HTTPS names) and aws (s3 cp, list, rm into a local
// mirror). The AWS CLI team's private key is not ours, so the script is run
// with a throwaway signing key's fingerprint in place of the pinned one;
// TestMirrorScriptPinsTheAWSCLITeamKey holds the pinned one.

const (
	mirrorStepName       = "Mirror the pinned AWS CLI to S3"
	mirrorVerifyStepName = "Verify the AWS CLI mirror"
	awsCLITeamFPR        = "FB5DB77FD5C118B80511ADA8A6310ACC4672475C"
	mirrorScript         = "scripts/mirror-awscli.sh"
)

var mirrorFPRLine = regexp.MustCompile(`(?m)^readonly AWSCLI_KEY_FPR=([0-9A-F]{40})$`)

// gpgHome is a throwaway GNUPGHOME holding signing keys. Under /tmp, not
// t.TempDir(): gpg-agent's socket lives in it, and macOS's temp paths are
// longer than a socket path may be.
type gpgHome struct {
	t   *testing.T
	dir string
}

func newGPGHome(t *testing.T) *gpgHome {
	t.Helper()
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

// mirrorSandbox is a release sandbox set up to run the AWS CLI mirror steps.
type mirrorSandbox struct {
	*relSandbox
	gpg      *gpgHome
	fpr      string // the key the sandbox's script pins
	other    string // a second key, not the pinned one
	zips     map[string][]byte
	pinLines string // what the stand-in `go run . awscli-pin` prints
}

// testZip is a stand-in installer zip's bytes for arch at version.
func testZip(arch, version string) []byte {
	return []byte("PK fake AWS CLI " + version + " for " + arch + "\n")
}

func newMirrorSandbox(t *testing.T) *mirrorSandbox {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the release steps run on ubuntu-latest under bash")
	}
	for _, tool := range []string{"cmp", "sed", "sort"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	g := newGPGHome(t)
	sb := &mirrorSandbox{relSandbox: newRelSandbox(t, 0, fakeBody), gpg: g, zips: map[string][]byte{}}
	sb.fpr = g.newKey("spawn test <pinned@example.invalid>")
	sb.other = g.newKey("spawn test <other@example.invalid>")

	// The script, pinning the test key instead of the AWS CLI team's, and that
	// key as the committed key file.
	src, err := os.ReadFile(mirrorScript)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(mirrorFPRLine.FindAllIndex(src, -1)); n != 1 {
		t.Fatalf("%s has %d `readonly AWSCLI_KEY_FPR=` lines, want 1", mirrorScript, n)
	}
	for _, d := range []string{"scripts/awsclipin", "origin"} {
		if err := os.MkdirAll(filepath.Join(sb.dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sb.write(mirrorScript, mirrorFPRLine.ReplaceAllString(string(src), "readonly AWSCLI_KEY_FPR="+sb.fpr))
	sb.write("scripts/awsclipin/aws-cli-team.asc", g.run("--armor", "--export", sb.fpr))

	// The origin: each pinned zip, signed by the pinned key, plus a newer
	// release that must not be mirrored.
	var lines []string
	for _, p := range launcher.AWSCLIPins() {
		b := testZip(p.Arch, p.Version)
		sum := sha256.Sum256(b)
		sb.zips[p.MirrorKey] = b
		sb.origin(filepath.Base(p.MirrorKey), b, sb.fpr)
		lines = append(lines, strings.Join([]string{p.Arch, p.Version, hex.EncodeToString(sum[:]), p.MirrorKey}, " "))
	}
	sb.origin("awscli-exe-linux-x86_64-2.99.0.zip", testZip("x86_64", "2.99.0"), sb.fpr)
	sb.pinLines = strings.Join(lines, "\n") + "\n"

	sb.write("bin/go", `#!/bin/bash
[ "$1 $2 $3" = "run . awscli-pin" ] || { echo "unexpected go $*" >&2; exit 64; }
cat "$FAKE_PINS"
`)
	sb.write("pins", sb.pinLines)
	// curl -o OUT URL: awscli.amazonaws.com from origin/, a bucket's regional
	// HTTPS name from the mirror; anything else, or a missing object, is 22.
	sb.write("bin/curl", `#!/bin/bash
o=""; u=""; prev=""
for a; do [ "$prev" = -o ] && o="$a"; case "$a" in https://*) u="$a" ;; esac; prev="$a"; done
echo "$u" >> "$HOME/curl-calls"
case "$u" in
  https://awscli.amazonaws.com/*) f="$HOME/origin/${u#https://awscli.amazonaws.com/}" ;;
  https://spawn-binaries-*.amazonaws.com/*)
    h="${u#https://}"; host="${h%%/*}"; key="${h#*/}"; b="${host%%.s3.*}"; r="${host#*.s3.}"; r="${r%.amazonaws.com}"
    [ "$b" = "spawn-binaries-$r" ] || { echo "curl: not a regional S3 name: $u" >&2; exit 6; }
    f="`+sb.mirror+`/$b/$key" ;;
  *) echo "curl: unexpected $u" >&2; exit 6 ;;
esac
[ -f "$f" ] || { echo "curl: (22) 404 $u" >&2; exit 22; }
cp "$f" "$o"
`)
	// aws: s3 cp into the mirror (refusing a region not the bucket's),
	// s3api list-objects-v2 as --output text prints Contents[].Key, s3 rm.
	sb.write("bin/aws", `#!/bin/bash
echo "$*" >> "$HOME/aws-calls"
region=""; bucket=""; prefix=""; args=("$@")
for ((i=0; i<$#; i++)); do case "${args[i]}" in --region) region="${args[i+1]}" ;; --bucket) bucket="${args[i+1]}" ;; --prefix) prefix="${args[i+1]}" ;; esac; done
s3path() { local p="${1#s3://}"; [ "${p%%/*}" = "spawn-binaries-$region" ] || { echo "region $region for $1" >&2; exit 1; }; echo "`+sb.mirror+`/$p"; }
if [ "$1 $2" = "s3 cp" ]; then d=$(s3path "$4") || exit 1; mkdir -p "$(dirname "$d")" && cp "$3" "$d"; exit $?; fi
if [ "$1 $2" = "s3 rm" ]; then for a; do case "$a" in s3://*) d=$(s3path "$a") || exit 1; rm "$d" || exit 1 ;; esac; done; exit 0; fi
if [ "$1 $2" = "s3api list-objects-v2" ]; then
  [ "$bucket" = "spawn-binaries-$region" ] || { echo "region $region for $bucket" >&2; exit 1; }
  ks=$(cd "`+sb.mirror+`/$bucket" 2>/dev/null && find . -type f | sed 's#^\./##' | grep "^$prefix" | sort | paste -sd '\t' -)
  echo "${ks:-None}"; exit 0
fi
echo "unexpected aws $*" >&2; exit 1
`)
	return sb
}

// origin publishes name and its detached signature by key at
// awscli.amazonaws.com.
func (sb *mirrorSandbox) origin(name string, b []byte, key string) {
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

func (sb *mirrorSandbox) runMirror(name string) (string, error) {
	sb.t.Helper()
	return sb.run(name, "", "FAKE_PINS="+filepath.Join(sb.dir, "pins"), "AWSCLI_MIRROR_DIR="+filepath.Join(sb.dir, "rt", "awscli-mirror"))
}

// mirrored lists every key under spawn/awscli/ in bucket.
func (sb *mirrorSandbox) mirrored(bucket string) []string {
	var out []string
	root := filepath.Join(sb.mirror, bucket)
	_ = filepath.Walk(filepath.Join(root, "spawn", "awscli"), func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			r, _ := filepath.Rel(root, p)
			out = append(out, filepath.ToSlash(r))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func (sb *mirrorSandbox) publishedBuckets() []string {
	var out []string
	for _, r := range sporedinstall.PublishedRegions() {
		out = append(out, "spawn-binaries-"+r)
	}
	return out
}

// TestReleaseMirrorsExactlyThePinnedAWSCLI: every published region's bucket
// gets exactly the pinned zips, byte for byte, at the keys the bootstrap reads,
// and the newer release at the origin is not mirrored. The verify step then
// passes.
func TestReleaseMirrorsExactlyThePinnedAWSCLI(t *testing.T) {
	sb := newMirrorSandbox(t)
	out, err := sb.runMirror(mirrorStepName)
	if err != nil {
		t.Fatalf("the mirror step failed: %v\n%s", err, out)
	}
	var want []string
	for k := range sb.zips {
		want = append(want, k)
	}
	sort.Strings(want)
	if len(want) != 2 {
		t.Fatalf("the bootstrap pins %d zips, want 2 (x86_64, aarch64)", len(want))
	}
	for _, b := range sb.publishedBuckets() {
		if got := sb.mirrored(b); strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("%s holds %v under spawn/awscli/, want exactly the pinned %v", b, got, want)
		}
		for k, z := range sb.zips {
			if got, _ := sb.object(b, k); got != string(z) {
				t.Errorf("%s/%s is not the verified zip", b, k)
			}
		}
	}
	if out, err := sb.runMirror(mirrorVerifyStepName); err != nil {
		t.Fatalf("the verify step failed on a good mirror: %v\n%s", err, out)
	}
	// verify reads the way instances do: anonymous HTTPS at the regional name.
	calls, _ := os.ReadFile(filepath.Join(sb.dir, "curl-calls"))
	for _, b := range sb.publishedBuckets() {
		r := strings.TrimPrefix(b, "spawn-binaries-")
		for k := range sb.zips {
			if !strings.Contains(string(calls), "https://"+b+".s3."+r+".amazonaws.com/"+k+"\n") {
				t.Errorf("verify did not fetch %s/%s over its regional HTTPS name", b, k)
			}
		}
	}
}

// TestReleaseMirrorRefusesAnUnverifiedAWSCLI: a zip is mirrored only when its
// signature verifies against the committed key, made by the pinned
// fingerprint, AND it has the pinned sha256. Otherwise the step fails and no
// bucket gets anything.
func TestReleaseMirrorRefusesAnUnverifiedAWSCLI(t *testing.T) {
	aarch := func() launcher.AWSCLIPin {
		for _, p := range launcher.AWSCLIPins() {
			if p.Arch == "aarch64" {
				return p
			}
		}
		t.Fatal("no aarch64 pin")
		return launcher.AWSCLIPin{}
	}()
	name := filepath.Base(aarch.MirrorKey)
	for tc, spoil := range map[string]func(*mirrorSandbox){
		"signed by another key": func(sb *mirrorSandbox) { sb.origin(name, sb.zips[aarch.MirrorKey], sb.other) },
		"no signature":          func(sb *mirrorSandbox) { sb.origin(name, sb.zips[aarch.MirrorKey], "") },
		"zip altered after signing": func(sb *mirrorSandbox) {
			sb.write("origin/"+name, string(sb.zips[aarch.MirrorKey])+"x")
		},
		"validly signed but not the pinned sha256": func(sb *mirrorSandbox) {
			sb.origin(name, testZip("aarch64", "different build"), sb.fpr)
		},
		"key file holds a second key": func(sb *mirrorSandbox) {
			sb.write("scripts/awsclipin/aws-cli-team.asc", sb.gpg.run("--armor", "--export", sb.fpr, sb.other))
		},
	} {
		t.Run(tc, func(t *testing.T) {
			sb := newMirrorSandbox(t)
			spoil(sb)
			out, err := sb.runMirror(mirrorStepName)
			if err == nil {
				t.Fatalf("the mirror step succeeded\n%s", out)
			}
			if !strings.Contains(out, "::error::") {
				t.Errorf("the refusal is not a workflow error\n%s", out)
			}
			for _, b := range sb.publishedBuckets() {
				if got := sb.mirrored(b); len(got) != 0 {
					t.Errorf("%s got %v from a step that refused", b, got)
				}
			}
		})
	}
}

// TestReleaseMirrorDeletesOldVersions: the mirror's deleter keeps the pinned
// version and the two newest others, deletes older ones, and leaves keys not
// of its own name form alone.
func TestReleaseMirrorDeletesOldVersions(t *testing.T) {
	sb := newMirrorSandbox(t)
	const b = "spawn-binaries-eu-west-1"
	pinned := launcher.AWSCLIPins()[0].Version
	seed := []string{"2.30.0", "2.38.0", "2.39.0", "2.39.1"}
	foreign := []string{"spawn/awscli/README", "spawn/awscli/awscli-exe-linux-x86_64-2.30.0.zip.bak"}
	for _, v := range seed {
		for _, a := range []string{"x86_64", "aarch64"} {
			sb.write(mkdirFor(t, sb.mirror, b, "spawn/awscli/awscli-exe-linux-"+a+"-"+v+".zip"), "old")
		}
	}
	for _, k := range foreign {
		sb.write(mkdirFor(t, sb.mirror, b, k), "not ours")
	}
	if out, err := sb.runMirror(mirrorStepName); err != nil {
		t.Fatalf("the mirror step failed: %v\n%s", err, out)
	}
	var want []string
	for _, v := range []string{pinned, "2.39.0", "2.39.1"} {
		for _, a := range []string{"x86_64", "aarch64"} {
			want = append(want, "spawn/awscli/awscli-exe-linux-"+a+"-"+v+".zip")
		}
	}
	want = append(want, foreign...)
	sort.Strings(want)
	if got := sb.mirrored(b); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("after publish %s holds\n  %v\nwant\n  %v", b, got, want)
	}
}

// mkdirFor makes the directory for bucket/key in the mirror and returns the
// path relative to the sandbox, for relSandbox.write.
func mkdirFor(t *testing.T, mirror, bucket, key string) string {
	t.Helper()
	p := filepath.Join(mirror, bucket, key)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := filepath.Rel(filepath.Dir(mirror), p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestReleaseMirrorVerifyCatchesABadCopy: a mirrored zip that is missing, or
// is not the pinned bytes, in any one region fails the verify step.
func TestReleaseMirrorVerifyCatchesABadCopy(t *testing.T) {
	for tc, spoil := range map[string]func(sb *mirrorSandbox, path string){
		"altered": func(sb *mirrorSandbox, p string) {
			if err := os.WriteFile(p, []byte("altered"), 0o644); err != nil {
				sb.t.Fatal(err)
			}
		},
		"missing": func(sb *mirrorSandbox, p string) {
			if err := os.Remove(p); err != nil {
				sb.t.Fatal(err)
			}
		},
	} {
		t.Run(tc, func(t *testing.T) {
			sb := newMirrorSandbox(t)
			if out, err := sb.runMirror(mirrorStepName); err != nil {
				t.Fatalf("the mirror step failed: %v\n%s", err, out)
			}
			spoil(sb, filepath.Join(sb.mirror, "spawn-binaries-ap-northeast-1", launcher.AWSCLIPins()[1].MirrorKey))
			out, err := sb.runMirror(mirrorVerifyStepName)
			if err == nil {
				t.Fatalf("verify passed with an %s copy\n%s", tc, out)
			}
			if !strings.Contains(out, "::error::s3://spawn-binaries-ap-northeast-1/") {
				t.Errorf("verify does not name the bad copy\n%s", out)
			}
		})
	}
}

// TestReleaseMirrorsTheAWSCLIBeforeTheCLI: the mirror and its verification
// run after AWS credentials and before GoReleaser publishes a spawn whose
// bootstrap prefers the mirror, and each step invokes the script (anchored,
// so a comment or an echo naming it cannot satisfy this).
func TestReleaseMirrorsTheAWSCLIBeforeTheCLI(t *testing.T) {
	j := releaseJobWithStep(t)
	for name, re := range map[string]*regexp.Regexp{
		mirrorStepName:       regexp.MustCompile(`(?m)\A\s*scripts/mirror-awscli\.sh publish\s*\z`),
		mirrorVerifyStepName: regexp.MustCompile(`(?m)\A\s*scripts/mirror-awscli\.sh verify\s*\z`),
	} {
		if s := j.step(t, name); !re.MatchString(s.Run) {
			t.Errorf("step %q runs %q, want %s", name, s.Run, re)
		}
	}
	mirror := j.index(func(s releaseStep) bool { return s.Name == mirrorStepName })
	verify := j.index(func(s releaseStep) bool { return s.Name == mirrorVerifyStepName })
	gorel := j.index(func(s releaseStep) bool { return strings.HasPrefix(s.Uses, "goreleaser/goreleaser-action@") })
	creds := j.index(func(s releaseStep) bool {
		return strings.HasPrefix(s.Uses, "aws-actions/configure-aws-credentials@") && s.With["role-to-assume"] != ""
	})
	if len(gorel) != 1 || len(creds) == 0 {
		t.Fatalf("found %d GoReleaser and %d credential steps", len(gorel), len(creds))
	}
	if !(creds[0] < mirror[0] && mirror[0] < verify[0] && verify[0] < gorel[0]) {
		t.Errorf("steps: credentials %d, mirror %d, verify %d, GoReleaser %d; want them in that order", creds[0], mirror[0], verify[0], gorel[0])
	}
	for _, i := range append(mirror, verify...) {
		if j.Steps[i].If != "" {
			t.Errorf("step %q is conditional (%q); the mirror is not optional", j.Steps[i].Name, j.Steps[i].If)
		}
	}
}

// TestMirrorScriptPinsTheAWSCLITeamKey: the script accepts signatures only by
// the AWS CLI team's key, as the AWS CLI install guide publishes its
// fingerprint, and the committed key file is that key.
func TestMirrorScriptPinsTheAWSCLITeamKey(t *testing.T) {
	src, err := os.ReadFile(mirrorScript)
	if err != nil {
		t.Fatal(err)
	}
	m := mirrorFPRLine.FindAllSubmatch(src, -1)
	if len(m) != 1 || string(m[0][1]) != awsCLITeamFPR {
		t.Fatalf("%s pins %q, want exactly one `readonly AWSCLI_KEY_FPR=%s`", mirrorScript, m, awsCLITeamFPR)
	}
	if !regexp.MustCompile(`(?m)^readonly KEY_FILE=scripts/awsclipin/aws-cli-team\.asc$`).Match(src) {
		t.Errorf("%s does not read the committed key scripts/awsclipin/aws-cli-team.asc", mirrorScript)
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
