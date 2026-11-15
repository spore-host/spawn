//go:build !windows

package cmd

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// #1598: Amazon DCV installs only once its signing key imports and every RPM's
// signature verifies; otherwise nothing is installed. Runs dcvInstallBlock in
// bash with rpm, curl, tar, dnf, systemctl and uname stood in for.
func TestDCVInstallsOnlyVerifiedPackages(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	for name, tc := range map[string]struct {
		importOK bool
		checks   string // rpm -K output
		checkRC  int
		install  bool
	}{
		"verified":       {true, "/x/nice-dcv-server.rpm: digests signatures OK\n/x/nice-xdcv.rpm: digests signatures OK\n", 0, true},
		"key not import": {false, "/x/a.rpm: digests signatures OK\n", 0, false},
		"bad signature":  {true, "/x/a.rpm: digests signatures OK\n/x/b.rpm: digests SIGNATURES NOT OK\n", 1, false},
		"no packages":    {true, "", 1, false},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			if err := os.MkdirAll(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			rc := "1"
			if tc.importOK {
				rc = "0"
			}
			stubs := map[string]string{
				"rpm": "#!/bin/bash\nif [ \"$1\" = --import ]; then exit " + rc + "; fi\nif [ \"$1\" = -K ]; then printf '%s' '" +
					tc.checks + "'; exit " + strconv.Itoa(tc.checkRC) + "; fi\nexit 0\n",
				"curl":      "#!/bin/bash\nwhile [ $# -gt 0 ]; do [ \"$1\" = -o ] && : > \"$2\"; shift; done\nexit 0\n",
				"tar":       "#!/bin/bash\nexit 0\n",
				"dnf":       "#!/bin/bash\necho \"$*\" >> \"" + dir + "/dnf-calls\"\nexit 0\n",
				"systemctl": "#!/bin/bash\nexit 0\n",
				"uname":     "#!/bin/bash\necho x86_64\n",
			}
			for n, body := range stubs {
				if err := os.WriteFile(filepath.Join(bin, n), []byte(body), 0o755); err != nil { //nolint:gosec // test stand-ins must be executable
					t.Fatal(err)
				}
			}
			// The unpinned path, explicitly: the stubs cannot produce the
			// real key's fingerprints or the real tarball's sha256, and the
			// pinned path has its own test below (#1620).
			script := strings.ReplaceAll(dcvInstallBlockWith(dcvPins{}), "/tmp/", dir+"/")
			// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
			cmd := exec.Command("bash", "-c", script) //nolint:gosec // this package's own generated script
			cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + dir}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			calls, _ := os.ReadFile(filepath.Join(dir, "dnf-calls"))
			installed := strings.Contains(string(calls), "nice-dcv-server")
			if installed != tc.install {
				t.Fatalf("DCV installed=%t, want %t\n--- output ---\n%s\n--- dnf ---\n%s", installed, tc.install, out, calls)
			}
			if installed && !strings.Contains(string(calls), "--setopt=localpkg_gpgcheck=1") {
				t.Errorf("dnf installed the local DCV packages without checking their signatures: %s", calls)
			}
		})
	}
}

// #1620: once pinned, DCV installs only when the downloaded key's primary
// fingerprints and this machine's tarball sha256 are the pinned ones, on top
// of the #1598 signature checks. Runs dcvInstallBlockWith in bash with test
// pins and stand-ins; sha256sum is the real one (or shasum underneath it).
func TestDCVInstallsOnlyThePinnedTarballAndKey(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	// The real hash, wherever it lives: sha256sum, or shasum -a 256 (macOS).
	sumStub := ""
	if p, err := exec.LookPath("sha256sum"); err == nil {
		sumStub = "#!/bin/bash\nexec '" + p + "' \"$@\"\n"
	} else if p, err := exec.LookPath("shasum"); err == nil {
		sumStub = "#!/bin/bash\nexec '" + p + "' -a 256 \"$@\"\n"
	} else {
		t.Skip("neither sha256sum nor shasum")
	}
	const tgzBytes = "pretend DCV tarball"
	tgzSHA := fmt.Sprintf("%x", sha256.Sum256([]byte(tgzBytes)))
	const fprA, fprB = "1111111111111111111111111111111111111111", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	good := dcvPins{version: dcvVersion, sha256x8664: tgzSHA, sha256aarch64: strings.Repeat("0", 64), keyFingerprints: fprA + " " + fprB}
	for name, tc := range map[string]struct {
		pins    dcvPins
		keyFprs []string // what the downloaded key holds, in file order
		machine string
		install bool
	}{
		"pinned key and tarball":          {good, []string{fprB, fprA}, "x86_64", true},
		"another key":                     {good, []string{fprA, "2222222222222222222222222222222222222222"}, "x86_64", false},
		"an extra key in the file":        {good, []string{fprA, fprB, "2222222222222222222222222222222222222222"}, "x86_64", false},
		"a different tarball":             {good, []string{fprA, fprB}, "aarch64", false},
		"an architecture with no pin":     {good, []string{fprA, fprB}, "riscv64", false},
		"unpinned: signature checks only": {dcvPins{}, []string{"2222222222222222222222222222222222222222"}, "x86_64", true},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			if err := os.MkdirAll(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			var colons strings.Builder
			for _, f := range tc.keyFprs {
				colons.WriteString("pub:-:255:22:0000000000000000:1:::-:::scESC::::::23::0:\nfpr:::::::::" + f + ":\n")
				colons.WriteString("sub:-:255:18:0000000000000001:1::::::e::::::23:\nfpr:::::::::BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB:\n")
			}
			stubs := map[string]string{
				"rpm": "#!/bin/bash\nif [ \"$1\" = -K ]; then printf '/x/nice-dcv-server.rpm: digests signatures OK\\n'; fi\nexit 0\n",
				// The tarball gets tgzBytes; the key file anything.
				"curl":      "#!/bin/bash\nurl=\nwhile [ $# -gt 0 ]; do case $1 in -o) out=$2; shift ;; http*) url=$1 ;; esac; shift; done\ncase $url in *.tgz) printf '%s' '" + tgzBytes + "' > \"$out\" ;; *) echo key > \"$out\" ;; esac\nexit 0\n",
				"gpg":       "#!/bin/bash\nprintf '%s' '" + colons.String() + "'\n",
				"tar":       "#!/bin/bash\nexit 0\n",
				"dnf":       "#!/bin/bash\necho \"$*\" >> \"" + dir + "/dnf-calls\"\nexit 0\n",
				"systemctl": "#!/bin/bash\nexit 0\n",
				"uname":     "#!/bin/bash\necho " + tc.machine + "\n",
			}
			stubs["sha256sum"] = sumStub
			for n, body := range stubs {
				if err := os.WriteFile(filepath.Join(bin, n), []byte(body), 0o755); err != nil { //nolint:gosec // test stand-ins must be executable
					t.Fatal(err)
				}
			}
			script := strings.ReplaceAll(dcvInstallBlockWith(tc.pins), "/tmp/", dir+"/")
			// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
			cmd := exec.Command("bash", "-c", script) //nolint:gosec // this package's own generated script
			cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + dir}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			calls, _ := os.ReadFile(filepath.Join(dir, "dnf-calls"))
			if installed := strings.Contains(string(calls), "nice-dcv-server"); installed != tc.install {
				t.Fatalf("DCV installed=%t, want %t\n--- output ---\n%s", installed, tc.install, out)
			}
		})
	}
}
