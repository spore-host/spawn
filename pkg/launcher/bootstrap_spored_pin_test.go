package launcher

import (
	"regexp"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/sporedinstall"
)

// #1412: a launch installs the spored of the spawn that launched it, from
// spawn/versions/<version>/, the release its bootstrap body is pinned to
// (#1274), so the CLI, the bootstrap and spored are one version. A dev build
// keeps the latest. A versioned spored that is missing is fatal, as a bad
// signature is: the instance retires rather than run another release's spored.

const pinRelease = "9.9.9"

// The whole inline bootstrap is executed in the boot sandbox, spored install
// included, against a stand-in bucket.
func TestAReleaseBootstrapInstallsItsOwnSpored(t *testing.T) {
	latest := []string{"spawn/spored-linux-amd64", "spored-linux-amd64"}
	versioned := "spawn/versions/" + pinRelease + "/spored-linux-amd64"
	noLatest := func(t *testing.T, srv *stubServer) {
		t.Helper()
		for _, region := range []string{"eu-west-1", "us-east-1"} {
			for _, k := range latest {
				for _, suf := range []string{"", ".sig", ".sha256"} {
					if n := srv.hit(region, k+suf); n > 0 {
						t.Errorf("a %s bootstrap read the unversioned %s/%s%s %d times (#1412)", pinRelease, region, k, suf, n)
					}
				}
			}
		}
	}

	t.Run("its own spored installs and starts", func(t *testing.T) {
		sb, srv, out, err := fullBootPinned(t, BootstrapConfig{Username: "tester"}, "", pinRelease, "spawn/versions/"+pinRelease)
		if err != nil || !sporedStarted(sb) || retired(sb) {
			t.Fatalf("err=%v spored=%v retired=%v\n%s", err, sporedStarted(sb), retired(sb), out)
		}
		for _, suf := range []string{"", ".sig", ".sha256"} {
			if srv.hit("eu-west-1", versioned+suf) == 0 {
				t.Errorf("never fetched eu-west-1/%s%s", versioned, suf)
			}
		}
		noLatest(t, srv)
	})
	t.Run("with only the latest spored published, the instance retires", func(t *testing.T) {
		sb, srv, out, err := fullBootPinned(t, BootstrapConfig{Username: "tester", CustomUserData: "echo post > /tmp/POST_RAN\n"},
			"", pinRelease, "spawn")
		assertRetired(t, sb, out, err, "spored install failed", awsTerminates)
		if sporedStarted(sb) || fileExists(sb.dir+"/tmp/POST_RAN") {
			t.Errorf("the bootstrap carried on without its own spored\n%s", out)
		}
		if srv.hit("us-east-1", versioned+".sig") == 0 {
			t.Error("the us-east-1 versioned spored was never tried")
		}
		noLatest(t, srv)
	})
	t.Run("a dev build installs the latest", func(t *testing.T) {
		sb, srv, out, err := fullBootPinned(t, BootstrapConfig{Username: "tester"}, "", "", "spawn")
		if err != nil || !sporedStarted(sb) {
			t.Fatalf("err=%v spored=%v\n%s", err, sporedStarted(sb), out)
		}
		if srv.hit("eu-west-1", "spawn/spored-linux-amd64") == 0 {
			t.Error("a dev build did not install the latest spored")
		}
	})
}

// pinLine is the assignment the user data makes; the body reads it.
var pinLine = regexp.MustCompile(`(?m)^` + sporedinstall.PinVar + `='([^']*)'$`)

// The spored a launch installs is the release its CLI is, the one its stub
// fetches the body of, and the one spored will report as spawn:spored-version:
// in rendered terms, the pin equals CallerVersion equals SPAWN_BS_VERSION.
func TestTheSporedPinIsTheCallersRelease(t *testing.T) {
	prev := CallerVersion
	t.Cleanup(func() { CallerVersion = prev })
	for caller, want := range map[string]string{"v" + pinRelease: pinRelease, pinRelease: pinRelease, "dev": "", "0.1.0-0.20260101-abc": "", "0.1.0+dirty": ""} {
		CallerVersion = caller
		if got := SporedVersion(); got != want {
			t.Errorf("CallerVersion %q: SporedVersion() = %q, want %q", caller, got, want)
		}
		script, err := BuildLinuxBootstrap(BootstrapConfig{Username: "ec2-user"})
		if err != nil {
			t.Fatal(err)
		}
		m := pinLine.FindAllStringSubmatch(script, -1)
		if len(m) != 1 || m[0][1] != want {
			t.Errorf("CallerVersion %q: the user data pins spored to %v, want exactly one pin to %q", caller, m, want)
		}
		if want != "" && sporedinstall.VerificationEnabled() && !strings.Contains(script, "\nSPAWN_BS_VERSION='"+want+"'\n") {
			t.Errorf("CallerVersion %q: the spored pin and the bootstrap body are different releases", caller)
		}
		// The pin is assigned before anything that installs spored runs.
		if p, b := strings.Index(script, sporedinstall.PinVar+"="), strings.Index(script, "SPAWN_BS_VERSION="); want != "" && (p < 0 || p > b) {
			t.Errorf("the pin is not assigned ahead of the body")
		}
	}
}

// A library caller (lagotto, the MCP server) pins to the spawn module it was
// built against, as its body does; a replaced spawn names no release.
func TestALibraryCallerPinsToItsSpawnModule(t *testing.T) {
	dep := func(v string, replaced bool) *debug.BuildInfo {
		m := &debug.Module{Path: spawnModulePath, Version: v}
		if replaced {
			m.Replace = &debug.Module{Path: "../.."}
		}
		return &debug.BuildInfo{Main: debug.Module{Path: "github.com/spore-host/lagotto"}, Deps: []*debug.Module{m}}
	}
	if got := publishedBodyVersion("", dep("v"+pinRelease, false), true); got != pinRelease {
		t.Errorf("a library built against spawn v%s pins %q", pinRelease, got)
	}
	if got := publishedBodyVersion("", dep("v"+pinRelease, true), true); got != "" {
		t.Errorf("a replaced spawn pins %q; it names no release", got)
	}
}
