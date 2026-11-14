package sporedinstall

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// #1598: every downloaded installer the Windows scripts run passes the Amazon
// signature check first. Discovered from the generated script: each
// `Start-Process` of a downloaded $msi, and each msiexec of it, must follow an
// Assert-SpawnAmazonSigned of the same variable since its download.
func TestWindowsStandaloneRunsOnlyAmazonSignedInstallers(t *testing.T) {
	script, err := WindowsStandaloneScript(SigningKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	AssertDownloadsAreAmazonSigned(t, script)
}

// AssertDownloadsAreAmazonSigned fails t unless, in script, every
// Invoke-WebRequest -OutFile $v is followed by Assert-SpawnAmazonSigned $v
// before $v is run (Start-Process $v, or msiexec with $v).
func AssertDownloadsAreAmazonSigned(t *testing.T, script string) {
	t.Helper()
	download := regexp.MustCompile(`Invoke-WebRequest\b[^\n]*-OutFile (\$\w+)`)
	found := 0
	for _, m := range download.FindAllStringSubmatchIndex(script, -1) {
		v := script[m[2]:m[3]]
		rest := script[m[1]:]
		run := regexp.MustCompile(`Start-Process (` + regexp.QuoteMeta(v) + `\b|msiexec\.exe[^\n]*` + regexp.QuoteMeta(v) + `\b)`).FindStringIndex(rest)
		if run == nil {
			continue // downloaded and not run (spored: verified by its own signature)
		}
		found++
		check := strings.Index(rest, WindowsAmazonSignedFuncName+" "+v)
		if check < 0 || check > run[0] {
			t.Errorf("%s is downloaded and run without %s %s first (#1598)", v, WindowsAmazonSignedFuncName, v)
		}
	}
	if found == 0 {
		t.Fatal("found no downloaded installer that is run: the discovery is broken")
	}
	if !strings.Contains(script, "function "+WindowsAmazonSignedFuncName) {
		t.Errorf("the script calls %s without defining it", WindowsAmazonSignedFuncName)
	}
}

// The check itself, run in Windows PowerShell on the Windows CI job: an
// unsigned file and a Microsoft-signed one are refused, and the runner's own
// Amazon-signed AWS CLI passes.
func TestAssertSpawnAmazonSignedOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Get-AuthenticodeSignature is Windows-only")
	}
	ps, err := exec.LookPath("powershell")
	if err != nil {
		t.Skip("no Windows PowerShell")
	}
	unsigned := filepath.Join(t.TempDir(), "unsigned.exe")
	if err := os.WriteFile(unsigned, []byte("MZ not signed"), 0o600); err != nil {
		t.Fatal(err)
	}
	try := func(path string) error {
		f := filepath.Join(t.TempDir(), "check.ps1")
		body := "$ErrorActionPreference = 'Stop'\n" + WindowsAmazonSignedFunc + "\nAssert-SpawnAmazonSigned '" + path + "'\n"
		if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return exec.Command(ps, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", f).Run()
	}
	if try(unsigned) == nil {
		t.Error("an unsigned file passed the Amazon signature check")
	}
	if ms := filepath.Join(os.Getenv("SystemRoot"), "System32", "notepad.exe"); fileExists(ms) && try(ms) == nil {
		t.Errorf("%s, signed by Microsoft, passed the Amazon signature check", ms)
	}
	if aws := filepath.Join(os.Getenv("ProgramFiles"), "Amazon", "AWSCLIV2", "aws.exe"); fileExists(aws) {
		if err := try(aws); err != nil {
			t.Errorf("the runner's Amazon-signed %s failed the check: %v", aws, err)
		}
	} else {
		t.Logf("no AWS CLI at %s to check the accepting half", aws)
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func TestWindowsAWSCLIMSIURL(t *testing.T) {
	if got := WindowsAWSCLIMSIURL("2.39.3"); got != "https://awscli.amazonaws.com/AWSCLIV2-2.39.3.msi" {
		t.Errorf("versioned MSI URL %q", got)
	}
	if got := WindowsAWSCLIMSIURL(""); got != "https://awscli.amazonaws.com/AWSCLIV2.msi" {
		t.Errorf("latest MSI URL %q", got)
	}
}
