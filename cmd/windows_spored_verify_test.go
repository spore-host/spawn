package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The Windows bootstrap must install spored only after verifying it (#879,
// GHSA-q59f-6gc3-qj2p).
//
// The verifier itself is EXECUTED in pkg/sporedinstall's tests (pwsh against
// signed fixtures and the real release artifact). What can only be checked here is
// the WIRING: that the user-data actually splices the verifier in, and that the
// service install is reachable only through it. A bootstrap that defined the
// verifier and then installed unconditionally would pass every test over there.
//
// Matched on the rendered user-data, not the Go source, so the check is about what
// an instance actually runs (rule 3).
func TestWindowsBootstrapInstallsSporedOnlyAfterVerifying(t *testing.T) {
	ud, err := buildWindowsUserData("ssh-ed25519 AAAATESTKEY test")
	if err != nil {
		t.Fatal(err)
	}

	verify := strings.Index(ud, `Test-SporeBinary $cand`)
	gate := strings.Index(ud, `if ($sporedInstalled) {`)
	install := strings.Index(ud, `service install`)
	switch {
	case verify < 0:
		t.Fatal("the Windows user-data never calls Test-SporeBinary — spored would be " +
			"installed as a SYSTEM service unverified")
	case install < 0:
		t.Fatal("the Windows user-data no longer installs spored at all; every Windows spore " +
			"would boot with no lifecycle")
	case gate < 0 || gate > install:
		t.Fatal("`service install` is not inside `if ($sporedInstalled)`, so a failed " +
			"verification would still install whatever was downloaded")
	case verify > gate:
		t.Fatal("verification happens after the install gate")
	}
	if strings.Count(ud, "service install") != 1 {
		t.Errorf("found %d `service install` calls; only the gated one may exist",
			strings.Count(ud, "service install"))
	}

	// The old unverified loop wrote straight to the final path and installed from it.
	if strings.Contains(ud, `$aws s3 cp $uri $exe`) {
		t.Error("the user-data still copies S3 straight onto the installed path — the " +
			"unverified install this replaced")
	}
	// No template residue: a leftover placeholder means the verifier was not spliced.
	for _, p := range []string{"__SPORED_WINDOWS_VERIFIED_INSTALL__", "__KEY_X__", "__SIG_VERIFY__", "%!"} {
		if strings.Contains(ud, p) {
			t.Errorf("rendered user-data contains %q", p)
		}
	}
	// EC2 caps user-data at 16 KB. The verifier roughly doubled the script; say so
	// here rather than at RunInstances.
	if len(ud) > 16*1024 {
		t.Errorf("Windows user-data is %d bytes, over EC2's 16 KB limit", len(ud))
	}
}

// The rendered Windows bootstrap must PARSE as PowerShell.
//
// A syntax error in <powershell> user-data does not fail RunInstances or surface
// anywhere in spawn — EC2Launch logs it on the instance and moves on — so every
// Windows spore would boot with no spored and nobody would be told. Parsed with
// PowerShell's own parser (nothing is executed). pwsh is on GitHub's ubuntu-latest
// runners, so in CI its absence is a failure, not a skip.
func TestWindowsBootstrapParsesAsPowerShell(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("pwsh is not available in CI; the Windows bootstrap cannot be parse-checked")
		}
		t.Skip("pwsh not installed")
	}
	ud, err := buildWindowsUserData("ssh-ed25519 AAAATESTKEY test")
	if err != nil {
		t.Fatal(err)
	}
	a := strings.Index(ud, "<powershell>")
	b := strings.Index(ud, "</powershell>")
	if a < 0 || b < a {
		t.Fatal("no <powershell> block in the Windows user-data")
	}
	f := filepath.Join(t.TempDir(), "bootstrap.ps1")
	if err := os.WriteFile(f, []byte(ud[a+len("<powershell>"):b]), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-Command",
		`$e=$null; [System.Management.Automation.Language.Parser]::ParseFile('`+f+`', [ref]$null, [ref]$e) | Out-Null; `+
			`if ($e.Count) { $e | ForEach-Object { 'line ' + $_.Extent.StartLineNumber + ': ' + $_.Message }; exit 1 }`).CombinedOutput()
	if err != nil {
		t.Fatalf("the Windows bootstrap does not parse as PowerShell — every Windows spore would boot "+
			"without spored, silently:\n%s", out)
	}
}
