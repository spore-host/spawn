package cmd

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
)

// A one-shot `spawn connect <linux> -- <cmd>` that takes the SSM path (no
// public IP, --session-manager, a security group closed to SSH, or failed key
// injection) runs the command with SSM RunCommand (AWS-RunShellScript) and
// exits with its status (#1306). It used to open an interactive
// `aws ssm start-session`, which dropped the command and waited on stdin.
//
// RunCommand rather than a start-session with AWS-StartInteractiveCommand,
// because RunCommand:
//   - returns the command's exit code (GetCommandInvocation's ResponseCode),
//     which a session does not hand back to the caller;
//   - keeps stdout and stderr apart (a session runs on a PTY, which merges
//     them, the same trade-off as --tty on the SSH path);
//   - needs neither the AWS CLI nor the session-manager-plugin on this machine.
//
// The cost: output arrives when the command exits, not as it is written, SSM
// keeps only the first 24,000 characters of stdout and 8,000 of stderr, stdin
// is not forwarded, and AWS-RunShellScript's own limit of one hour applies.

// ssmOneShotTimeout is how long connect waits for the invocation. It is just
// over AWS-RunShellScript's default executionTimeout (3600s), so SSM's own
// TimedOut status is what a too-long command reports.
const ssmOneShotTimeout = 61 * time.Minute

// SSM truncates GetCommandInvocation output at these lengths.
const (
	ssmStdoutLimit = 24000
	ssmStderrLimit = 8000
)

// remoteExitError carries a remote command's non-zero exit status out of
// `spawn connect`, so exitForError can exit with it.
type remoteExitError struct{ code int }

func (e *remoteExitError) Error() string {
	return fmt.Sprintf("remote command exited with status %d", e.code)
}

// remoteExitCode is the status err carries, or 0 if it is not a remoteExitError.
func remoteExitCode(err error) int {
	var re *remoteExitError
	if errors.As(err, &re) {
		return re.code
	}
	return 0
}

// connectOverSSM is every Linux SSM path of `spawn connect`: a one-shot
// command runs over RunCommand (run), anything else opens an interactive
// session (openSession, connectViaSessionManager in production).
func connectOverSSM(ctx context.Context, run ssmShellRunner, openSession func(instanceID, region string) error,
	instance *aws.InstanceInfo, user, remoteCmd string) error {
	if remoteCmd != "" {
		return runOneShotOverSSM(ctx, run, instance, user, remoteCmd, connectTTY, os.Stdout, os.Stderr)
	}
	return openSession(instance.InstanceID, instance.Region)
}

// runOneShotOverSSM runs remoteCmd on a Linux instance as user, over SSM
// RunCommand, writes its stdout and stderr, and returns a remoteExitError for a
// non-zero exit.
//
// run is (*aws.Client).RunShellScript (ssmShellRunner, cmd/terminate_flush.go).
func runOneShotOverSSM(ctx context.Context, run ssmShellRunner, instance *aws.InstanceInfo, user, remoteCmd string, tty bool, stdout, stderr io.Writer) error {
	script, err := oneShotSSMScript(user, remoteCmd)
	if err != nil {
		return err
	}
	if tty {
		_, _ = fmt.Fprintln(stderr, "note: --tty has no effect over SSM RunCommand; output arrives when the command exits")
	}
	_, _ = fmt.Fprintf(stderr, "Running on %s as %s via SSM RunCommand (no stdin; output arrives when the command exits)...\n", instance.InstanceID, user)
	res, err := run(ctx, instance.Region, instance.InstanceID, script, ssmOneShotTimeout)
	if err != nil {
		return fmt.Errorf("run the command over SSM: %w", err)
	}
	_, _ = io.WriteString(stdout, res.Stdout)
	_, _ = io.WriteString(stderr, res.Stderr)
	if len(res.Stdout) >= ssmStdoutLimit || len(res.Stderr) >= ssmStderrLimit {
		_, _ = fmt.Fprintf(stderr, "warning: SSM keeps only the first %d characters of stdout and %d of stderr, so this output may be cut short; "+
			"write large output to a file on the instance instead\n", ssmStdoutLimit, ssmStderrLimit)
	}
	switch {
	case res.Status == "Success" && res.ResponseCode == 0:
		return nil
	case res.Status == "Failed" && res.ResponseCode > 0 && res.ResponseCode <= 255:
		return &remoteExitError{code: int(res.ResponseCode)}
	default:
		// TimedOut, Cancelled, or a Failed with no exit status (the command
		// never ran): no remote status to pass on.
		return fmt.Errorf("remote command over SSM ended with status %s (exit code %d)", res.Status, res.ResponseCode)
	}
}

// oneShotSSMScript is the AWS-RunShellScript body that runs remoteCmd the way
// sshd would for user: in their home directory, with their login shell's -c.
// RunCommand runs as root, so it drops to user with sudo, the same user SSH
// logs in as (resolveSSHUser). The command travels base64-encoded, so nothing
// in it needs quoting; user is rejected if it is not a plain name. A missing
// user exits 255, which is ssh's code for "could not run the command at all".
func oneShotSSMScript(user, remoteCmd string) (string, error) {
	if user == "" || strings.ContainsAny(user, "'\"$`\\ \t\n;&|<>()") {
		return "", fmt.Errorf("unsafe ssh user %q", user)
	}
	enc := base64.StdEncoding.EncodeToString([]byte(remoteCmd))
	return fmt.Sprintf(`u=%s
pw=$(getent passwd "$u") || { echo "spawn connect: no user $u on this instance (pass --user)" >&2; exit 255; }
home=$(printf '%%s' "$pw" | cut -d: -f6)
sh=$(printf '%%s' "$pw" | cut -d: -f7)
cmd=$(printf '%%s' '%s' | base64 -d)
cd "$home" 2>/dev/null || cd /
exec sudo -H -u "$u" -- "${sh:-/bin/sh}" -c "$cmd"
`, user, enc), nil
}
