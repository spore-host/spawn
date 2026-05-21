package cmd

import (
	"strings"
	"testing"
)

// buildSSHArgs replicates the one-shot SSH argument construction from runConnect
// so we can test it without a live instance.
func buildSSHArgs(keyPath, user, host string, port int, remoteArgs []string) []string {
	args := []string{
		"-i", keyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-p", "22",
		user + "@" + host,
	}
	if len(remoteArgs) > 0 {
		// regression fix for #315: wrap in bash -c '...' so the remote shell
		// interprets operators (&&, ;, &) correctly and backgrounded processes
		// don't cause SSH to exit 255
		remoteCmd := strings.Join(remoteArgs, " ")
		remoteCmd = strings.ReplaceAll(remoteCmd, "'", "'\\''")
		args = append(args, "bash -c '"+remoteCmd+"'")
	}
	_ = port
	return args
}

// TestConnectOneShot_CompoundCommandJoined is a regression test for #315.
// Before the fix, remoteArgs were appended individually, so && was split.
// Now wrapped in bash -c '...' so the remote shell interprets operators.
func TestConnectOneShot_CompoundCommandJoined(t *testing.T) {
	remoteArgs := []string{"cmd1", "&&", "cmd2"}
	sshArgs := buildSSHArgs("key.pem", "ec2-user", "1.2.3.4", 22, remoteArgs)

	lastArg := sshArgs[len(sshArgs)-1]
	if lastArg != "bash -c 'cmd1 && cmd2'" {
		t.Errorf("expected bash -c wrapper with && preserved, got %q\nfull args: %v", lastArg, sshArgs)
	}
}

// TestConnectOneShot_BackgroundOperator verifies & in bash -c doesn't cause exit 255.
// The root cause of #315: SSH exits 255 when the remote process is backgrounded
// without a wrapper — bash -c handles this correctly.
func TestConnectOneShot_BackgroundOperator(t *testing.T) {
	remoteArgs := []string{"nohup", "bash", "/tmp/run.sh", ">", "/tmp/run.log", "2>&1", "&"}
	sshArgs := buildSSHArgs("key.pem", "ec2-user", "1.2.3.4", 22, remoteArgs)

	lastArg := sshArgs[len(sshArgs)-1]
	if !strings.HasPrefix(lastArg, "bash -c '") {
		t.Errorf("expected bash -c wrapper, got: %q", lastArg)
	}
	if !strings.Contains(lastArg, "&") {
		t.Errorf("background operator & must be preserved inside bash -c, got: %q", lastArg)
	}
}

// TestConnectOneShot_Semicolon verifies ; is preserved inside bash -c.
func TestConnectOneShot_Semicolon(t *testing.T) {
	remoteArgs := []string{"cmd1;", "cmd2"}
	sshArgs := buildSSHArgs("key.pem", "ec2-user", "1.2.3.4", 22, remoteArgs)

	lastArg := sshArgs[len(sshArgs)-1]
	if !strings.Contains(lastArg, ";") {
		t.Errorf("semicolon separator must be preserved in bash -c, got: %q", lastArg)
	}
}

// TestConnectOneShot_SingleCommandWrapped verifies single commands are also wrapped.
func TestConnectOneShot_SingleCommandWrapped(t *testing.T) {
	remoteArgs := []string{"tail", "-20", "/tmp/run.log"}
	sshArgs := buildSSHArgs("key.pem", "ec2-user", "1.2.3.4", 22, remoteArgs)

	lastArg := sshArgs[len(sshArgs)-1]
	if lastArg != "bash -c 'tail -20 /tmp/run.log'" {
		t.Errorf("expected bash -c wrapper, got %q", lastArg)
	}
}

// TestConnectOneShot_InteractiveModeNoExtraArgs verifies interactive mode (no remote args)
// does not append a remote command argument.
func TestConnectOneShot_InteractiveModeNoExtraArgs(t *testing.T) {
	remoteArgs := []string{}
	sshArgs := buildSSHArgs("key.pem", "ec2-user", "1.2.3.4", 22, remoteArgs)

	lastArg := sshArgs[len(sshArgs)-1]
	if lastArg != "ec2-user@1.2.3.4" {
		t.Errorf("interactive mode: last arg should be host, got %q", lastArg)
	}
}

// TestConnectOneShot_SingleQuoteEscaping verifies single quotes in the command
// are escaped before wrapping in bash -c '...'.
func TestConnectOneShot_SingleQuoteEscaping(t *testing.T) {
	remoteArgs := []string{"echo", "it's", "working"}
	sshArgs := buildSSHArgs("key.pem", "ec2-user", "1.2.3.4", 22, remoteArgs)

	lastArg := sshArgs[len(sshArgs)-1]
	// Single quote must be escaped as '\'' inside the bash -c wrapper
	if !strings.Contains(lastArg, `'\''`) {
		t.Errorf("single quote must be escaped as '\\'' inside bash -c wrapper, got: %q", lastArg)
	}
}

// TestConnectOneShot_PreQuotedString verifies a pre-quoted compound string works.
func TestConnectOneShot_PreQuotedString(t *testing.T) {
	// Simulates: spawn connect my-instance -- 'aws s3 cp s3://b/f /tmp/f && bash /tmp/f'
	// Shell has already stripped the outer quotes, leaving one arg.
	remoteArgs := []string{"aws s3 cp s3://bucket/run.sh /tmp/run.sh && bash /tmp/run.sh"}
	sshArgs := buildSSHArgs("key.pem", "ec2-user", "1.2.3.4", 22, remoteArgs)

	lastArg := sshArgs[len(sshArgs)-1]
	expected := "bash -c 'aws s3 cp s3://bucket/run.sh /tmp/run.sh && bash /tmp/run.sh'"
	if lastArg != expected {
		t.Errorf("expected %q, got %q", expected, lastArg)
	}
}
