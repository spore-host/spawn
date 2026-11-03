package launcher

import (
	"crypto/sha256"
	"encoding/hex"
	"os/exec"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/syntax"
)

// The #1494 gates. An Ubuntu instance in a private subnet with only VPC
// endpoints cannot reach awscli.amazonaws.com, so spawn_need_aws fetches the
// pinned zip from the launch region's spawn-binaries bucket first, where the
// release workflow mirrors it, and from awscli.amazonaws.com second. Whatever
// the source, the bytes must match the pinned sha256 before anything is
// unpacked: the mirror is no more trusted than the origin.

func x86Pin(t *testing.T) AWSCLIPin {
	t.Helper()
	for _, p := range AWSCLIPins() {
		if p.Arch == "x86_64" {
			return p
		}
	}
	t.Fatal("AWSCLIPins has no x86_64 zip")
	return AWSCLIPin{}
}

func TestAWSCLIMirrorTriedFirstAndShaChecked(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	good := testAWSCLIZipMarked(t, "good")
	evil := testAWSCLIZipMarked(t, "evil")
	sum := sha256.Sum256(good)
	pinGood := func(s string) string { return strings.ReplaceAll(s, awsCLISHA256x86_64, hex.EncodeToString(sum[:])) }
	mirrorKey := x86Pin(t).MirrorKey
	const region = "eu-west-1" // the stand-in IMDS's region

	for name, tc := range map[string]struct {
		mirror, origin []byte
		installed      string // the awscli-installs log; "" for none
		mirrorHits     int
		originHits     int
		ok             bool
	}{
		"the mirror alone serves the pin":               {mirror: good, installed: "good\n", mirrorHits: 1, ok: true},
		"the mirror wins over the origin":               {mirror: good, origin: good, installed: "good\n", mirrorHits: 1, ok: true},
		"no mirror: the origin":                         {origin: good, installed: "good\n", mirrorHits: 1, originHits: 1, ok: true},
		"a mirror zip that is not the pin is never run": {mirror: evil, origin: good, installed: "good\n", mirrorHits: 1, originHits: 1, ok: true},
		"a bad mirror and no origin fail closed":        {mirror: evil, mirrorHits: 1, originHits: 1, ok: false},
		"the origin is sha-checked too, after a miss":   {origin: evil, mirrorHits: 1, originHits: 1, ok: false},
		"a bad mirror and a bad origin fail closed":     {mirror: evil, origin: evil, mirrorHits: 1, originHits: 1, ok: false},
	} {
		t.Run(name, func(t *testing.T) {
			var srv *stubServer
			setup := func(s *stubServer) {
				srv = s
				if tc.mirror != nil {
					s.put(region, mirrorKey, tc.mirror)
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
			if n := srv.hit(region, mirrorKey); n != tc.mirrorHits {
				t.Errorf("the regional mirror %s/%s was fetched %d times, want %d", region, mirrorKey, n, tc.mirrorHits)
			}
			if n := srv.hit("awscli", awsCLIZipKey); n != tc.originHits {
				t.Errorf("awscli.amazonaws.com was fetched %d times, want %d", n, tc.originHits)
			}
			if string(tc.mirror) == string(evil) && !strings.Contains(out, "/"+awsCLIMirrorDir+": sha256 mismatch") {
				t.Errorf("a mirror zip failing its sha256 is not reported\n%s", out)
			}
		})
	}
}

// TestEverySpawnNeedAWSCallNamesItsRegion: spawn_need_aws takes the region
// whose mirror it tries. Called with nothing, it would ask for
// spawn-binaries-.s3..amazonaws.com and quietly fall back to the origin, which
// is the private-subnet failure this mirror exists for. Every call in every
// rendered bootstrap must pass the region the script read: the instance's
// ($REGION), the staged files' bucket's ($SPAWN_ST_REGION, in the launch
// region, and fetched before the body reads $REGION), or the launch region the
// egress proxy's install names ($SPAWN_PX_REGION, #1515, also before the body).
func TestEverySpawnNeedAWSCallNamesItsRegion(t *testing.T) {
	calls := 0
	for name, script := range toolMatrix(t) {
		f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(script), name)
		if err != nil {
			t.Fatal(err)
		}
		syntax.Walk(f, func(n syntax.Node) bool {
			c, ok := n.(*syntax.CallExpr)
			if !ok || len(c.Args) == 0 || c.Args[0].Lit() != "spawn_need_aws" {
				return true
			}
			calls++
			var arg strings.Builder
			if len(c.Args) == 2 {
				_ = syntax.NewPrinter().Print(&arg, c.Args[1])
			}
			if a := arg.String(); a != `"$REGION"` && a != `"$SPAWN_ST_REGION"` && a != `"$SPAWN_PX_REGION"` {
				t.Errorf("%s: spawn_need_aws called with %d args (%q), want one region variable", name, len(c.Args)-1, a)
			}
			return true
		})
	}
	if calls == 0 {
		t.Fatal("found no spawn_need_aws call in any rendered bootstrap; the discovery is broken")
	}
}

// TestAWSCLIPinsAreTheInstalledOnes: what the release mirrors (AWSCLIPins,
// through `spawn awscli-pin`) is what the bootstrap fetches: the same version,
// the same sha256 for each architecture uname -m names, and a key under the
// directory spawn_need_aws reads.
func TestAWSCLIPinsAreTheInstalledOnes(t *testing.T) {
	fn := awsCLIInstallFunc()
	arches := map[string]bool{}
	for _, p := range AWSCLIPins() {
		arches[p.Arch] = true
		want := map[string]string{"x86_64": awsCLISHA256x86_64, "aarch64": awsCLISHA256aarch64}[p.Arch]
		if want == "" || p.SHA256 != want || p.Version != awsCLIVersion {
			t.Errorf("AWSCLIPins %+v is not the bootstrap's pin for %s", p, p.Arch)
		}
		if p.MirrorKey != awsCLIMirrorDir+"/awscli-exe-linux-"+p.Arch+"-"+awsCLIVersion+".zip" {
			t.Errorf("%s mirror key %q is not <dir>/<official zip name>", p.Arch, p.MirrorKey)
		}
		if !strings.Contains(fn, p.SHA256) {
			t.Errorf("the %s sha256 is not in spawn_need_aws", p.Arch)
		}
	}
	if len(arches) != 2 || !arches["x86_64"] || !arches["aarch64"] {
		t.Errorf("AWSCLIPins covers %v, want x86_64 and aarch64", arches)
	}
	if !strings.Contains(fn, ".amazonaws.com/"+awsCLIMirrorDir+`" https://awscli.amazonaws.com;`) {
		t.Errorf("spawn_need_aws does not try the regional mirror at %s, then awscli.amazonaws.com:\n%s", awsCLIMirrorDir, fn)
	}
}
