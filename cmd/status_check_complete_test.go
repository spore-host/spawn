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
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
)

// The --check-complete contract, from the flag's help text:
// "0=complete, 1=failed, 2=running, 3=error".
const (
	wantComplete = 0
	wantFailed   = 1
	wantRunning  = 2
	wantError    = 3
)

type ssmCall struct{ region, instanceID, command string }

// recordingSSM is an ssmShellRunner that records each call and returns res/err.
func recordingSSM(calls *[]ssmCall, res *aws.SSMRunResult, err error) ssmShellRunner {
	return func(_ context.Context, region, instanceID, command string, _ time.Duration) (*aws.SSMRunResult, error) {
		*calls = append(*calls, ssmCall{region, instanceID, command})
		return res, err
	}
}

var checkCompleteInstance = &aws.InstanceInfo{InstanceID: "i-0cfc74096c1099179", Region: "us-east-1"}

// #1493: the bootstrap failed before it created the spawn:local-username user,
// so SSH is refused (ssh exits 255) while spored on the instance has the failed
// record and exits 1. spawn must ask spored over SSM and exit 1, not 3.
func TestCheckCompleteFallsBackToSSMWhenSSHAuthFails(t *testing.T) {
	sshCalls := 0
	runSSH := func() (int, string, error) {
		sshCalls++
		return 255, "wheel@44.212.15.221: Permission denied (publickey,gssapi-keyex,gssapi-with-mic).", nil
	}
	var calls []ssmCall
	run := recordingSSM(&calls, &aws.SSMRunResult{Status: "Failed", ResponseCode: 1}, nil)
	var stderr bytes.Buffer

	got := checkCompleteViaSSH(context.Background(), runSSH, run, checkCompleteInstance, &stderr)

	if got != wantFailed {
		t.Errorf("exit code = %d, want %d (failed): spored over SSM said failed; stderr:\n%s", got, wantFailed, stderr.String())
	}
	if sshCalls != 1 {
		t.Errorf("ssh ran %d times, want 1", sshCalls)
	}
	if len(calls) != 1 {
		t.Fatalf("SSM RunCommand sent %d times, want exactly 1: %+v", len(calls), calls)
	}
	c := calls[0]
	if c.region != "us-east-1" || c.instanceID != "i-0cfc74096c1099179" {
		t.Errorf("SSM sent to %s/%s, want us-east-1/i-0cfc74096c1099179", c.region, c.instanceID)
	}
	if strings.TrimSpace(c.command) != "/usr/local/bin/spored status --check-complete" {
		t.Errorf("SSM command = %q, want spored's own --check-complete", c.command)
	}
	if !strings.Contains(stderr.String(), "Permission denied") {
		t.Errorf("stderr should keep ssh's diagnostic; got:\n%s", stderr.String())
	}
}

// Every answer spored can give over the fallback reaches the caller unchanged.
func TestCheckCompleteSSMFallbackPassesSporedsCodeThrough(t *testing.T) {
	for _, code := range []int{wantComplete, wantFailed, wantRunning, wantError} {
		var calls []ssmCall
		run := recordingSSM(&calls, &aws.SSMRunResult{Status: "Success", ResponseCode: int32(code)}, nil)
		runSSH := func() (int, string, error) { return 255, "", nil }
		if got := checkCompleteViaSSH(context.Background(), runSSH, run, checkCompleteInstance, &bytes.Buffer{}); got != code {
			t.Errorf("spored exited %d over SSM: spawn exit = %d, want %d", code, got, code)
		}
	}
}

// ssh that cannot even be started is no answer from spored either.
func TestCheckCompleteFallsBackToSSMWhenSSHCannotRun(t *testing.T) {
	var calls []ssmCall
	run := recordingSSM(&calls, &aws.SSMRunResult{Status: "Success", ResponseCode: 2}, nil)
	runSSH := func() (int, string, error) { return -1, "", exec.ErrNotFound }
	if got := checkCompleteViaSSH(context.Background(), runSSH, run, checkCompleteInstance, &bytes.Buffer{}); got != wantRunning {
		t.Errorf("exit code = %d, want %d from SSM", got, wantRunning)
	}
	if len(calls) != 1 {
		t.Errorf("SSM sent %d times, want 1", len(calls))
	}
}

// When SSH reached spored, its answer stands and SSM is never used: the
// fallback must not double every poll's cost or override a real answer.
func TestCheckCompleteSSHAnswerStandsWithoutSSM(t *testing.T) {
	for _, code := range []int{wantComplete, wantFailed, wantRunning, wantError} {
		var calls []ssmCall
		run := recordingSSM(&calls, &aws.SSMRunResult{Status: "Success", ResponseCode: 0}, nil)
		runSSH := func() (int, string, error) { return code, "", nil }
		if got := checkCompleteViaSSH(context.Background(), runSSH, run, checkCompleteInstance, &bytes.Buffer{}); got != code {
			t.Errorf("spored exited %d over SSH: spawn exit = %d", code, got)
		}
		if len(calls) != 0 {
			t.Errorf("spored answered %d over SSH, yet SSM was sent %d times", code, len(calls))
		}
	}
}

// When SSM fails too, or does not return one of spored's four codes, the
// answer is 3 (error), never a code outside the contract.
func TestCheckCompleteSSMFailureIsError(t *testing.T) {
	runSSH := func() (int, string, error) { return 255, "", nil }
	cases := []struct {
		name string
		res  *aws.SSMRunResult
		err  error
	}{
		{"ssm error", nil, errors.New("InvalidInstanceId: instance not in a valid state")},
		{"timed out", &aws.SSMRunResult{Status: "TimedOut", ResponseCode: -1}, nil},
		{"spored missing", &aws.SSMRunResult{Status: "Failed", ResponseCode: 127}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls []ssmCall
			got := checkCompleteViaSSH(context.Background(), runSSH, recordingSSM(&calls, tc.res, tc.err), checkCompleteInstance, &bytes.Buffer{})
			if got != wantError {
				t.Errorf("exit code = %d, want %d (error)", got, wantError)
			}
		})
	}
}

// sshExitStatus must report ssh's real 255 as 255: that is what triggers the
// fallback. Uses a real child process (this test binary) so the
// *exec.ExitError is genuine; the child exits at once.
func TestSSHExitStatusOfARealExit255(t *testing.T) {
	if os.Getenv("SPAWN_TEST_EXIT_255") == "1" {
		os.Exit(255)
	}
	// nosemgrep
	c := exec.Command(os.Args[0], "-test.run=^TestSSHExitStatusOfARealExit255$")
	c.Env = append(os.Environ(), "SPAWN_TEST_EXIT_255=1")
	code, err := sshExitStatus(c.Run())
	if err != nil || code != 255 {
		t.Errorf("sshExitStatus = (%d, %v), want (255, nil)", code, err)
	}
	if code, err := sshExitStatus(nil); code != 0 || err != nil {
		t.Errorf("sshExitStatus(nil) = (%d, %v), want (0, nil)", code, err)
	}
	if _, err := sshExitStatus(exec.ErrNotFound); err == nil {
		t.Error("sshExitStatus(ErrNotFound) returned no error: a missing ssh must not look like a remote exit")
	}
}

// The helpers above are only the fix if the command uses them: runStatus's
// SSH --check-complete path must go through checkCompleteViaSSH (#1493), and
// the keyless SSM path through checkCompleteOverSSM, so both map SSM the same.
func TestStatusCheckCompletePathsUseTheFallback(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "status.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"runStatus":        "checkCompleteViaSSH",
		"runStatusOverSSM": "checkCompleteOverSSM",
	}
	found := map[string]bool{}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || want[fn.Name.Name] == "" {
			continue
		}
		callee := want[fn.Name.Name]
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == callee {
					found[fn.Name.Name] = true
				}
			}
			return true
		})
	}
	for fn, callee := range want {
		if !found[fn] {
			t.Errorf("%s does not call %s", fn, callee)
		}
	}
}
