package sporedinstall

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// #1597: the .sig covers a spored's bytes alone and its .sha256 comes from the
// same bucket, so before this any spored ever signed, of any version and
// architecture, verified at any release's path. From ManifestFloor on, a
// versioned install also needs the release's manifest signature over the
// binary's name, the version asked for and the bytes downloaded.

// manifestVersion is a pinned release at or past ManifestFloor.
const manifestVersion = "9.9.9"

func TestAVersionedInstallRequiresTheManifestSignature(t *testing.T) {
	requireBash(t)
	where := "/regional/spawn/versions/" + manifestVersion
	for name, tamper := range map[string]func(f *fixture){
		"missing": func(f *fixture) { delete(f.bodies, where+"/"+f.binary+".msig") },
		// An older release's signed spored, replayed at this release's path:
		// its .sig and .sha256 are genuine, its manifest names the old version.
		"another version": func(f *fixture) {
			f.bodies[where+"/"+f.binary+".msig"] = f.manifestSig(f.binary, "0.150.0", f.sum)
		},
		// Another architecture's signed spored under this one's name.
		"another binary": func(f *fixture) {
			f.bodies[where+"/"+f.binary+".msig"] = f.manifestSig("spored-linux-amd64", manifestVersion, f.sum)
		},
		"other bytes": func(f *fixture) {
			f.bodies[where+"/"+f.binary+".msig"] = f.manifestSig(f.binary, manifestVersion, strings.Repeat("0", 64))
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.copyTo(where)
			f.copyTo("/fallback/spawn/versions/" + manifestVersion)
			tamper(f)
			delete(f.bodies, "/fallback/spawn/versions/"+manifestVersion+"/"+f.binary)
			code, out, installed, _ := f.runFromBuckets(manifestVersion)
			if code == 0 || installed {
				t.Fatalf("a spored whose manifest signature is %s was installed (exit %d):\n%s", name, code, out)
			}
			if !strings.Contains(out, "#1597") {
				t.Errorf("the refusal does not say why:\n%s", out)
			}
		})
	}
	t.Run("valid", func(t *testing.T) {
		f := newFixture(t)
		f.copyTo(where)
		code, out, installed, paths := f.runFromBuckets(manifestVersion)
		if code != 0 || !installed {
			t.Fatalf("the signed spored did not install (exit %d):\n%s", code, out)
		}
		if !contains(paths, where+"/"+f.binary+".msig") {
			t.Errorf("never fetched the manifest signature: %v", paths)
		}
	})
}

// A release older than ManifestFloor published no manifest signature, and is
// held to its .sig alone, as before.
func TestAVersionBeforeTheFloorNeedsNoManifestSignature(t *testing.T) {
	requireBash(t)
	const old = "0.100.0"
	if versionAtLeast(old, ManifestFloor) {
		t.Fatalf("%s is not before ManifestFloor %s", old, ManifestFloor)
	}
	f := newFixture(t)
	where := "/regional/spawn/versions/" + old
	f.copyTo(where)
	delete(f.bodies, where+"/"+f.binary+".msig")
	if code, out, installed, _ := f.runFromBuckets(old); code != 0 || !installed {
		t.Fatalf("a pre-floor spored did not install (exit %d):\n%s", code, out)
	}
	if strings.Contains(PinScriptFor(old, false), ManifestVersionVar) {
		t.Errorf("PinScriptFor(%s) asks for a manifest signature", old)
	}
	if !strings.Contains(PinScriptFor(manifestVersion, false), ManifestVersionVar+"='"+manifestVersion+"'") {
		t.Errorf("PinScriptFor(%s) does not ask for its manifest signature", manifestVersion)
	}
}

// The bash installer's printf writes exactly ManifestText.
func TestManifestTextIsWhatEveryWriterWrites(t *testing.T) {
	requireBash(t)
	want := ManifestText("spored-linux-arm64", "1.2.3", strings.Repeat("ab", 32))
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	out, err := exec.Command("bash", "-c", "printf '"+manifestPrintfFormat+"' spored-linux-arm64 1.2.3 "+strings.Repeat("ab", 32)).Output() //nolint:gosec // constant format
	if err != nil || string(out) != want {
		t.Errorf("bash printf wrote %q (%v), want %q", out, err, want)
	}
	// The release workflow signs the same text.
	wf, err := os.ReadFile("../../.github/workflows/release.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^\s*printf '(spored-manifest-v1[^']*)' "\$name" "\$VERSION" "\$\(cat "\$out/\$name\.sha256"\)" > "\$out/\$name\.manifest"$`).FindSubmatch(wf)
	if m == nil || string(m[1]) != manifestPrintfFormat {
		t.Errorf("release.yaml does not sign the manifest with printf '%s' over the name, version and sha256 (#1597)", manifestPrintfFormat)
	}
	for _, want := range []string{`--message "fileb://$out/$name.manifest.digest"`, `> "$out/$name.msig"`, `for f in "$name" "$name.sha256" "$name.sig" "$name.msig"; do`} {
		if !strings.Contains(string(wf), want) {
			t.Errorf("release.yaml lacks %q: the manifest signature is not signed or published", want)
		}
	}
}

func TestVersionAtLeast(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"0.165.0", "0.165.0", true}, {"0.165.1", "0.165.0", true}, {"0.166.0", "0.165.9", true},
		{"1.0.0", "0.165.0", true}, {"0.164.9", "0.165.0", false}, {"0.99.0", "0.165.0", false},
		{"0.x.0", "0.165.0", true},
	} {
		if got := versionAtLeast(tc.a, tc.b); got != tc.want {
			t.Errorf("versionAtLeast(%s, %s) = %t, want %t", tc.a, tc.b, got, tc.want)
		}
	}
}

// ManifestFloor must not name a release that already exists: one tagged
// before this change published no .msig, and every versioned install of it
// would be refused. Read from the tags, which CI fetches.
func TestManifestFloorIsNotAnExistingRelease(t *testing.T) {
	out, err := exec.Command("git", "tag", "--list", "v*").Output()
	if err != nil {
		t.Skipf("no git: %v", err)
	}
	tags := strings.Fields(string(out))
	if len(tags) == 0 {
		t.Skip("no release tags fetched")
	}
	for _, tag := range tags {
		v := strings.TrimPrefix(tag, "v")
		if !versionAtLeast(v, ManifestFloor) || ReleaseVersion(v) != v {
			continue
		}
		wf, err := exec.Command("git", "show", tag+":.github/workflows/release.yaml").Output()
		if err != nil {
			continue
		}
		if !strings.Contains(string(wf), `"$name.msig"`) {
			t.Errorf("release %s is at or past ManifestFloor %s but its release workflow published no .msig: "+
				"raise ManifestFloor past it, or every versioned install of it is refused (#1597)", tag, ManifestFloor)
		}
	}
}
