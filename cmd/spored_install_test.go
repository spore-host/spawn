package cmd

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spore-host/spawn/internal/treewalk"
	"github.com/spore-host/spawn/pkg/launcher"
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

// installerCall matches an INVOCATION of the shell installer as a command, the
// way every call site writes it (`if ! spore_install_verified_spored "$BIN" ...`),
// not the bare name: a comment or a log line naming the function must not count
// as calling it (CLAUDE.md rule 3).
//
// Since #1412 every call site calls BucketInstallFuncName, which picks the
// sources from the pinned spored version, not FuncName with its own list of
// bases: a hand-written base list is how a path would install the latest spored
// under a pinned CLI.
var installerCall = regexp.MustCompile(`(?m)^\s*(?:if\s+!\s+)?` + sporedinstall.BucketInstallFuncName + `\s+"`)

// directInstallerCall matches a call site that invokes FuncName with its own
// bases, bypassing the pinned source selection (#1412).
var directInstallerCall = regexp.MustCompile(`(?m)^\s*(?:if\s+!\s+)?` + sporedinstall.FuncName + `\s+"`)

// pinCall matches the Go call that assigns the spored version the script
// installs (#1412). A script that calls the installer without it installs the
// latest spored whatever spawn rendered it.
var pinCall = regexp.MustCompile(`\bsporedinstall\.PinScript(For)?\(`)

// keySetupCall matches the Go call that emits SPORED_SIG_VERIFY and the signing
// key. A script that calls the installer without it runs with verification OFF:
// the function treats an unset SPORED_SIG_VERIFY as "no key", checks only the
// sha256, and installs. That is the fail-open this file exists to prevent.
var keySetupCall = regexp.MustCompile(`\bsporedinstall\.KeySetupScript\(\)`)

// skipDirs are never source: VCS metadata and third-party trees.
var skipDirs = map[string]bool{".git": true, "node_modules": true, "vendor": true}

// discoverSporedDownloaders walks EVERY .go file in the repository, nested
// modules included (lambda/*, scripts/), and returns the non-test files that
// download spored, keyed by repo-relative path, with their contents.
//
// The whole repository, not cmd/ and pkg/: the first version walked only those
// two, so the pipeline orchestrator Lambda, which generates the user data of
// every pipeline stage instance, installed spored with its own inline curl and
// a sha256 from the same bucket, never checking the signature, and the gate
// never looked at it (#1173). Discovery, not a list (CLAUDE.md rule 4). Fails
// if discovery finds nothing.
func discoverSporedDownloaders(t *testing.T) map[string]string {
	t.Helper()
	root := repoRootFromCmd(t)
	exempt := map[string]bool{
		filepath.Join("pkg", "sporedinstall", "install.go"): true, // the implementation itself
	}

	found := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if treewalk.Vanished(root, path, err) {
				return nil
			}
			return err
		}
		if info.IsDir() {
			if path != root && skipDirs[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		// Test files are not install paths. Without this the gate flagged
		// ITSELF, since it names the artifact in order to look for it.
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if exempt[rel] {
			return nil
		}
		b, err := os.ReadFile(path) //nolint:gosec // walking the repo's own tree
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
		found[rel] = body
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(found) == 0 {
		t.Fatal("found no file that downloads spored — discovery has broken, it has not proven the " +
			"repo clean")
	}
	// A floor: there are at least three call sites (cmd/app.go,
	// cmd/upgrade_spored.go, pkg/launcher/bootstrap.go). If discovery drops below
	// it, the match has stopped working rather than the paths having been removed.
	if len(found) < 3 {
		t.Errorf("found only %d spored downloaders (%v); there are at least three call sites, so the "+
			"match has stopped finding some of them", len(found), keysOf(found))
	}
	return found
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// No source file anywhere in the repository may download spored without calling
// the shared installer, with the signing key set up.
func TestEverySporedDownloaderUsesTheVerifiedInstaller(t *testing.T) {
	for rel, body := range discoverSporedDownloaders(t) {
		if !installerCall.MatchString(body) {
			t.Errorf("%s downloads spored without calling %s. spored runs as root; every install path "+
				"must verify the checksum AND the signature and fail closed, and there must be one "+
				"implementation of that rather than one per call site (#819, #1173, #1412)", rel,
				sporedinstall.BucketInstallFuncName)
			continue
		}
		if directInstallerCall.MatchString(body) {
			t.Errorf("%s calls %s with its own sources instead of %s, so it does not install the spored "+
				"of the spawn that rendered it (#1412)", rel, sporedinstall.FuncName, sporedinstall.BucketInstallFuncName)
		}
		if !pinCall.MatchString(body) {
			t.Errorf("%s calls %s but never emits sporedinstall.PinScript(), so %s is unset and every "+
				"launch installs the latest spored, not its own spawn's (#1412)", rel,
				sporedinstall.BucketInstallFuncName, sporedinstall.PinVar)
		}
		if !keySetupCall.MatchString(body) {
			t.Errorf("%s calls %s but never emits sporedinstall.KeySetupScript(), so SPORED_SIG_VERIFY "+
				"is unset and the installer checks only the sha256: the signature is never verified", rel,
				sporedinstall.FuncName)
		}
	}
}

// No call site may swallow the installer's failure.
//
// `mv /tmp/spored-new /usr/local/bin/spored || true` is what the app paths did:
// even the install failing was discarded. A verified installer whose exit status
// is ignored is not meaningfully better than no installer.
func TestNoCallSiteSwallowsTheInstallerFailure(t *testing.T) {
	// Every discovered downloader, not a hand-written list of three: the list
	// this replaced could not have included a fourth path nobody knew about
	// (#1173). TestEverySporedDownloaderUsesTheVerifiedInstaller fails a
	// downloader that does not call the installer; this checks how it calls it.
	downloaders := discoverSporedDownloaders(t)
	checked := 0

	for rel, body := range downloaders {
		if !installerCall.MatchString(body) {
			continue
		}
		checked++

		// Any line invoking the installer must not end in a failure-swallowing
		// suffix, and must be reached through a conditional that inspects it.
		for _, line := range strings.Split(body, "\n") {
			if !strings.Contains(line, sporedinstall.FuncName+" ") && !strings.Contains(line, sporedinstall.BucketInstallFuncName+" ") {
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

	if checked < 3 {
		t.Fatalf("checked %d call sites, fewer than the three known ones (cmd/app.go, cmd/upgrade_spored.go, "+
			"pkg/launcher/bootstrap.go); the gate is looking at fewer files than it claims", checked)
	}
}

// The generated app user-data must carry both the installer and a call to it.
//
// The sentinel splice in app.go returns the script unchanged if the sentinel is
// missing, which would be a silent downgrade — so assert the OUTPUT, not the
// splice helper.
func TestGeneratedAppUserDataInstallsSporedVerified(t *testing.T) {
	pinCallerVersion(t, "9.9.9")
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
				sporedinstall.FuncName + "()",                   // the definition
				sporedinstall.BucketInstallFuncName + " \"spor", // a call
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
			assertPinnedBeforeInstall(t, script, "9.9.9")
		})
	}
}

// pinCallerVersion makes this spawn a release build of version for the test.
func pinCallerVersion(t *testing.T, version string) {
	t.Helper()
	prev := launcher.CallerVersion
	t.Cleanup(func() { launcher.CallerVersion = prev })
	launcher.CallerVersion = version
}

// assertPinnedBeforeInstall checks that script assigns the spored version
// (#1412) to want, as a line of its own, before it installs spored.
func assertPinnedBeforeInstall(t *testing.T, script, want string) {
	t.Helper()
	pin := regexp.MustCompile(`(?m)^` + sporedinstall.PinVar + `='` + regexp.QuoteMeta(want) + `'$`).FindStringIndex(script)
	call := installerCall.FindStringIndex(script)
	if pin == nil {
		t.Fatalf("the script does not pin spored to %s (%s='%s'), so it installs the latest spored instead "+
			"of the one this spawn ships (#1412)", want, sporedinstall.PinVar, want)
	}
	if call == nil || pin[0] > call[0] {
		t.Errorf("the spored version is not assigned before the install runs (pin at %v, install at %v)", pin, call)
	}
}

// A dev build of spawn has no published spored of its own, and installs the
// latest (#1412).
func TestAppUserDataFromADevBuildInstallsTheLatestSpored(t *testing.T) {
	pinCallerVersion(t, "dev")
	raw, err := base64.StdEncoding.DecodeString(buildDCVUserDataWithInit("echo hi", "sess-1", "", "test"))
	if err != nil {
		t.Fatal(err)
	}
	assertPinnedBeforeInstall(t, string(raw), "")
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
	if !strings.Contains(s, sporedinstall.BucketInstallFuncName+" \"$BIN\"") {
		t.Error("the upgrade script does not call the verified installer")
	}
	// The target, and only the target: the upgrade installs
	// spawn/versions/<target>/, never the latest (#1412).
	assertPinnedBeforeInstall(t, s, "0.128.0")
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
	iInstall := strings.Index(s, sporedinstall.BucketInstallFuncName+" \"$BIN\"")
	if iBackup < 0 || iInstall < 0 || iBackup > iInstall {
		t.Error("the rollback backup is not taken before the install, so a failed health check has " +
			"nothing to roll back to")
	}
}

// windowsInstallCall matches a call of the Windows installer generator, and
// windowsPinnedCall one that passes it this spawn's spored version (#1412).
var (
	windowsInstallCall = regexp.MustCompile(`\bsporedinstall\.WindowsInstallScript(For)?\(`)
	windowsPinnedCall  = regexp.MustCompile(`\bsporedinstall\.WindowsInstallScript(For)?\([^,()]+,\s*launcher\.SporedVersion\(\)(,\s*[A-Za-z_]+)?\)`)
)

// verifiedInstallCall matches a call of the Linux installer generator.
var verifiedInstallCall = regexp.MustCompile(`\bsporedinstall\.VerifiedInstallFunc\(\)`)

// Every file outside pkg/sporedinstall that emits the Linux installer also
// emits the spored version pin (#1412). Discovered by the generator call, not by
// the artifact name, so an install path that names no spored-linux-* binary
// itself is covered too.
func TestEveryVerifiedInstallCallerIsPinned(t *testing.T) {
	found := 0
	walkGoSource(t, func(rel string, b []byte) {
		if !verifiedInstallCall.Match(b) {
			return
		}
		found++
		if !pinCall.Match(b) {
			t.Errorf("%s emits sporedinstall.VerifiedInstallFunc() but never sporedinstall.PinScript(), so "+
				"what it installs is the latest spored, not its own spawn's (#1412)", rel)
		}
	})
	// pkg/launcher, cmd/app.go, cmd/upgrade_spored.go, the pipeline orchestrator.
	if found < 4 {
		t.Fatalf("found %d files emitting the installer, fewer than the four known; discovery has broken", found)
	}
}

// walkGoSource calls fn for every non-test .go file in the repository outside
// pkg/sporedinstall, nested modules included.
func walkGoSource(t *testing.T, fn func(rel string, b []byte)) {
	t.Helper()
	root := repoRootFromCmd(t)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if treewalk.Vanished(root, path, err) {
				return nil
			}
			return err
		}
		if info.IsDir() {
			if path != root && skipDirs[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") ||
			filepath.Dir(path) == filepath.Join(root, "pkg", "sporedinstall") {
			return nil
		}
		b, err := os.ReadFile(path) //nolint:gosec // walking the repo's own tree
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		fn(rel, b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Every Windows install path outside pkg/sporedinstall installs the spored of
// the spawn rendering it (#1412). Discovered from the repository's source, so a
// new Windows bootstrap is covered without editing this.
func TestEveryWindowsInstallPathIsPinned(t *testing.T) {
	found := 0
	walkGoSource(t, func(rel string, b []byte) {
		calls := len(windowsInstallCall.FindAllIndex(b, -1))
		pinned := len(windowsPinnedCall.FindAllIndex(b, -1))
		found += calls
		if calls != pinned {
			t.Errorf("%s calls sporedinstall.WindowsInstallScript %d times but passes launcher.SporedVersion() "+
				"in only %d, so a Windows launch installs a spored other than its own spawn's (#1412)", rel, calls, pinned)
		}
	})
	if found == 0 {
		t.Fatal("found no call of sporedinstall.WindowsInstallScript; discovery has broken, it has not proven the repo clean")
	}
}
