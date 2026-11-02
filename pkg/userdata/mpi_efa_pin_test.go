package userdata

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The #1505 gates. --efa runs the EFA installer as root, so its tarball must be
// the pinned one: fetched from the launch region's spawn-binaries mirror first,
// then from AWS, each source's bytes checked against the pinned sha256 before
// anything is unpacked, and nothing left in /tmp afterwards. A tarball that is
// not the pinned one is never extracted or run, and the member fails closed
// (#1483). With the installer's AL2 support gone (1.50.0), --efa on AL2 is
// refused before any download (#1504).
//
// These run the whole rendered member script in the sandbox, with real tar and
// sha256sum on real gzipped tarballs; only the bytes curl returns are stand-ins
// (efaCurl), and the pinned sha256 is the good stand-in's (pinEFATarball).
// TestEFAPinIsTheGoSourceOfTruth holds the real rendering to efa.go.

// tamperedInstaller would announce itself in efa.log if it ever ran.
const tamperedInstaller = "#!/bin/bash\necho TAMPERED >> \"$HOME/efa.log\"\n"

func curlLog(t *testing.T, r sandboxRun) []string {
	t.Helper()
	log, _ := r.read(t, "curl.log")
	return strings.Fields(log)
}

// leftovers lists what the EFA step may have left under the member's /tmp: its
// work directory, an extracted installer, or a tarball.
func leftovers(t *testing.T, root string) []string {
	t.Helper()
	var found []string
	err := filepath.Walk(filepath.Join(root, "tmp"), func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		n := info.Name()
		if n == "spawn-efa" || n == "aws-efa-installer" || strings.HasSuffix(n, ".tgz") || strings.HasSuffix(n, ".tar.gz") {
			found = append(found, strings.TrimPrefix(p, root))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func TestEFAInstallerNotThePinnedOneIsNeverUnpackedOrRun(t *testing.T) {
	good := efaTarball(t, efaInstallerScript(true))
	pinEFATarball(t, good)
	bad := efaTarball(t, tamperedInstaller)
	for name, served := range map[string]map[string][]byte{
		"tampered at both sources":          {efaMirrorURL: bad, efaOriginURL: bad},
		"tampered at the origin, no mirror": {efaOriginURL: bad},
		"truncated":                         {efaOriginURL: good[:len(good)/2]},
		"neither source serves it":          {},
	} {
		t.Run(name, func(t *testing.T) {
			r := efaCohortServing(t, true, sandboxOpts{}, served)
			assertSetupFailed(t, r, 1)
			if log, ok := r.read(t, "efa.log"); ok {
				t.Errorf("an EFA installer ran from a tarball that is not the pinned one: %q", log)
			}
			if l := leftovers(t, r.root); len(l) != 0 {
				t.Errorf("left under /tmp after refusing the tarball (it must not even be unpacked): %q", l)
			}
			if got := curlLog(t, r); len(got) != 2 || got[0] != efaMirrorURL || got[1] != efaOriginURL {
				t.Errorf("fetched %q, want the mirror and then the origin, each once", got)
			}
			if log, _ := r.read(t, "var/log/spawn-command.log"); !strings.Contains(log, "spawn: no EFA installer "+efaInstallerVersion+" with the pinned sha256") {
				t.Errorf("the failure does not say why:\n%s", log)
			}
			if got := whichMpirun(t, r); got != "" {
				t.Errorf("mpirun %q ran after the EFA installer was refused", got)
			}
		})
	}
}

func TestEFAMirrorIsTriedFirstAndStillSHAChecked(t *testing.T) {
	good := efaTarball(t, efaInstallerScript(true))
	pinEFATarball(t, good)
	bad := efaTarball(t, tamperedInstaller)
	for name, c := range map[string]struct {
		region  string
		served  map[string][]byte
		fetched []string
	}{
		"the mirror has it":            {served: map[string][]byte{efaMirrorURL: good, efaOriginURL: good}, fetched: []string{efaMirrorURL}},
		"the mirror lacks it":          {served: map[string][]byte{efaOriginURL: good}, fetched: []string{efaMirrorURL, efaOriginURL}},
		"the mirror's copy is altered": {served: map[string][]byte{efaMirrorURL: bad, efaOriginURL: good}, fetched: []string{efaMirrorURL, efaOriginURL}},
		"a region with no spawn-binaries bucket": {region: "af-south-1",
			served: map[string][]byte{efaOriginURL: good}, fetched: []string{efaOriginURL}},
	} {
		t.Run(name, func(t *testing.T) {
			r := efaCohortServing(t, true, sandboxOpts{cfg: MPIConfig{Region: c.region}}, c.served)
			if r.rc != 0 {
				t.Fatalf("rank 0 exited %d", r.rc)
			}
			if got := curlLog(t, r); strings.Join(got, " ") != strings.Join(c.fetched, " ") {
				t.Errorf("fetched %q, want %q", got, c.fetched)
			}
			if log, _ := r.read(t, "efa.log"); strings.TrimSpace(log) != "efa_installer.sh -y -g" {
				t.Errorf("efa.log = %q, want the pinned installer run once with -y -g", log)
			}
			if got := whichMpirun(t, r); got != efaMarker {
				t.Errorf("rank 0 ran mpirun %q, want the EFA installer's", got)
			}
		})
	}
}

func TestEFAInstallerLeavesNothingInTmp(t *testing.T) {
	t.Run("installed", func(t *testing.T) {
		r := efaCohort(t, true, sandboxOpts{})
		if r.rc != 0 {
			t.Fatalf("rank 0 exited %d", r.rc)
		}
		if log, _ := r.read(t, "efa.log"); !strings.Contains(log, "efa_installer.sh -y -g") {
			t.Fatalf("the installer did not run: %q", log)
		}
		if l := leftovers(t, r.root); len(l) != 0 {
			t.Errorf("left under /tmp after the EFA install: %q", l)
		}
	})
	// A failing installer still fails the member with its status, and still
	// leaves nothing behind.
	t.Run("the installer fails", func(t *testing.T) {
		failing := efaTarball(t, efaInstallerScript(false)+"exit 3\n")
		pinEFATarball(t, failing)
		r := efaCohortServing(t, true, sandboxOpts{}, map[string][]byte{efaOriginURL: failing})
		assertSetupFailed(t, r, 3)
		if l := leftovers(t, r.root); len(l) != 0 {
			t.Errorf("left under /tmp after the EFA installer failed: %q", l)
		}
	})
}

func TestEFARefusedOnAmazonLinux2(t *testing.T) {
	osRelease := func(body string) func(*testing.T, string) {
		return func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, "etc", "os-release"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	// /etc/os-release as the AMIs ship it.
	const al2 = "NAME=\"Amazon Linux\"\nVERSION=\"2\"\nID=\"amzn\"\nID_LIKE=\"centos rhel fedora\"\nVERSION_ID=\"2\"\nPRETTY_NAME=\"Amazon Linux 2\"\n"
	const al2023 = "NAME=\"Amazon Linux\"\nVERSION=\"2023\"\nID=\"amzn\"\nID_LIKE=\"fedora\"\nVERSION_ID=\"2023\"\nPRETTY_NAME=\"Amazon Linux 2023.9.20251027\"\n"

	r := efaCohort(t, true, sandboxOpts{setup: osRelease(al2)})
	assertSetupFailed(t, r, 1)
	if got := curlLog(t, r); len(got) != 0 {
		t.Errorf("on AL2 the script fetched %q; it must refuse before downloading", got)
	}
	if log, ok := r.read(t, "efa.log"); ok {
		t.Errorf("the EFA installer ran on AL2: %q", log)
	}
	if log, _ := r.read(t, "var/log/spawn-command.log"); !strings.Contains(log, "spawn: --efa needs AL2023 or Ubuntu: the EFA installer dropped Amazon Linux 2") {
		t.Errorf("the AL2 refusal does not say why:\n%s", log)
	}

	r = efaCohort(t, true, sandboxOpts{setup: osRelease(al2023)})
	if r.rc != 0 {
		t.Fatalf("on AL2023 rank 0 exited %d", r.rc)
	}
	if got := whichMpirun(t, r); got != efaMarker {
		t.Errorf("on AL2023 rank 0 ran mpirun %q, want the EFA installer's", got)
	}
}

// The rendered script checks exactly efa.go's pinned sha256 and fetches only
// efa.go's pinned version, from both sources, and never "latest". Read from the
// rendering, on the comparison and the loop themselves (rule 3), with the seam
// at its default.
func TestEFAPinIsTheGoSourceOfTruth(t *testing.T) {
	if efaPinnedSHA256 != efaInstallerSHA256 {
		t.Fatalf("efaPinnedSHA256 = %q, want efaInstallerSHA256 %q", efaPinnedSHA256, efaInstallerSHA256)
	}
	if !regexp.MustCompile(`^1\.\d+\.\d+$`).MatchString(efaInstallerVersion) || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(efaInstallerSHA256) {
		t.Fatalf("pin %q / %q is malformed", efaInstallerVersion, efaInstallerSHA256)
	}
	script, err := GenerateMPIUserData(MPIConfig{Region: "us-west-2", JobArrayID: "a", JobArraySize: 2, EFAEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	cmp := regexp.MustCompile(`(?m)^\s*\[ "\$\(sha256sum e\.tgz \| cut -d ' ' -f 1\)" = (\S+) \] && \{ r=0; break; \}$`).FindAllStringSubmatch(script, -1)
	if len(cmp) != 1 || cmp[0][1] != efaInstallerSHA256 {
		t.Errorf("sha256 comparisons in the EFA step = %q, want exactly one, against efaInstallerSHA256 %s", cmp, efaInstallerSHA256)
	}
	loop := regexp.MustCompile(`(?m)^for u in (.+); do$`).FindAllStringSubmatch(script, -1)
	if len(loop) != 1 {
		t.Fatalf("want exactly one `for u in ...; do` source loop, found %d", len(loop))
	}
	want := []string{
		"'https://spawn-binaries-us-west-2.s3.us-west-2.amazonaws.com/spawn/efa/aws-efa-installer-" + efaInstallerVersion + ".tar.gz'",
		"'https://efa-installer.amazonaws.com/aws-efa-installer-" + efaInstallerVersion + ".tar.gz'",
	}
	if got := strings.Fields(loop[0][1]); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("EFA sources = %q, want %q", got, want)
	}
	if strings.Contains(script, "aws-efa-installer-latest") {
		t.Error("the EFA step still names the moving aws-efa-installer-latest tarball")
	}
	// The download goes to the file the comparison reads.
	if !regexp.MustCompile(`(?m)^\s*if curl -fsS [^\n]*-o e\.tgz "\$u"; then$`).MatchString(script) {
		t.Error("the loop does not download each source to e.tgz")
	}
}
