package cmd

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
)

// fakeSSMRun records RunShellScript calls and returns a canned result.
type fakeSSMRun struct {
	calls   int
	scripts []string
	res     *aws.SSMRunResult
	err     error
}

func (f *fakeSSMRun) run(_ context.Context, _, _, script string, _ time.Duration) (*aws.SSMRunResult, error) {
	f.calls++
	f.scripts = append(f.scripts, script)
	return f.res, f.err
}

var oneShotInstance = &aws.InstanceInfo{InstanceID: "i-0abc", Region: "us-east-1"}

// #1306: on the SSM path a one-shot command runs over RunCommand and no
// interactive session opens; without a command, the session opens as before.
func TestConnectOverSSMRunsTheOneShotCommand(t *testing.T) {
	f := &fakeSSMRun{res: &aws.SSMRunResult{Status: "Success"}}
	sessions := 0
	open := func(string, string) error { sessions++; return nil }

	if err := connectOverSSM(context.Background(), f.run, open, oneShotInstance, "alice", "uptime"); err != nil {
		t.Fatalf("one-shot: %v", err)
	}
	if f.calls != 1 || sessions != 0 {
		t.Fatalf("one-shot: %d RunCommand call(s), %d session(s); want 1 and 0 — "+
			"an interactive session drops the command and waits on stdin (#1306)", f.calls, sessions)
	}

	if err := connectOverSSM(context.Background(), f.run, open, oneShotInstance, "alice", ""); err != nil {
		t.Fatalf("interactive: %v", err)
	}
	if f.calls != 1 || sessions != 1 {
		t.Errorf("interactive: %d RunCommand call(s), %d session(s); want still 1 and 1", f.calls, sessions)
	}
}

// The remote exit status is spawn connect's exit status, stdout and stderr
// stay apart, and an invocation that never produced a status is an error, not
// a success.
func TestRunOneShotOverSSMPropagatesStatusAndStreams(t *testing.T) {
	for _, tc := range []struct {
		name     string
		res      aws.SSMRunResult
		wantCode int  // exitForError of the returned error
		wantErr  bool // any error at all
	}{
		{"success", aws.SSMRunResult{Status: "Success", Stdout: "out\n", Stderr: "err\n"}, 0, false},
		{"non-zero exit", aws.SSMRunResult{Status: "Failed", ResponseCode: 3, Stdout: "out\n", Stderr: "err\n"}, 3, true},
		{"timed out", aws.SSMRunResult{Status: "TimedOut", ResponseCode: -1}, 1, true},
		{"failed with no status", aws.SSMRunResult{Status: "Failed", ResponseCode: -1}, 1, true},
		{"cancelled", aws.SSMRunResult{Status: "Cancelled"}, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := tc.res
			f := &fakeSSMRun{res: &res}
			var out, errb bytes.Buffer
			err := runOneShotOverSSM(context.Background(), f.run, oneShotInstance, "alice", "x", false, &out, &errb)
			if (err != nil) != tc.wantErr || exitForError(err) != tc.wantCode {
				t.Fatalf("err = %v (exit %d); want error=%v, exit %d", err, exitForError(err), tc.wantErr, tc.wantCode)
			}
			if out.String() != res.Stdout {
				t.Errorf("stdout = %q, want exactly the remote stdout %q", out.String(), res.Stdout)
			}
			if !strings.Contains(errb.String(), res.Stderr) || (res.Stderr != "" && strings.Contains(out.String(), res.Stderr)) {
				t.Errorf("stderr = %q, want it to carry the remote stderr %q", errb.String(), res.Stderr)
			}
		})
	}

	f := &fakeSSMRun{err: errors.New("AccessDeniedException: ssm:SendCommand")}
	if err := runOneShotOverSSM(context.Background(), f.run, oneShotInstance, "alice", "x", false, &bytes.Buffer{}, &bytes.Buffer{}); err == nil ||
		!strings.Contains(err.Error(), "ssm:SendCommand") {
		t.Errorf("a refused SendCommand must surface its reason, got %v", err)
	}
}

// SSM truncates invocation output; a reader must be told when it may have.
func TestRunOneShotOverSSMWarnsOnTruncation(t *testing.T) {
	f := &fakeSSMRun{res: &aws.SSMRunResult{Status: "Success", Stdout: strings.Repeat("x", ssmStdoutLimit)}}
	var errb bytes.Buffer
	_ = runOneShotOverSSM(context.Background(), f.run, oneShotInstance, "alice", "x", false, &bytes.Buffer{}, &errb)
	if !strings.Contains(errb.String(), "cut short") {
		t.Errorf("no truncation warning for %d characters of stdout; stderr was %q", ssmStdoutLimit, errb.String())
	}
}

// The SSM script runs the command as the SSH login user, not root, and refuses
// a user name that could break out of the script.
func TestOneShotSSMScriptRunsAsTheLoginUser(t *testing.T) {
	s, err := oneShotSSMScript("alice", "id -un")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s, "u=alice\n") || !strings.Contains(s, `exec sudo -H -u "$u" -- `) {
		t.Errorf("script does not drop to the login user:\n%s", s)
	}
	for _, bad := range []string{"", "a b", "a;rm", "$(x)", "a'b"} {
		if _, err := oneShotSSMScript(bad, "true"); err == nil {
			t.Errorf("user %q accepted", bad)
		}
	}
}

// Run the script for real under sh, with getent and sudo shimmed, so the
// quoting and the exit-status path are checked end to end: a command full of
// shell metacharacters must arrive intact, and its status must be the
// script's status (which is what SSM reports as ResponseCode).
func TestOneShotSSMScriptRunsTheCommandIntact(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX sh")
	}
	shims := t.TempDir()
	home := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(shims, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil { //nolint:gosec // test shim must be executable
			t.Fatal(err)
		}
	}
	write("getent", `[ "$2" = alice ] || exit 2; echo "alice:x:1000:1000::`+home+`:/bin/sh"`+"\n")
	// sudo -H -u alice -- <shell> -c <cmd>: check the user, then run the rest.
	write("sudo", `[ "$1 $2 $3 $4" = "-H -u alice --" ] || { echo "bad sudo args: $*" >&2; exit 99; }; shift 4; exec "$@"`+"\n")

	cmdLine := `printf '%s|' "it's" "$HOME" "a  b"; pwd; echo err >&2; exit 7`
	script, err := oneShotSSMScript("alice", cmdLine)
	if err != nil {
		t.Fatal(err)
	}
	// nosemgrep
	c := exec.Command("/bin/sh", "-c", script) // the script under test, built by oneShotSSMScript from fixed inputs
	c.Env = append(os.Environ(), "PATH="+shims+string(os.PathListSeparator)+os.Getenv("PATH"), "HOME="+home)
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	err = c.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 7 {
		t.Fatalf("exit = %v, want 7 (stderr %q)", err, errb.String())
	}
	resolvedHome, _ := filepath.EvalSymlinks(home)
	got := strings.TrimSpace(out.String())
	if !strings.HasPrefix(got, "it's|"+home+"|a  b|") || !(strings.HasSuffix(got, home) || strings.HasSuffix(got, resolvedHome)) {
		t.Errorf("stdout = %q, want the arguments intact and the cwd to be the user's home %s", got, home)
	}
	if strings.TrimSpace(errb.String()) != "err" {
		t.Errorf("stderr = %q, want just the command's stderr", errb.String())
	}

	// No such user: 255, like ssh failing to run anything.
	script, _ = oneShotSSMScript("bob", "true")
	// nosemgrep
	c = exec.Command("/bin/sh", "-c", script)
	c.Env = append(os.Environ(), "PATH="+shims+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := c.Run(); !errors.As(err, &ee) || ee.ExitCode() != 255 {
		t.Errorf("unknown user: exit = %v, want 255", err)
	}
}

// Every Linux SSM exit of runConnect goes through connectOverSSM, so none can
// open an interactive session for a one-shot command. Discovered from the AST:
// runConnect must call connectViaSessionManager nowhere, and connectOverSSM at
// least for the transport verdict and the key-injection fallback.
func TestRunConnectSendsEverySSMPathThroughConnectOverSSM(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "connect.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string]int{}
	found := false
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "runConnect" {
			continue
		}
		found = true
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok {
					calls[id.Name]++
				}
			}
			return true
		})
	}
	if !found {
		t.Fatal("runConnect not found in connect.go")
	}
	if n := calls["connectViaSessionManager"]; n != 0 {
		t.Errorf("runConnect calls connectViaSessionManager directly %d time(s): that opens an interactive session even for `-- <cmd>` (#1306); use connectOverSSM", n)
	}
	if n := calls["connectOverSSM"]; n < 2 {
		t.Errorf("runConnect calls connectOverSSM %d time(s), want the SSM verdict and the key-injection fallback (2)", n)
	}
}

// exitForError passes a remote status through.
func TestExitForErrorPassesTheRemoteStatus(t *testing.T) {
	if got := exitForError(&remoteExitError{code: 42}); got != 42 {
		t.Errorf("exitForError(remote 42) = %d", got)
	}
	if got := exitForError(remoteExit(exec.Command("/bin/sh", "-c", "exit 5").Run())); runtime.GOOS != "windows" && got != 5 {
		t.Errorf("ssh exit 5 maps to %d, want 5", got)
	}
}

// The SSH path passes the remote status on too, so a script sees the same exit
// code whichever transport connect picked: every Run() of the ssh command in
// sshToInstance is wrapped in remoteExit.
func TestSSHToInstancePassesTheRemoteStatus(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "connect.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	runs, wrapped := 0, 0
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "sshToInstance" {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Run" {
				runs++
			}
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "remoteExit" && len(call.Args) == 1 {
				if inner, ok := call.Args[0].(*ast.CallExpr); ok {
					if sel, ok := inner.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Run" {
						wrapped++
					}
				}
			}
			return true
		})
	}
	if runs == 0 {
		t.Fatal("found no Run() call in sshToInstance; the discovery is broken")
	}
	if wrapped != runs {
		t.Errorf("sshToInstance has %d Run() call(s), %d wrapped in remoteExit: an unwrapped one exits 1 for every remote failure", runs, wrapped)
	}
}
