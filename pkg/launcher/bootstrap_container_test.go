package launcher

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/sporedinstall"
)

// The #1490 bootstrap run on the real bases' userland: the whole inline
// bootstrap in an ubuntu:24.04 container (Ubuntu's bash 5.2, mawk, coreutils,
// no ec2-metadata, no aws CLI) and an amazonlinux:2 one (bash 4.2), with
// --network none. Only what would touch AWS or the machine is a stand-in:
// curl (serving IMDS and the spored bucket from files), aws (when present),
// systemctl, the user tools and su. Never pulls an image: each run skips
// unless the image is already present locally.

// containerCurl is a stand-in curl: IMDSv2 (the instance ID only with the
// token), the regional spawn-binaries bucket from $STUB_ROOT/b, and nothing
// else (no network). It honours -o.
const containerCurl = `#!/bin/bash
out= url= hdr=
while [ $# -gt 0 ]; do
    case "$1" in
        -o) out=$2; shift ;;
        -H) hdr="$hdr|$2"; shift ;;
        -X|--connect-timeout|--max-time|--retry|-m) shift ;;
        http*) url=$1 ;;
    esac
    shift
done
case "$url" in
    http://169.254.169.254/latest/api/token) body=tok ;;
    http://169.254.169.254/latest/meta-data/placement/region) body=eu-west-1 ;;
    http://169.254.169.254/latest/meta-data/instance-id)
        case "$hdr" in *"X-aws-ec2-metadata-token: tok"*) ;; *) exit 22 ;; esac
        [ -e "$STUB_ROOT/no-instance-id" ] && exit 22
        body=i-0ubuntu ;;
    http://169.254.169.254/latest/meta-data/local-ipv4) body=172.31.1.2 ;;
    https://spawn-binaries-eu-west-1.s3.eu-west-1.amazonaws.com/*)
        f="$STUB_ROOT/b/${url#https://spawn-binaries-eu-west-1.s3.eu-west-1.amazonaws.com/}"
        [ -f "$f" ] || exit 22
        if [ -n "$out" ]; then cat "$f" > "$out"; else cat "$f"; fi
        exit 0 ;;
    *) echo "curl: (6) Could not resolve host (no network in this test)" >&2; exit 6 ;;
esac
if [ -n "$out" ]; then printf '%s' "$body" > "$out"; else printf '%s' "$body"; fi
`

// containerTags is a stand-in aws CLI serving the live #1490 shapes: an MPI
// member's array, Name, TTL and on-complete tags.
const containerTags = `#!/bin/bash
echo "$*" >> "$STUB_ROOT/aws-calls"
[ "$2" = describe-tags ] || exit 0
for a; do case "$a" in Name=key,Values=*) k=${a#Name=key,Values=} ;; esac; done
case "$k" in
    spawn:job-array-id) echo arr1 ;;
    spawn:job-array-name) echo sim ;;
    spawn:job-array-size) echo 2 ;;
    spawn:job-array-index) echo 1 ;;
    Name) echo sim-1 ;;
    spawn:ttl) echo 1h ;;
    spawn:on-complete) echo terminate ;;
    spawn:session-timeout) echo 30m ;;
    *) echo None ;;
esac
`

type containerBootRun struct {
	root string
	out  string
	rc   int
}

func (r containerBootRun) read(name string) string {
	return string(readOr(filepath.Join(r.root, name)))
}

// containerBoot runs the inline bootstrap for cfg in image. withAWS puts the
// tag-serving aws stand-in on PATH; without it the image has no aws CLI.
func containerBoot(t *testing.T, image string, cfg BootstrapConfig, withAWS, noInstanceID bool) containerBootRun {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Linux user data")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	if err := exec.Command("docker", "image", "inspect", image).Run(); err != nil { //nolint:gosec // test-built argv
		t.Skipf("%s not present locally (docker pull %s to run this)", image, image)
	}
	// No signing key: the images have no openssl, and the signature path is
	// the stub's and the installer's own gates' business. The sha256 is checked.
	prevKey := sporedinstall.SigningKeyPEM
	t.Cleanup(func() { sporedinstall.SigningKeyPEM = prevKey })
	sporedinstall.SigningKeyPEM = ""

	root := t.TempDir()
	for _, d := range []string{"bin", "tmp", "etc/spawn", "etc/sudoers.d", "etc/profile.d", "etc/ssh", "etc/systemd/system",
		"home", "var/log", "var/lib", "run/spawn", "b/spawn"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	spored := []byte("#!/bin/bash\n[ \"$1\" = is-spot-replacement ] && exit 1\nexit 0\n")
	sum := sha256.Sum256(spored)
	for name, b := range map[string][]byte{
		"b/spawn/spored-linux-amd64":        spored,
		"b/spawn/spored-linux-amd64.sha256": []byte(hex.EncodeToString(sum[:]) + "\n"),
	} {
		if err := os.WriteFile(filepath.Join(root, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if noInstanceID {
		if err := os.WriteFile(filepath.Join(root, "no-instance-id"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ok := "#!/bin/bash\nexit 0\n"
	rec := func(name string) string { return "#!/bin/bash\necho \"$*\" >> \"$STUB_ROOT/" + name + "\"\n" }
	stands := map[string]string{
		"curl": containerCurl, "uname": "#!/bin/bash\necho x86_64\n",
		"systemctl": rec("systemctl-calls"), "hostnamectl": rec("hostnamectl-calls"),
		"logger": rec("journal"), "shutdown": rec("shutdowns"), "poweroff": rec("shutdowns"),
		"id": "#!/bin/bash\nexit 1\n", "useradd": ok, "usermod": ok, "chown": ok,
		"su": "#!/bin/bash\nexec bash \"$4\"\n",
		// Retry back-offs (whole seconds) are skipped; the sub-second polls
		// (the --command drain, the test's own wait) are real.
		"sleep": "#!/bin/bash\necho \"$*\" >> \"$STUB_ROOT/sleeps\"\ncase \"$1\" in *.*) exec /usr/bin/sleep \"$1\" ;; esac\n",
	}
	if withAWS {
		stands["aws"] = containerTags
	}
	for name, body := range stands {
		if err := os.WriteFile(filepath.Join(root, "bin", name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	script, err := buildLinuxBootstrap(cfg, "", "", BootstrapBody())
	if err != nil {
		t.Fatal(err)
	}
	rw := strings.NewReplacer(
		"/etc/", root+"/etc/", "/home/", root+"/home/", "/var/", root+"/var/", "/run/", root+"/run/",
		"/tmp/", root+"/tmp/", "/dev/console", root+"/console", "/usr/local/bin/spored", root+"/spored",
	)
	file := filepath.Join(root, "boot.sh")
	if err := os.WriteFile(file, []byte("echo \"sandbox bash $BASH_VERSION\"\n"+rw.Replace(script)), 0o755); err != nil {
		t.Fatal(err)
	}
	args := []string{"run", "--rm", "--network", "none", "-v", root + ":" + root,
		"--user", strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid()),
		"-e", "PATH=" + root + "/bin:/usr/sbin:/usr/bin:/sbin:/bin", "-e", "STUB_ROOT=" + root, "-e", "HOME=" + root,
		image, "/bin/bash", file}
	out, err := exec.Command("docker", args...).CombinedOutput() //nolint:gosec // test-built argv
	t.Logf("%s boot exited %v:\n%s", image, err, out)
	return containerBootRun{root: root, out: string(out), rc: exitStatus(err)}
}

// TestBootstrapOnUbuntuReadsTheInstancesTags is the live #1490 result turned
// into a gate: on Ubuntu the MOTD showed an empty instance and TTL, idle and
// on-complete "disabled", job-array.sh was missing and the --name hostname was
// never set. With the instance ID from IMDSv2 and the tags read with an aws
// CLI, all of them must come out right; and with no aws CLI (none on the
// image, none downloadable) the bootstrap must stop with a failed record, not
// write a MOTD of defaults.
func TestBootstrapOnUbuntuReadsTheInstancesTags(t *testing.T) {
	waitCmd := "for i in $(seq 1 100); do [ -s /tmp/SPAWN_COMPLETE ] && break; sleep 0.1; done\n"
	cfg := BootstrapConfig{Username: "tester", Command: "echo ran", CustomUserData: waitCmd}
	for _, image := range []string{"ubuntu:24.04", "amazonlinux:2"} {
		t.Run(image+"/tags read", func(t *testing.T) {
			r := containerBoot(t, image, cfg, true, false)
			if r.rc != 0 {
				t.Fatalf("the bootstrap exited %d", r.rc)
			}
			if image == "amazonlinux:2" && !strings.Contains(r.out, "sandbox bash 4.2.") {
				t.Fatal("the amazonlinux:2 run did not run under bash 4.2")
			}
			if strings.Contains(r.out, "command not found") {
				t.Errorf("a tool the bootstrap runs is missing from %s", image)
			}
			motd := r.read("etc/motd")
			for _, want := range []string{"Instance: i-0ubuntu\n", "TTL:              1h\n", "On Complete:      terminate\n", "Instance Index:   1 / 2\n"} {
				if !strings.Contains(motd, want) {
					t.Errorf("the MOTD lacks %q:\n%s", want, motd)
				}
			}
			if ja := r.read("etc/profile.d/job-array.sh"); !strings.Contains(ja, `export JOB_ARRAY_INDEX="1"`) {
				t.Errorf("job-array.sh = %q, want the array's index", ja)
			}
			if h := r.read("hostnamectl-calls"); h != "set-hostname sim-1\n" {
				t.Errorf("hostnamectl was called %q, want the Name tag's hostname", h)
			}
			for _, line := range strings.Split(strings.TrimSpace(r.read("aws-calls")), "\n") {
				if !strings.Contains(line, "Name=resource-id,Values=i-0ubuntu ") {
					t.Errorf("a tag read does not name the instance: %q", line)
				}
			}
			if !fileExists(filepath.Join(r.root, "var/lib/spawn/command-ran-i-0ubuntu")) {
				t.Error("the --command once-marker does not carry the instance ID")
			}
			if rec := r.read("tmp/SPAWN_COMPLETE"); !strings.Contains(rec, `"status": "completed"`) {
				t.Errorf("the --command did not complete: %q", rec)
			}
		})
	}
	t.Run("ubuntu:24.04/no aws CLI and no network", func(t *testing.T) {
		r := containerBoot(t, "ubuntu:24.04", cfg, false, false)
		if r.rc == 0 {
			t.Fatal("the bootstrap exited 0 with no aws CLI")
		}
		if rec := r.read("tmp/SPAWN_COMPLETE"); !strings.Contains(rec, `"source": "bootstrap"`) || !strings.Contains(rec, `"failed"`) {
			t.Errorf("record = %q, want a failed bootstrap record", rec)
		}
		if !strings.Contains(r.out, "could not install AWS CLI "+awsCLIVersion) {
			t.Error("the failure does not say the AWS CLI could not be installed")
		}
		if fileExists(filepath.Join(r.root, "etc/motd")) || strings.Contains(r.read("var/log/spawn-command.log"), "ran") {
			t.Error("the bootstrap carried on: the MOTD was written or the command ran")
		}
	})
	t.Run("ubuntu:24.04/no instance ID", func(t *testing.T) {
		r := containerBoot(t, "ubuntu:24.04", cfg, true, true)
		if r.rc == 0 {
			t.Fatal("the bootstrap exited 0 with no instance ID")
		}
		if rec := r.read("tmp/SPAWN_COMPLETE"); !strings.Contains(rec, `"source": "bootstrap"`) {
			t.Errorf("record = %q, want a failed bootstrap record", rec)
		}
		if r.read("aws-calls") != "" {
			t.Error("tags were read with no instance ID")
		}
	})
}
