package cmd

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/sporedinstall"
)

// The installer pins (#1620) are written by .github/workflows/installer-pins.yml
// and are empty until its first run. Whatever they hold, each group is all set
// or all empty, every value has its form, and the DCV hashes are for the DCV
// version the boot installs: bumping dcvVersion without re-pinning would make
// every DCV launch refuse its own tarball, so it fails here instead.
func TestInstallerPinsAreWellFormed(t *testing.T) {
	sha := regexp.MustCompile(`^[0-9a-f]{64}$`)
	fprs := regexp.MustCompile(`^[0-9A-F]{40}( [0-9A-F]{40})*$`)
	cliVersion := regexp.MustCompile(`^2\.\d+\.\d+$`)
	ssmVersion := regexp.MustCompile(`^\d+\.\d+\.\d+\.\d+$`)

	for name, c := range map[string]struct {
		pin     sporedinstall.WindowsInstallerPin
		version *regexp.Regexp
	}{
		"AWS CLI MSI": {sporedinstall.WindowsAWSCLIMSIPin(), cliVersion},
		"SSM agent":   {sporedinstall.WindowsSSMAgentPin(), ssmVersion},
	} {
		if (c.pin.Version == "") != (c.pin.SHA256 == "") {
			t.Errorf("%s pin: version %q and sha256 %q must be set or empty together", name, c.pin.Version, c.pin.SHA256)
			continue
		}
		if !c.pin.Set() {
			t.Logf("%s: not pinned yet (installer-pins.yml has not run)", name)
			continue
		}
		if !c.version.MatchString(c.pin.Version) || !sha.MatchString(c.pin.SHA256) {
			t.Errorf("%s pin %+v is malformed", name, c.pin)
		}
	}

	p := currentDCVPins()
	empty := p.version == "" && p.sha256x8664 == "" && p.sha256aarch64 == "" && p.keyFingerprints == ""
	switch {
	case empty:
		t.Log("DCV: not pinned yet (installer-pins.yml has not run)")
	case !p.set():
		t.Errorf("DCV pins %+v: all four must be set or empty together", p)
	default:
		if p.version != dcvVersion {
			t.Errorf("the DCV pins are for %s but the boot installs dcvVersion %s: run installer-pins.yml to re-pin", p.version, dcvVersion)
		}
		if !sha.MatchString(p.sha256x8664) || !sha.MatchString(p.sha256aarch64) {
			t.Errorf("DCV tarball sha256s %q / %q are malformed", p.sha256x8664, p.sha256aarch64)
		}
		if f := strings.Fields(p.keyFingerprints); !sort.StringsAreSorted(f) {
			t.Errorf("DCV key fingerprints %q are not sorted: the boot compares the sorted list", p.keyFingerprints)
		}
		if !fprs.MatchString(p.keyFingerprints) {
			t.Errorf("DCV key fingerprints %q: want sorted, space-separated 40-hex uppercase", p.keyFingerprints)
		}
	}
}

// The pin scripts verify with the bootstrap's own checks, so what they pin is
// what the instance accepts: the Windows one carries
// sporedinstall.WindowsAmazonSignedFunc verbatim, and the DCV one computes the
// key's fingerprints with the same pipeline as the boot.
func TestInstallerPinScriptsAgreeWithTheBootstrap(t *testing.T) {
	ps, err := os.ReadFile("../scripts/installer-pins-windows.ps1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ReplaceAll(string(ps), "\r", ""), strings.TrimSpace(sporedinstall.WindowsAmazonSignedFunc)) {
		t.Error("scripts/installer-pins-windows.ps1 does not carry sporedinstall.WindowsAmazonSignedFunc verbatim; copy it again")
	}
	sh, err := os.ReadFile("../scripts/installer-pins-dcv.sh")
	if err != nil {
		t.Fatal(err)
	}
	pipe := regexp.MustCompile(`gpg --batch --show-keys --with-colons \S+ 2>/dev/null \| (awk .*? \| sort \| paste -sd ' ' -)`)
	boot := pipe.FindStringSubmatch(dcvInstallBlockWith(dcvPins{version: "v", sha256x8664: "x", sha256aarch64: "y", keyFingerprints: "z"}))
	script := pipe.FindStringSubmatch(string(sh))
	if boot == nil || script == nil {
		t.Fatalf("no fingerprint pipeline found: boot %v, script %v", boot != nil, script != nil)
	}
	if boot[1] != script[1] {
		t.Errorf("the DCV key fingerprints are computed differently:\n boot:   %s\n script: %s", boot[1], script[1])
	}
}
