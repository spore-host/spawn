package launcher

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The #1490 behaviour gates, in the boot sandbox (fullBoot: the whole inline
// bootstrap, spored install included, with stand-ins for everything that
// would touch the machine or AWS).
//
// The bootstrap used to read each spawn:* tag as
// `$(aws ec2 describe-tags ... 2>/dev/null || echo <default>)` and the
// instance ID with ec2-metadata. On Ubuntu, which has neither tool, every one
// of those silently became its default. The intent (#1490): a missing
// instance ID, a missing aws CLI that cannot be installed, or a tag read that
// keeps failing stops the bootstrap, which records a failed completion
// (#1484), instead of carrying on with wrong values.

// tagStandIn is an aws CLI that serves describe-tags: a value for the
// job-array and sweep IDs (so those blocks, and the reads inside them, run),
// one parameter, and None (no such tag) for everything else; or, for the key
// failKey, an error every time.
func tagStandIn(failKey string) string {
	return `#!/bin/bash
echo "$*" >> "$HOME/aws-calls"
[ "$2" = describe-tags ] || exit 0
for a; do case "$a" in Name=key,Values=*) k=${a#Name=key,Values=} ;; esac; done
if [ "$k" = '` + failKey + `' ]; then
    echo "An error occurred (RequestLimitExceeded) when calling the DescribeTags operation" >&2
    exit 254
fi
case "$k" in
    spawn:job-array-id|spawn:sweep-id) echo v1 ;;
    'spawn:param:*') printf 'spawn:param:alpha\tbeta\n' ;;
    *) echo None ;;
esac
`
}

var tagKeyRE = regexp.MustCompile(`Name=key,Values=(\S+)`)

// tagKeysRead returns the tag keys a boot read, from the stand-in's log.
func tagKeysRead(sb *bootSandbox) map[string]int {
	keys := map[string]int{}
	for _, line := range strings.Split(sb.read("aws-calls"), "\n") {
		if !strings.Contains(line, "describe-tags") {
			continue
		}
		if m := tagKeyRE.FindStringSubmatch(line); m != nil {
			keys[m[1]]++
		}
	}
	return keys
}

// TestEveryTagReadFailsClosed discovers the tags a boot reads (from a healthy
// boot that takes the job-array and sweep branches), then fails each one in
// turn. Each must stop the bootstrap with a failed `bootstrap` record, after a
// bounded retry, naming the tag; none may fall back to a default.
func TestEveryTagReadFailsClosed(t *testing.T) {
	cfg := BootstrapConfig{Username: "tester", CustomUserData: "echo post > /tmp/POST_RAN\n"}
	sb, out, err := fullBoot(t, cfg, "", bootOpts{standIns: map[string]string{"aws": tagStandIn("")}})
	if err != nil {
		t.Fatalf("a healthy boot failed: %v\n%s", err, out)
	}
	keys := tagKeysRead(sb)
	if len(keys) < 10 {
		t.Fatalf("a healthy boot read only %d tags (%v); the discovery is broken", len(keys), keys)
	}
	for k, n := range keys {
		if n != 1 {
			t.Errorf("a healthy boot read %s %d times, want once", k, n)
		}
	}
	if calls := sb.read("aws-calls"); !strings.Contains(calls, "Name=resource-id,Values=i-0test ") {
		t.Errorf("the tag reads do not name the instance from IMDS:\n%s", calls)
	}
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	t.Logf("tags the bootstrap reads (%d): %s", len(names), strings.Join(names, " "))

	for _, k := range names {
		t.Run(k, func(t *testing.T) {
			sb, out, err := fullBoot(t, cfg, "", bootOpts{standIns: map[string]string{"aws": tagStandIn(k)}})
			if err == nil {
				t.Fatalf("the bootstrap exited 0 although reading %s failed every time: it carried on with a default\n%s", k, out)
			}
			r, ok := readRecord(t, sb)
			if !ok || r.Status != "failed" || r.Source != "bootstrap" {
				t.Errorf("record = %+v (written %v), want a failed bootstrap record", r, ok)
			}
			if !strings.Contains(out, "could not read tag "+k) {
				t.Errorf("the failure does not name the tag %s\n%s", k, out)
			}
			if !strings.Contains(out, "RequestLimitExceeded") {
				t.Errorf("the aws CLI's own error was hidden\n%s", out)
			}
			if n := tagKeysRead(sb)[k]; n != 6 {
				t.Errorf("%s was read %d times, want 6 (a bounded retry)", k, n)
			}
			if fileExists(filepath.Join(sb.dir, "tmp", "POST_RAN")) {
				t.Error("the user data ran after the bootstrap failed")
			}
		})
	}
}

// TestNoInstanceIDFailsClosed: no instance ID from IMDS stops the bootstrap
// before any tag read, with a failed record, instead of reading every tag for
// an empty resource ID (#1490's Ubuntu failure).
func TestNoInstanceIDFailsClosed(t *testing.T) {
	sb, out, err := fullBoot(t, BootstrapConfig{Username: "tester", Command: "echo SHOULD_NOT_RUN"}, "",
		bootOpts{imdsMissing: []string{"/latest/meta-data/instance-id"}})
	if err == nil {
		t.Fatalf("the bootstrap exited 0 with no instance ID\n%s", out)
	}
	if !strings.Contains(out, "no instance ID from IMDS") {
		t.Errorf("the failure does not say why\n%s", out)
	}
	if r, ok := readRecord(t, sb); !ok || r.Status != "failed" || r.Source != "bootstrap" {
		t.Errorf("record = %+v (written %v), want a failed bootstrap record", r, ok)
	}
	if n := len(tagKeysRead(sb)); n != 0 {
		t.Errorf("read %d tags with no instance ID", n)
	}
	if strings.Contains(sb.read("var/log/spawn-command.log"), "SHOULD_NOT_RUN") || strings.Contains(out, "SHOULD_NOT_RUN") {
		t.Error("the --command ran")
	}
}

// testAWSCLIZip is a stand-in for the AWS CLI installer zip: aws/install
// (mode 0755, as in the real zip) installs an aws that serves describe-tags.
func testAWSCLIZip(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	add := func(name, body string, mode uint32) {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		h.SetMode(0)
		h.ExternalAttrs = mode << 16
		w, err := z.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	add("aws/dist/aws", tagStandIn(""), 0o100755)
	add("aws/install", "#!/bin/bash\necho installed >> \"$HOME/awscli-installs\"\ncp \"$(dirname \"$0\")/dist/aws\" \"$HOME/bin/aws\"\n", 0o100755)
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

const awsCLIZipKey = "awscli-exe-linux-x86_64-" + awsCLIVersion + ".zip"

// TestAWSCLIInstalledWhenAbsent: on an image with no aws CLI the bootstrap
// installs the pinned one, from the official installer zip, only if it
// matches the pinned sha256, and then reads its tags with it. A zip that does
// not match is never run, and the bootstrap stops with a failed record.
func TestAWSCLIInstalledWhenAbsent(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	zipBytes := testAWSCLIZip(t)
	sum := sha256.Sum256(zipBytes)
	pin := func(s string) string { return strings.ReplaceAll(s, awsCLISHA256x86_64, hex.EncodeToString(sum[:])) }
	serve := func(b []byte) func(*stubServer) {
		return func(s *stubServer) { s.put("awscli", awsCLIZipKey, b) }
	}
	cfg := BootstrapConfig{Username: "tester", CustomUserData: "echo post > /tmp/POST_RAN\n"}

	t.Run("installed, then used for the tag reads", func(t *testing.T) {
		sb, out, err := fullBoot(t, cfg, "", bootOpts{standIns: map[string]string{"aws": ""}, edit: pin, setup: serve(zipBytes)})
		if err != nil {
			t.Fatalf("the bootstrap failed: %v\n%s", err, out)
		}
		if got := sb.read("awscli-installs"); got != "installed\n" {
			t.Errorf("the AWS CLI installer ran %q, want once", got)
		}
		if !strings.Contains(out, "installing AWS CLI "+awsCLIVersion) {
			t.Errorf("the install is not announced\n%s", out)
		}
		if len(tagKeysRead(sb)) < 10 {
			t.Errorf("the installed CLI did not read the tags: %q", sb.read("aws-calls"))
		}
		if fileExists(filepath.Join(sb.dir, "tmp", "spawn-awscli")) {
			t.Error("the unpacked installer was left in /tmp")
		}
		if !fileExists(filepath.Join(sb.dir, "tmp", "POST_RAN")) {
			t.Error("the user data did not run")
		}
	})
	for name, served := range map[string][]byte{
		"a zip that is not the pinned one": append(append([]byte{}, zipBytes...), 0),
		"no zip to download":               nil,
	} {
		t.Run(name, func(t *testing.T) {
			o := bootOpts{standIns: map[string]string{"aws": ""}, edit: pin}
			if served != nil {
				o.setup = serve(served)
			}
			sb, out, err := fullBoot(t, BootstrapConfig{Username: "tester", Command: "echo SHOULD_NOT_RUN"}, "", o)
			if err == nil {
				t.Fatalf("the bootstrap exited 0 with no aws CLI\n%s", out)
			}
			if sb.read("awscli-installs") != "" {
				t.Error("an AWS CLI that is not the pinned one was installed")
			}
			if served != nil && !strings.Contains(out, "AWS CLI zip: sha256 mismatch") {
				t.Errorf("the refusal does not say the zip failed its sha256 check\n%s", out)
			}
			for _, want := range []string{"could not install AWS CLI " + awsCLIVersion, "no aws CLI to read this instance's tags with"} {
				if !strings.Contains(out, want) {
					t.Errorf("the failure does not say %q\n%s", want, out)
				}
			}
			if r, ok := readRecord(t, sb); !ok || r.Status != "failed" || r.Source != "bootstrap" {
				t.Errorf("record = %+v (written %v), want a failed bootstrap record", r, ok)
			}
			if strings.Contains(out, "SHOULD_NOT_RUN") || fileExists(filepath.Join(sb.dir, "etc", "motd")) {
				t.Error("the bootstrap carried on: the command ran or the MOTD was written")
			}
		})
	}
}

// TestAWSCLIPinIsWellFormed: the pins are what sha256sum -c can check.
func TestAWSCLIPinIsWellFormed(t *testing.T) {
	hexRE := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for arch, h := range map[string]string{"x86_64": awsCLISHA256x86_64, "aarch64": awsCLISHA256aarch64} {
		if !hexRE.MatchString(h) {
			t.Errorf("the %s AWS CLI pin %q is not a lowercase sha256", arch, h)
		}
		if !strings.Contains(awsCLIInstallFunc(), h) {
			t.Errorf("the %s pin is not in spawn_need_aws", arch)
		}
	}
	if !regexp.MustCompile(`^2\.\d+\.\d+$`).MatchString(awsCLIVersion) {
		t.Errorf("AWS CLI version %q is not a v2 release", awsCLIVersion)
	}
}
