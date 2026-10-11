package cmd

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/sporedinstall"
)

// Every path that installs spored must use the one verified installer.
//
// spored runs as root and is installed by curl-to-root from an S3 bucket. The
// boot path got this right in spore-host#440; two others — both app user-data
// templates and the SSM upgrade script — shipped without any of it, because each
// was written by copying the shape of the download rather than the shape of the
// verification (GHSA-3x4q-99gh-fxhx, #819).
//
// The gates that existed could not catch it: they asserted things like
// strings.Contains(script, "sha256sum"), which a comment satisfies and a
// fail-OPEN checksum block satisfies equally. These assert the structure instead.

// sporedDownloader matches a reference to a spored release artifact, which is the
// signature of "this file installs spored".
var sporedDownloader = regexp.MustCompile(`spored-linux-`)

// No source file may download spored without calling the shared installer.
//
// Discovery, not a list (CLAUDE.md rule 4): it walks the tree for files
// mentioning a spored artifact rather than naming the three known offenders, so a
// fourth path cannot be added quietly. Fails if discovery finds nothing.
func TestEverySporedDownloaderUsesTheVerifiedInstaller(t *testing.T) {
	root := repoRootFromCmd(t)

	// Where generated install shell can live. pkg/sporedinstall is the
	// implementation itself and is exempt.
	roots := []string{
		filepath.Join(root, "cmd"),
		filepath.Join(root, "pkg"),
	}
	exempt := map[string]bool{
		filepath.Join("pkg", "sporedinstall", "install.go"): true, // the implementation itself
	}

	found := map[string]bool{} // rel path -> calls the installer
	for _, dir := range roots {
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			// Test files are not install paths. Without this the gate flagged
			// ITSELF, since it names the artifact in order to look for it.
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			if exempt[rel] {
				return nil
			}
			b, err := os.ReadFile(path) //nolint:gosec // walking a fixed repo subtree
			if err != nil {
				return err
			}
			body := string(b)
			if !sporedDownloader.MatchString(body) {
				return nil
			}
			// A file that merely names the artifact in a comment or a test
			// expectation is not a downloader; require a fetch beside it.
			if !strings.Contains(body, "curl") && !strings.Contains(body, sporedinstall.FuncName) {
				return nil
			}
			found[rel] = strings.Contains(body, sporedinstall.FuncName)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}

	if len(found) == 0 {
		t.Fatal("found no file that downloads spored — discovery has broken, it has not proven the " +
			"repo clean")
	}
	// The three known call sites plus pkg/launcher's bootstrap. A floor: if
	// discovery drops below it, the match has stopped working rather than the
	// paths having been removed.
	if len(found) < 3 {
		t.Errorf("found only %d spored downloaders (%v); there are at least three call sites, so the "+
			"match has stopped finding some of them", len(found), found)
	}

	for rel, usesInstaller := range found {
		if !usesInstaller {
			t.Errorf("%s downloads spored without calling %s. spored runs as root; every install path "+
				"must verify the checksum AND the signature and fail closed, and there must be one "+
				"implementation of that rather than one per call site (#819)", rel, sporedinstall.FuncName)
		}
	}
}

// No call site may swallow the installer's failure.
//
// `mv /tmp/spored-new /usr/local/bin/spored || true` is what the app paths did:
// even the install failing was discarded. A verified installer whose exit status
// is ignored is not meaningfully better than no installer.
func TestNoCallSiteSwallowsTheInstallerFailure(t *testing.T) {
	root := repoRootFromCmd(t)
	checked := 0

	for _, rel := range []string{
		filepath.Join("cmd", "app.go"),
		filepath.Join("cmd", "upgrade_spored.go"),
		filepath.Join("pkg", "launcher", "bootstrap.go"),
	} {
		b, err := os.ReadFile(filepath.Join(root, rel)) //nolint:gosec // fixed repo-relative path
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		body := string(b)
		if !strings.Contains(body, sporedinstall.FuncName) {
			t.Errorf("%s does not call %s", rel, sporedinstall.FuncName)
			continue
		}
		checked++

		// Any line invoking the installer must not end in a failure-swallowing
		// suffix, and must be reached through a conditional that inspects it.
		for _, line := range strings.Split(body, "\n") {
			if !strings.Contains(line, sporedinstall.FuncName+" ") {
				continue
			}
			if strings.Contains(line, "|| true") {
				t.Errorf("%s swallows the installer's failure with '|| true':\n\t%s",
					rel, strings.TrimSpace(line))
			}
		}
		// The old unverified shape must be gone entirely.
		for _, gone := range []string{
			"/tmp/spored-new",
			"no checksum published for v${TARGET_VERSION}; skipping verification",
		} {
			if strings.Contains(body, gone) {
				t.Errorf("%s still contains the pre-#819 unverified install shape: %q", rel, gone)
			}
		}
	}

	if checked != 3 {
		t.Fatalf("checked %d of 3 call sites; the gate is looking at fewer files than it claims", checked)
	}
}

// The generated app user-data must carry both the installer and a call to it.
//
// The sentinel splice in app.go returns the script unchanged if the sentinel is
// missing, which would be a silent downgrade — so assert the OUTPUT, not the
// splice helper.
func TestGeneratedAppUserDataInstallsSporedVerified(t *testing.T) {
	scripts := map[string]string{
		"web/container app launch": buildWebUserData("ghcr.io/x/y:1", 8080, false, false, "us-east-1", nil),
		"DCV app launch":           buildDCVUserDataWithInit("echo hi", "sess-1", "", "test"),
	}

	for name, encoded := range scripts {
		t.Run(name, func(t *testing.T) {
			raw, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				t.Fatalf("user-data is not valid base64: %v", err)
			}
			script := string(raw)

			if strings.Contains(script, sporedInstallerSentinel) {
				t.Fatal("the installer sentinel is still present in the output — the splice did not " +
					"run, so this user-data installs nothing and would have silently shipped unverified")
			}
			for _, want := range []string{
				sporedinstall.FuncName + "()",      // the definition
				sporedinstall.FuncName + " \"spor", // a call
				"refusing to install an unverified spored",
				"openssl dgst -sha256 -verify",
				sporedinstall.SigningKeyPath,
			} {
				if !strings.Contains(script, want) {
					t.Errorf("generated user-data missing %q", want)
				}
			}
			if strings.Contains(script, "|| true") && strings.Contains(script, "/usr/local/bin/spored || true") {
				t.Error("still installs spored with a swallowed failure")
			}
		})
	}
}

// The upgrade script must fail closed on a missing checksum.
//
// Asserted on the generated text because the behaviour itself is covered by
// pkg/sporedinstall's executable tests; what belongs here is that this path
// routes through them at all.
func TestUpgradeScriptIsFailClosed(t *testing.T) {
	s := buildSporedUpgradeScript("0.128.0")

	if !strings.Contains(s, sporedinstall.FuncName+"()") {
		t.Error("the upgrade script does not embed the verified installer definition")
	}
	if !strings.Contains(s, sporedinstall.FuncName+" \"$BIN\"") {
		t.Error("the upgrade script does not call the verified installer")
	}
	// The fail-open that was here: a missing checksum warned and installed anyway.
	//
	// Matched on NON-COMMENT lines only. The first version of this check matched
	// the whole script, and the comment in upgrade_spored.go explaining the
	// removed fail-open satisfied it — CLAUDE.md rule 3, in its most literal form:
	// the sentence documenting the gate tripped the gate.
	for i, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(trimmed, "skipping verification") {
			t.Errorf("line %d still downgrades to no verification when a checksum is absent:\n\t%s",
				i+1, trimmed)
		}
	}
	if !strings.Contains(s, "refusing to install an unverified spored") {
		t.Error("the upgrade script does not refuse an unverified binary")
	}
	// Signature verification was absent from this path entirely.
	if !strings.Contains(s, "openssl dgst -sha256 -verify") {
		t.Error("the upgrade script does not verify a signature")
	}
	// Rollback still needs a backup taken BEFORE the install, since the installer
	// replaces the binary itself.
	iBackup := strings.Index(s, "BACKUP=$(mktemp")
	iInstall := strings.Index(s, sporedinstall.FuncName+" \"$BIN\"")
	if iBackup < 0 || iInstall < 0 || iBackup > iInstall {
		t.Error("the rollback backup is not taken before the install, so a failed health check has " +
			"nothing to roll back to")
	}
}
