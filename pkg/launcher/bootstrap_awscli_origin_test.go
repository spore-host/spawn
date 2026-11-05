package launcher

import (
	"crypto/sha256"
	"encoding/hex"
	"os/exec"
	"strings"
	"testing"
)

// The #1515 phase 7 gates. spawn does not mirror third-party artifacts, so the
// #1494 regional mirror of the AWS CLI is retired: spawn_need_aws fetches the
// pinned zip from awscli.amazonaws.com only, and installs it only if it has
// the pinned sha256. An instance with no route there gets one through the
// spawn egress proxy (TestAWSCLIOriginThroughTheEgressProxy); the
// --no-public-ip preflight refuses an image that may need it otherwise.

// retiredMirrorKey is where the release used to mirror the x86_64 zip in each
// spawn-binaries-<region> bucket (#1494). The sandbox serves a good zip
// there, so a bootstrap that fetched it again would be counted.
const retiredMirrorKey = "spawn/awscli/" + awsCLIZipKey

func TestAWSCLIOnlyFromTheOriginAndShaChecked(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	good := testAWSCLIZipMarked(t, "good")
	evil := testAWSCLIZipMarked(t, "evil")
	sum := sha256.Sum256(good)
	pinGood := func(s string) string { return strings.ReplaceAll(s, awsCLISHA256x86_64, hex.EncodeToString(sum[:])) }
	const region = "eu-west-1" // the stand-in IMDS's region

	for name, tc := range map[string]struct {
		mirror, origin []byte
		installed      string // the awscli-installs log; "" for none
		ok             bool
	}{
		"the origin serves the pin":                           {origin: good, installed: "good\n", ok: true},
		"the origin wins even with the old mirror populated":  {mirror: good, origin: good, installed: "good\n", ok: true},
		"no origin: the old mirror is not a fallback":         {mirror: good, ok: false},
		"an origin zip that is not the pin is never run":      {origin: evil, ok: false},
		"a bad origin is not rescued by a good mirror either": {mirror: good, origin: evil, ok: false},
	} {
		t.Run(name, func(t *testing.T) {
			var srv *stubServer
			setup := func(s *stubServer) {
				srv = s
				if tc.mirror != nil {
					s.put(region, retiredMirrorKey, tc.mirror)
				}
				if tc.origin != nil {
					s.put("awscli", awsCLIZipKey, tc.origin)
				}
			}
			cfg := BootstrapConfig{Username: "tester", Command: "echo COMMAND_RAN"}
			sb, out, err := fullBoot(t, cfg, "", bootOpts{standIns: map[string]string{"aws": ""}, edit: pinGood, setup: setup})
			if tc.ok != (err == nil) {
				t.Fatalf("bootstrap error = %v, want success %v\n%s", err, tc.ok, out)
			}
			if got := sb.read("awscli-installs"); got != tc.installed {
				t.Errorf("installed %q, want %q", got, tc.installed)
			}
			if strings.Contains(sb.read("awscli-installs"), "evil") {
				t.Error("a zip that is not the pinned one was unpacked and installed")
			}
			if n := srv.hit(region, retiredMirrorKey); n != 0 {
				t.Errorf("the retired regional mirror %s/%s was fetched %d times, want 0 (#1515)", region, retiredMirrorKey, n)
			}
			if n := srv.hit("awscli", awsCLIZipKey); n != 1 {
				t.Errorf("awscli.amazonaws.com was fetched %d times, want 1", n)
			}
			if string(tc.origin) == string(evil) && !strings.Contains(out, ": sha256 mismatch") {
				t.Errorf("an origin zip failing its sha256 is not reported\n%s", out)
			}
			if !tc.ok && strings.Contains(out, "COMMAND_RAN") {
				t.Error("the command ran with no verified aws CLI")
			}
		})
	}
}

// TestSpawnNeedAWSFetchesNothingButTheOrigin: the install function names no
// S3 host at all, and takes no region (the mirror's only use for one).
func TestSpawnNeedAWSFetchesNothingButTheOrigin(t *testing.T) {
	fn := awsCLIInstallFunc()
	for _, banned := range []string{"spawn-binaries", ".s3.", "spawn/awscli", "$1", "region"} {
		if strings.Contains(fn, banned) {
			t.Errorf("spawn_need_aws contains %q: the #1494 mirror is retired (#1515)\n%s", banned, fn)
		}
	}
	if n := strings.Count(fn, "https://awscli.amazonaws.com/awscli-exe-linux-"); n != 1 {
		t.Errorf("spawn_need_aws fetches from awscli.amazonaws.com %d times, want 1\n%s", n, fn)
	}
}

// TestAWSCLIPinsAreTheInstalledOnes: what the release verifies (AWSCLIPins,
// through `spawn awscli-pin`) is what the bootstrap fetches: the same version,
// and the same sha256 for each architecture uname -m names.
func TestAWSCLIPinsAreTheInstalledOnes(t *testing.T) {
	fn := awsCLIInstallFunc()
	arches := map[string]bool{}
	for _, p := range AWSCLIPins() {
		arches[p.Arch] = true
		want := map[string]string{"x86_64": awsCLISHA256x86_64, "aarch64": awsCLISHA256aarch64}[p.Arch]
		if want == "" || p.SHA256 != want || p.Version != awsCLIVersion {
			t.Errorf("AWSCLIPins %+v is not the bootstrap's pin for %s", p, p.Arch)
		}
		if !strings.Contains(fn, p.SHA256) {
			t.Errorf("the %s sha256 is not in spawn_need_aws", p.Arch)
		}
	}
	if len(arches) != 2 || !arches["x86_64"] || !arches["aarch64"] {
		t.Errorf("AWSCLIPins covers %v, want x86_64 and aarch64", arches)
	}
	if !strings.Contains(fn, "awscli-exe-linux-$(uname -m)-"+awsCLIVersion+".zip") {
		t.Errorf("spawn_need_aws does not fetch the pinned version's zip:\n%s", fn)
	}
}
