package cmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The launch's spored check follows the INSTANCE's OS (#882).
func TestSporedVerificationFollowsTheTargetOS(t *testing.T) {
	cases := []struct {
		wait     bool
		targetOS string
		want     sporedCheck
	}{
		{true, "windows", sporedCheckWindows},
		{true, "Windows", sporedCheckWindows},
		{true, "linux", sporedCheckLinux},
		{true, "", sporedCheckLinux}, // unresolved: the Linux default, never skipped
		{false, "windows", sporedCheckNone},
		{false, "linux", sporedCheckNone},
	}
	for _, c := range cases {
		if got := sporedVerificationFor(c.wait, c.targetOS); got != c.want {
			t.Errorf("sporedVerificationFor(wait=%v, %q) = %v, want %v", c.wait, c.targetOS, got, c.want)
		}
	}
}

// A Windows check must use a PowerShell document; a Linux one must not. Sending
// AWS-RunShellScript to Windows is exactly what produced UnsupportedPlatformType
// and a false "spored never came up".
func TestSporedProbeMatchesTheCheck(t *testing.T) {
	if ps, cmd := sporedProbe(sporedCheckWindows); !ps || !strings.Contains(cmd, "Get-Service spored") {
		t.Errorf("Windows probe = (powershell=%v, %q), want a PowerShell service check", ps, cmd)
	}
	if ps, cmd := sporedProbe(sporedCheckLinux); ps || cmd != "/usr/local/bin/spored status" {
		t.Errorf("Linux probe = (powershell=%v, %q), want shell `spored status`", ps, cmd)
	}
	if sporedVerifyTimeout(sporedCheckWindows) <= sporedVerifyTimeout(sporedCheckLinux) {
		t.Error("Windows gets no more time than Linux, though its first boot also installs the AWS CLI")
	}
}

// THE regression gate: the launch's verification decision must be taken from the
// instance's OS, and the workstation's OS must not take part in it.
//
// Structural, on the AST of the call site, because the original defect was
// exactly one wrong value in one condition — `plat.OS` (runtime.GOOS) where
// `config.TargetOS` belonged — and a behavioural test of sporedVerificationFor
// cannot see which value the CALLER passes. Matched as a call expression, so a
// comment naming plat.OS (there is one, explaining this) cannot trip it (rule 3).
func TestLaunchDecidesSporedVerificationByTargetOS(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "launch_single.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var decisions, verifies int
	var passesTarget bool
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok {
			return true
		}
		switch id.Name {
		case "verifySporedReady":
			t.Error("launch_single calls verifySporedReady, which always runs the LINUX probe; " +
				"it must call verifySporedReadyOn with the check chosen for the instance's OS")
		case "verifySporedReadyOn":
			verifies++
		case "sporedVerificationFor":
			decisions++
			if len(call.Args) == 2 {
				if sel, ok := call.Args[1].(*ast.SelectorExpr); ok && sel.Sel.Name == "TargetOS" {
					passesTarget = true
				}
				ast.Inspect(call.Args[1], func(m ast.Node) bool {
					if s, ok := m.(*ast.SelectorExpr); ok {
						if x, ok := s.X.(*ast.Ident); ok && x.Name == "plat" {
							t.Errorf("sporedVerificationFor is passed plat.%s — the WORKSTATION's OS. "+
								"The instance's OS is config.TargetOS (#882)", s.Sel.Name)
						}
					}
					return true
				})
			}
		}
		return true
	})
	if verifies == 0 {
		t.Fatal("launch_single no longer verifies spored at all")
	}
	if decisions == 0 {
		t.Fatal("launch_single verifies spored without sporedVerificationFor deciding how")
	}
	if !passesTarget {
		t.Error("sporedVerificationFor is not passed config.TargetOS, the instance's OS")
	}
}

// The Windows probe must parse as PowerShell; it is sent to the instance verbatim.
func TestWindowsSporedProbeParses(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("pwsh is not available in CI; the Windows spored probe cannot be parse-checked")
		}
		t.Skip("pwsh not installed")
	}
	_, cmd := sporedProbe(sporedCheckWindows)
	p := filepath.Join(t.TempDir(), "probe.ps1")
	if err := os.WriteFile(p, []byte(cmd), 0o600); err != nil {
		t.Fatal(err)
	}
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	out, err := exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-Command", //nolint:gosec // parses this package's own constant
		`$e=$null; [System.Management.Automation.Language.Parser]::ParseFile('`+p+`', [ref]$null, [ref]$e) | Out-Null; if ($e.Count) { $e | ForEach-Object { $_.Message }; exit 1 }`).CombinedOutput()
	if err != nil {
		t.Fatalf("the Windows spored probe does not parse:\n%s", out)
	}
}
