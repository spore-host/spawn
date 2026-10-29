package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The release workflow must publish the bootstrap body (#1274) everywhere it
// publishes spored, and BEFORE GoReleaser publishes the CLI and the taps: a
// release build's stub fetches spawn/versions/<v>/bootstrap.sh and retires the
// instance without it, so no installable spawn may exist before its body does.
// These tests RUN the workflow's steps under bash, with a stand-in aws CLI
// (fake KMS; s3 cp into a local mirror), a stand-in go and a stand-in spawn;
// asserting on the YAML text instead would pass for a step that names the body
// in a comment and never uploads it.

const (
	bodyStepName   = "Publish the bootstrap body to S3"
	checkStepName  = "Check the released spawn prints the published body"
	sporedStepName = "Publish the versioned spored to S3"
	fakeBody       = "fake bootstrap body\nprinted by spawn bootstrap-body\n"
)

type releaseStep struct {
	Name string            `yaml:"name"`
	ID   string            `yaml:"id"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	If   string            `yaml:"if"`
	With map[string]string `yaml:"with"`
}

type releaseJob struct {
	Env   map[string]string `yaml:"env"`
	Steps []releaseStep     `yaml:"steps"`
}

// releaseJobWithStep returns the one job holding the body step.
func releaseJobWithStep(t *testing.T) releaseJob {
	t.Helper()
	b, err := os.ReadFile(".github/workflows/release.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]releaseJob `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(b, &wf); err != nil {
		t.Fatal(err)
	}
	var found []releaseJob
	for _, j := range wf.Jobs {
		for _, s := range j.Steps {
			if s.Name == bodyStepName {
				found = append(found, j)
			}
		}
	}
	if len(found) != 1 {
		t.Fatalf("found %d jobs with a %q step, want 1", len(found), bodyStepName)
	}
	return found[0]
}

func (j releaseJob) step(t *testing.T, name string) releaseStep {
	t.Helper()
	var out []releaseStep
	for _, s := range j.Steps {
		if s.Name == name {
			out = append(out, s)
		}
	}
	if len(out) != 1 || strings.TrimSpace(out[0].Run) == "" {
		t.Fatalf("found %d %q steps with a script, want exactly 1", len(out), name)
	}
	return out[0]
}

func (j releaseJob) index(pred func(releaseStep) bool) []int {
	var out []int
	for i, s := range j.Steps {
		if pred(s) {
			out = append(out, i)
		}
	}
	return out
}

// TestReleasePublishesTheBodyBeforeTheCLI: credentials, then the body, then
// GoReleaser (which publishes the CLI and the taps), then the check that the
// shipped binary prints the published body.
func TestReleasePublishesTheBodyBeforeTheCLI(t *testing.T) {
	j := releaseJobWithStep(t)
	body := j.index(func(s releaseStep) bool { return s.Name == bodyStepName })
	gorel := j.index(func(s releaseStep) bool { return strings.HasPrefix(s.Uses, "goreleaser/goreleaser-action@") })
	check := j.index(func(s releaseStep) bool { return s.Name == checkStepName })
	creds := j.index(func(s releaseStep) bool {
		return strings.HasPrefix(s.Uses, "aws-actions/configure-aws-credentials@") && s.With["role-to-assume"] != ""
	})
	if len(gorel) != 1 || len(check) != 1 || len(body) != 1 {
		t.Fatalf("found %d GoReleaser, %d body and %d check steps, want one of each", len(gorel), len(body), len(check))
	}
	if !(body[0] < gorel[0]) {
		t.Errorf("the bootstrap body is published at step %d, after GoReleaser (step %d) publishes the CLI: a spawn installed in between launches into a missing body", body[0], gorel[0])
	}
	if !(gorel[0] < check[0]) {
		t.Errorf("the released-binary body check (step %d) runs before GoReleaser builds it (step %d)", check[0], gorel[0])
	}
	if len(creds) == 0 || creds[0] > body[0] {
		t.Errorf("no AWS credentials are configured before the body step (credential steps at %v)", creds)
	}
	if !strings.Contains(j.step(t, checkStepName).If, "!cancelled()") {
		t.Error("the released-binary body check must run even when GoReleaser fails late (the binaries are published by then)")
	}
}

// relSandbox is a directory to run release steps in.
type relSandbox struct {
	t      *testing.T
	dir    string
	mirror string
	job    releaseJob
}

func newRelSandbox(t *testing.T, goExit int, releasedBody string) *relSandbox {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the release steps run on ubuntu-latest under bash")
	}
	for _, tool := range []string{"bash", "openssl", "xargs", "awk", "od"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	dir := t.TempDir()
	sb := &relSandbox{t: t, dir: dir, mirror: filepath.Join(dir, "s3"), job: releaseJobWithStep(t)}
	for _, d := range []string{"bin", "s3", "rt", "dist/spored_linux_amd64_v1", "dist/spored_linux_arm64_v8.0", "dist/spored_windows_amd64_v1", "dist/spawn_linux_amd64_v1"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sb.write("dist/spored_linux_amd64_v1/spored", "amd64")
	sb.write("dist/spored_linux_arm64_v8.0/spored", "arm64")
	sb.write("dist/spored_windows_amd64_v1/spored.exe", "win")
	// The shipped binary GoReleaser built.
	sb.write("dist/spawn_linux_amd64_v1/spawn", "#!/bin/bash\n[ \"$1\" = bootstrap-body ] || exit 64\nprintf '%s' '"+releasedBody+"'\n")
	// go: `go run -ldflags "-X .../cmd.Version=V" . bootstrap-body` from the
	// tagged source. Records its arguments; fails when goExit is non-zero.
	// `go run . spored-signing-key` prints $FAKE_SIGNING_PUB. `go build ... -o
	// OUT PKG` records its arguments and GO*/CGO_* environment in go-builds and
	// writes "spored GOOS/GOARCH" to OUT.
	sb.write("bin/go", `#!/bin/bash
echo "$*" >> `+filepath.Join(dir, "go-calls")+`
if [ "$1" = build ]; then
  { echo CALL; env | grep -E '^C?GO' | sort | sed 's/^/env /'; for a in "$@"; do printf 'arg %s\n' "$a"; done; echo END; } >> `+filepath.Join(dir, "go-builds")+`
  o=""; prev=""; for a in "$@"; do [ "$prev" = -o ] && o="$a"; prev="$a"; done
  printf 'spored %s/%s\n' "$GOOS" "$GOARCH" > "$o"
  exit 0
fi
if [ "$1" = run ] && [ "${!#}" = spored-signing-key ]; then cat "$FAKE_SIGNING_PUB"; exit 0; fi
[ "$1" = run ] && [ "${!#}" = bootstrap-body ] || exit 64
printf '%s' '`+fakeBody+`'
exit `+strconv.Itoa(goExit)+"\n")
	// aws: `kms sign` returns "KMS(<hex of the digest file>)", or, with
	// $FAKE_KMS_KEY set, a real base64 DER ECDSA signature of the digest by that
	// key, as KMS returns; `s3 cp SRC s3://B/K --region R` copies into the
	// mirror and `s3 cp s3://B/K DST` out of it, refusing a region not B's.
	sb.write("bin/aws", `#!/bin/bash
if [ "$1" = kms ] && [ "$2" = sign ]; then
  while [ $# -gt 0 ]; do [ "$1" = --message ] && f="${2#fileb://}"; shift; done
  if [ -n "${FAKE_KMS_KEY:-}" ]; then
    openssl pkeyutl -sign -inkey "$FAKE_KMS_KEY" -in "$f" | openssl base64 -A && echo
    exit $?
  fi
  printf 'KMS(%s)\n' "$(od -An -tx1 "$f" | tr -d ' \n')"
  exit 0
fi
if [ "$1" = s3 ] && [ "$2" = cp ]; then
  src="$3"; dst="$4"; region=""
  while [ $# -gt 0 ]; do [ "$1" = --region ] && region="$2"; shift; done
  if [ "${src#s3://}" != "$src" ]; then
    src="${src#s3://}"
    [ "${src%%/*}" = "spawn-binaries-$region" ] || { echo "region $region for $src" >&2; exit 1; }
    cp "`+sb.mirror+`/$src" "$dst"
    exit $?
  fi
  dst="${dst#s3://}"
  [ "${dst%%/*}" = "spawn-binaries-$region" ] || { echo "region $region for $dst" >&2; exit 1; }
  mkdir -p "`+sb.mirror+`/$(dirname "$dst")" && cp "$src" "`+sb.mirror+`/$dst"
  exit $?
fi
echo "unexpected aws $*" >&2; exit 1
`)
	// gh: `gh release view TAG ... --jq .isDraft` prints $FAKE_GH_DRAFT, or
	// fails as for a release that does not exist when it is unset.
	sb.write("bin/gh", "#!/bin/bash\n[ \"$1 $2\" = \"release view\" ] || exit 64\n[ -n \"${FAKE_GH_DRAFT:-}\" ] || { echo 'release not found' >&2; exit 1; }\necho \"$FAKE_GH_DRAFT\"\n")
	if _, err := exec.LookPath("sha256sum"); err != nil {
		sb.write("bin/sha256sum", "#!/bin/bash\nexec shasum -a 256 \"$@\"\n")
	}
	return sb
}

func (sb *relSandbox) write(rel, body string) {
	sb.t.Helper()
	if err := os.WriteFile(filepath.Join(sb.dir, rel), []byte(body), 0o755); err != nil {
		sb.t.Fatal(err)
	}
}

// run executes the named step with the job's env, as the runner would, plus
// any extra KEY=VALUE pairs for the stand-in tools.
func (sb *relSandbox) run(name, keyID string, extra ...string) (string, error) {
	sb.t.Helper()
	// sign_blob writes its digest under /tmp; keep that inside the sandbox.
	script := strings.ReplaceAll(sb.job.step(sb.t, name).Run, "/tmp/", sb.dir+"/")
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd := exec.Command("bash", "-c", script) //nolint:gosec // the repo's own release steps
	cmd.Dir = sb.dir
	cmd.Env = []string{"PATH=" + filepath.Join(sb.dir, "bin") + ":" + os.Getenv("PATH"), "HOME=" + sb.dir,
		"GITHUB_REF_NAME=v9.9.9", "GITHUB_REPOSITORY=spore-host/spawn", "SPORED_SIGNING_KEY_ID=" + keyID, "RUNNER_TEMP=" + filepath.Join(sb.dir, "rt")}
	cmd.Env = append(cmd.Env, extra...)
	for k, v := range sb.job.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (sb *relSandbox) object(bucket, key string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(sb.mirror, bucket, key))
	return string(b), err == nil
}

// bucketsWith discovers the buckets holding key.
func (sb *relSandbox) bucketsWith(key string) []string {
	sb.t.Helper()
	ents, err := os.ReadDir(sb.mirror)
	if err != nil {
		sb.t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if _, ok := sb.object(e.Name(), key); ok {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

func TestReleasePublishesTheSignedBootstrapBodyBesideSpored(t *testing.T) {
	sb := newRelSandbox(t, 0, fakeBody)
	if out, err := sb.run(bodyStepName, "alias/test-signing"); err != nil {
		t.Fatalf("the body step failed: %v\n%s", err, out)
	}
	keys := sb.signingKeys("release")
	if out, err := sb.run(sporedBuildStepName, ""); err != nil {
		t.Fatalf("the spored build step failed: %v\n%s", err, out)
	}
	if out, err := sb.run(sporedStepName, "alias/test-signing", signEnv(keys, keys)...); err != nil {
		t.Fatalf("the spored step failed: %v\n%s", err, out)
	}
	if out, err := sb.run(checkStepName, "alias/test-signing"); err != nil {
		t.Fatalf("the released-binary check failed on a matching body: %v\n%s", err, out)
	}

	spored := sb.bucketsWith("spawn/versions/9.9.9/spored-linux-amd64")
	bodies := sb.bucketsWith("spawn/versions/9.9.9/bootstrap.sh")
	if len(spored) < 11 {
		t.Fatalf("spored reached %d buckets; the step or this harness has broken", len(spored))
	}
	if strings.Join(bodies, " ") != strings.Join(spored, " ") {
		t.Errorf("the body went to %v, spored to %v: they must be published to the same buckets", bodies, spored)
	}
	calls, _ := os.ReadFile(filepath.Join(sb.dir, "go-calls"))
	if !strings.Contains(string(calls), "-X github.com/spore-host/spawn/cmd.Version=9.9.9") {
		t.Errorf("the body was not generated with the release's version ldflag: go %s", calls)
	}
	sum := sha256.Sum256([]byte(fakeBody))
	wantSig := "KMS(" + hex.EncodeToString(sum[:]) + ")\n"
	for _, b := range spored {
		if got, ok := sb.object(b, "spawn/versions/9.9.9/bootstrap.sh"); !ok || got != fakeBody {
			t.Errorf("%s: bootstrap.sh is %q (present %v), want exactly what `spawn bootstrap-body` prints", b, got, ok)
		}
		if got, ok := sb.object(b, "spawn/versions/9.9.9/bootstrap.sh.sig"); !ok || got != wantSig {
			t.Errorf("%s: bootstrap.sh.sig is %q (present %v), want the KMS signature of the body's sha256 digest", b, got, ok)
		}
		if got, ok := sb.object(b, "spawn/versions/9.9.9/bootstrap.sh.sha256"); !ok || strings.TrimSpace(got) != hex.EncodeToString(sum[:]) {
			t.Errorf("%s: bootstrap.sh.sha256 is %q (present %v)", b, got, ok)
		}
	}
	// GoReleaser refuses a dirty tree, so nothing may be written into the checkout.
	if _, err := os.Stat(filepath.Join(sb.dir, "bootstrap.sh")); err == nil {
		t.Error("the body step wrote bootstrap.sh into the checkout; GoReleaser would refuse the dirty tree")
	}
}

func TestReleaseFailsWithoutASignedBootstrapBody(t *testing.T) {
	for name, tc := range map[string]struct {
		keyID  string
		goExit int
	}{
		"no signing key":                        {"", 0},
		"generating the body from source fails": {"alias/test-signing", 3},
	} {
		t.Run(name, func(t *testing.T) {
			sb := newRelSandbox(t, tc.goExit, fakeBody)
			out, err := sb.run(bodyStepName, tc.keyID)
			if err == nil {
				t.Fatalf("the body step succeeded without a signed body; GoReleaser would then publish a CLI whose every launch retires itself\n%s", out)
			}
			if bs := sb.bucketsWith("spawn/versions/9.9.9/bootstrap.sh"); len(bs) != 0 {
				t.Errorf("a body was published to %d buckets by a step that failed", len(bs))
			}
		})
	}
}

// The shipped binary must print the published bytes; its stub accepts no others.
func TestReleaseFailsWhenTheShippedBodyDiffers(t *testing.T) {
	sb := newRelSandbox(t, 0, "a different body\n")
	if out, err := sb.run(bodyStepName, "alias/test-signing"); err != nil {
		t.Fatalf("the body step failed: %v\n%s", err, out)
	}
	out, err := sb.run(checkStepName, "alias/test-signing")
	if err == nil {
		t.Fatalf("the check passed although the released spawn prints a different body\n%s", out)
	}
	if !strings.Contains(out, "::error::") {
		t.Errorf("the mismatch is not reported as a workflow error\n%s", out)
	}
}
