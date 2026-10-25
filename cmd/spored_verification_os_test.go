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
//
// The decision moved out of launch_single.go into runPostLaunchChecks (#1296), so
// the gate follows the value across the hop: launch_single must hand
// config.TargetOS to runPostLaunchChecks and resolvePostLaunchPlan, and
// runPostLaunchChecks must hand its own targetOS parameter, untouched, to
// sporedVerificationFor.
func TestLaunchDecidesSporedVerificationByTargetOS(t *testing.T) {
	// targetArg is where each hop takes the instance's OS, and what it must be.
	hops := map[string]struct {
		arg  int
		want string
	}{
		"runPostLaunchChecks":   {5, "TargetOS"},
		"resolvePostLaunchPlan": {0, "TargetOS"},
		"sporedVerificationFor": {1, "targetOS"},
	}
	seen := map[string]bool{}
	var verifies int
	for _, file := range []string{"launch_single.go", "launch_postchecks.go"} {
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
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
				t.Errorf("%s calls verifySporedReady, which always runs the LINUX probe; "+
					"it must call verifySporedReadyOn with the check chosen for the instance's OS", file)
			case "verifySporedReadyOn":
				verifies++
			}
			hop, ok := hops[id.Name]
			if !ok || len(call.Args) <= hop.arg {
				return true
			}
			arg := call.Args[hop.arg]
			switch a := arg.(type) {
			case *ast.SelectorExpr:
				if a.Sel.Name == hop.want {
					if x, ok := a.X.(*ast.Ident); ok && x.Name == "config" {
						seen[id.Name] = true
					}
				}
			case *ast.Ident:
				if a.Name == hop.want {
					seen[id.Name] = true
				}
			}
			ast.Inspect(arg, func(m ast.Node) bool {
				if s, ok := m.(*ast.SelectorExpr); ok {
					if x, ok := s.X.(*ast.Ident); ok && x.Name == "plat" {
						t.Errorf("%s is passed plat.%s — the WORKSTATION's OS. "+
							"The instance's OS is config.TargetOS (#882)", id.Name, s.Sel.Name)
					}
				}
				return true
			})
			return true
		})
	}
	if verifies == 0 {
		t.Fatal("the launch no longer verifies spored at all")
	}
	for name, hop := range hops {
		if !seen[name] {
			t.Errorf("%s is not passed %s (argument %d), the instance's OS", name, hop.want, hop.arg)
		}
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
