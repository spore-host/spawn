package sporedinstall

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

var testPin = WindowsInstallerPin{Version: "2.40.1", SHA256: strings.Repeat("ab", 32)}

// #1620: with a pin, each download is checked for Amazon's signature AND the
// pinned sha256 before anything runs, and nothing falls back to an
// unversioned latest. Tested with a stand-in pin, since the committed ones
// stay empty until installer-pins.yml runs.
func TestPinnedWindowsDownloadsCheckTheSHA256(t *testing.T) {
	ssm := WindowsInstallerPin{Version: "3.3.2299.0", SHA256: strings.Repeat("cd", 32)}
	for name, c := range map[string]struct {
		script, pin string
	}{
		"AWS CLI MSI": {WindowsAWSCLIMSIDownload(testPin, "2.39.3"), testPin.SHA256},
		"SSM agent":   {WindowsSSMAgentDownload(ssm), ssm.SHA256},
	} {
		dl := strings.Index(c.script, "-OutFile $msi")
		signed := strings.Index(c.script, WindowsAmazonSignedFuncName+" $msi")
		sum := strings.Index(c.script, WindowsSHA256FuncName+" $msi '"+c.pin+"'")
		if dl < 0 || signed < dl || sum < dl {
			t.Errorf("%s: want a download to $msi, then %s and %s with the pin:\n%s", name, WindowsAmazonSignedFuncName, WindowsSHA256FuncName, c.script)
		}
		if strings.Contains(c.script, "/latest/") || strings.Contains(c.script, "AWSCLIV2.msi") {
			t.Errorf("%s: a pinned download must not fall back to the unversioned latest:\n%s", name, c.script)
		}
	}
	if s := WindowsAWSCLIMSIDownload(testPin, "2.39.3"); !strings.Contains(s, WindowsAWSCLIMSIURL(testPin.Version)) || strings.Contains(s, "2.39.3") {
		t.Errorf("the MSI pin must choose the release, not the Linux pin:\n%s", s)
	}
	for _, u := range WindowsSSMAgentSetupURLs(ssm.Version) {
		if !strings.Contains(WindowsSSMAgentDownload(ssm), u) {
			t.Errorf("the SSM agent download does not try %s", u)
		}
	}
	// Unpinned: Authenticode only, as before #1620.
	for name, s := range map[string]string{
		"AWS CLI MSI": WindowsAWSCLIMSIDownload(WindowsInstallerPin{}, "2.39.3"),
		"SSM agent":   WindowsSSMAgentDownload(WindowsInstallerPin{}),
	} {
		if strings.Contains(s, WindowsSHA256FuncName) || !strings.Contains(s, WindowsAmazonSignedFuncName+" $msi") {
			t.Errorf("%s unpinned:\n%s", name, s)
		}
	}
}

// Every script that splices a download in defines both checks it calls.
func TestWindowsScriptsDefineTheSHA256Check(t *testing.T) {
	s, err := WindowsStandaloneScript(SigningKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s, "function "+WindowsSHA256FuncName) || !strings.Contains(s, "function "+WindowsAmazonSignedFuncName) {
		t.Error("the standalone installer does not define both installer checks")
	}
	if !regexp.MustCompile(`^[0-9A-Za-z.-]*$`).MatchString(testPin.Version + testPin.SHA256) {
		t.Fatal("the test pin is not a plain literal")
	}
	defer func() {
		if recover() == nil {
			t.Error("a pin holding a quote was spliced into PowerShell")
		}
	}()
	WindowsAWSCLIMSIDownload(WindowsInstallerPin{Version: "2.40.1'; evil", SHA256: testPin.SHA256}, "")
}

// The check itself, in PowerShell where there is one (the Windows CI job has
// Windows PowerShell; pwsh runs it elsewhere): the right hash passes, any
// other is refused.
func TestAssertSpawnSHA256(t *testing.T) {
	ps := ""
	for _, c := range []string{"powershell", "pwsh"} {
		if p, err := exec.LookPath(c); err == nil {
			ps = p
			break
		}
	}
	if ps == "" {
		t.Skip("no PowerShell")
	}
	if runtime.GOOS != "windows" && filepath.Base(ps) == "powershell" {
		t.Skip("not Windows PowerShell")
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "installer.exe")
	body := []byte("pretend installer")
	if err := os.WriteFile(f, body, 0o600); err != nil {
		t.Fatal(err)
	}
	try := func(want string) error {
		script := filepath.Join(dir, "check.ps1")
		if err := os.WriteFile(script, []byte("$ErrorActionPreference = 'Stop'\n"+WindowsSHA256Func+"\nAssert-SpawnSHA256 '"+f+"' '"+want+"'\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
		return exec.Command(ps, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script).Run() //nolint:gosec // this package's own check
	}
	good := fmt.Sprintf("%x", sha256.Sum256(body))
	if err := try(good); err != nil {
		t.Errorf("the right sha256 was refused: %v", err)
	}
	if try(strings.Repeat("0", 64)) == nil {
		t.Error("a wrong sha256 passed")
	}
}
