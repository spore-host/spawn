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
// publishes spored: a release build's stub fetches spawn/versions/<v>/bootstrap.sh
// from the instance's regional bucket (us-east-1 fallback) and aborts the boot
// without it. These tests RUN the workflow's upload step under bash, with a
// stand-in aws CLI that records every copy and signs with a fake KMS, and a
// stand-in spawn binary; asserting on the YAML text instead would pass for a
// step that names the body in a comment and never uploads it.

const uploadStepName = "Upload spored binaries to S3"

func releaseUploadStep(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(".github/workflows/release.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string `yaml:"name"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(b, &wf); err != nil {
		t.Fatal(err)
	}
	var runs []string
	for _, j := range wf.Jobs {
		for _, s := range j.Steps {
			if s.Name == uploadStepName {
				runs = append(runs, s.Run)
			}
		}
	}
	if len(runs) != 1 || strings.TrimSpace(runs[0]) == "" {
		t.Fatalf("found %d %q steps with a script in release.yaml, want exactly 1", len(runs), uploadStepName)
	}
	return runs[0]
}

type uploadRun struct {
	dir, mirror string
	out         string
	err         error
}

const fakeBody = "fake bootstrap body\nprinted by the release spawn binary\n"

// runUploadStep runs the step in a sandbox. spawnExit is the stand-in spawn's
// exit status; keyID is SPORED_SIGNING_KEY_ID.
func runUploadStep(t *testing.T, keyID string, spawnExit int) uploadRun {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the release step runs on ubuntu-latest under bash")
	}
	for _, tool := range []string{"bash", "openssl", "xargs", "awk"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	dir := t.TempDir()
	mirror := filepath.Join(dir, "s3")
	bin := filepath.Join(dir, "bin")
	for _, d := range []string{bin, mirror, "dist/spored_linux_amd64_v1", "dist/spored_linux_arm64_v8.0", "dist/spored_windows_amd64_v1", "dist/spawn_linux_amd64_v1"} {
		p := d
		if !filepath.IsAbs(d) {
			p = filepath.Join(dir, d)
		}
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, body string, mode os.FileMode) {
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dir, "dist/spored_linux_amd64_v1/spored"), "amd64", 0o755)
	write(filepath.Join(dir, "dist/spored_linux_arm64_v8.0/spored"), "arm64", 0o755)
	write(filepath.Join(dir, "dist/spored_windows_amd64_v1/spored.exe"), "win", 0o755)
	write(filepath.Join(dir, "dist/spawn_linux_amd64_v1/spawn"),
		"#!/bin/bash\n[ \"$1\" = bootstrap-body ] || exit 64\nprintf '%s' '"+fakeBody+"'\nexit "+strconv.Itoa(spawnExit)+"\n", 0o755)
	// aws: `kms sign` returns "KMS(<hex of the digest file>)"; `s3 cp SRC s3://B/K
	// --region R` copies into the mirror, refusing a region that is not B's.
	write(filepath.Join(bin, "aws"), `#!/bin/bash
if [ "$1" = kms ] && [ "$2" = sign ]; then
  while [ $# -gt 0 ]; do [ "$1" = --message ] && f="${2#fileb://}"; shift; done
  printf 'KMS(%s)\n' "$(od -An -tx1 "$f" | tr -d ' \n')"
  exit 0
fi
if [ "$1" = s3 ] && [ "$2" = cp ]; then
  src="$3"; dst="${4#s3://}"; region=""
  while [ $# -gt 0 ]; do [ "$1" = --region ] && region="$2"; shift; done
  [ "${dst%%/*}" = "spawn-binaries-$region" ] || { echo "region $region for $dst" >&2; exit 1; }
  mkdir -p "`+mirror+`/$(dirname "$dst")" && cp "$src" "`+mirror+`/$dst"
  exit $?
fi
echo "unexpected aws $*" >&2; exit 1
`, 0o755)
	if _, err := exec.LookPath("sha256sum"); err != nil {
		write(filepath.Join(bin, "sha256sum"), "#!/bin/bash\nexec shasum -a 256 \"$@\"\n", 0o755)
	}

	// sign_blob writes its digest under /tmp; keep that inside the sandbox.
	script := strings.ReplaceAll(releaseUploadStep(t), "/tmp/", dir+"/")
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd := exec.Command("bash", "-c", script) //nolint:gosec // the repo's own release step
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + bin + ":" + os.Getenv("PATH"), "HOME=" + dir,
		"GITHUB_REF_NAME=v9.9.9", "SPORED_SIGNING_KEY_ID=" + keyID}
	out, err := cmd.CombinedOutput()
	return uploadRun{dir: dir, mirror: mirror, out: string(out), err: err}
}

func (r uploadRun) object(bucket, key string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(r.mirror, bucket, key))
	return string(b), err == nil
}

// sporedBuckets discovers the buckets the step published spored to.
func (r uploadRun) sporedBuckets(t *testing.T) []string {
	t.Helper()
	ents, err := os.ReadDir(r.mirror)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if _, ok := r.object(e.Name(), "spawn/versions/9.9.9/spored-linux-amd64"); ok {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

func TestReleasePublishesTheSignedBootstrapBodyBesideSpored(t *testing.T) {
	r := runUploadStep(t, "alias/test-signing", 0)
	if r.err != nil {
		t.Fatalf("the upload step failed: %v\n%s", r.err, r.out)
	}
	buckets := r.sporedBuckets(t)
	if len(buckets) < 11 {
		t.Fatalf("spored reached %d buckets; the step or this harness has broken\n%s", len(buckets), r.out)
	}
	sum := sha256.Sum256([]byte(fakeBody))
	wantSig := "KMS(" + hex.EncodeToString(sum[:]) + ")\n"
	for _, b := range buckets {
		if got, ok := r.object(b, "spawn/versions/9.9.9/bootstrap.sh"); !ok || got != fakeBody {
			t.Errorf("%s: bootstrap.sh is %q (present %v), want exactly what this release's `spawn bootstrap-body` prints", b, got, ok)
		}
		if got, ok := r.object(b, "spawn/versions/9.9.9/bootstrap.sh.sig"); !ok || got != wantSig {
			t.Errorf("%s: bootstrap.sh.sig is %q (present %v), want the KMS signature of the body's sha256 digest", b, got, ok)
		}
		if got, ok := r.object(b, "spawn/versions/9.9.9/bootstrap.sh.sha256"); !ok || strings.TrimSpace(got) != hex.EncodeToString(sum[:]) {
			t.Errorf("%s: bootstrap.sh.sha256 is %q (present %v)", b, got, ok)
		}
	}
}

func TestReleaseFailsWithoutASignedBootstrapBody(t *testing.T) {
	for name, tc := range map[string]struct {
		keyID     string
		spawnExit int
	}{
		"no signing key":             {"", 0},
		"spawn bootstrap-body fails": {"alias/test-signing", 3},
	} {
		t.Run(name, func(t *testing.T) {
			r := runUploadStep(t, tc.keyID, tc.spawnExit)
			if r.err == nil {
				t.Fatalf("the release step succeeded without a signed bootstrap body; every launch by that release would stop at boot\n%s", r.out)
			}
			if bs := r.sporedBuckets(t); len(bs) != 0 {
				t.Errorf("spored was published to %d buckets by a release that could not publish its body", len(bs))
			}
		})
	}
}
