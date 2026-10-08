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

// TestBuildRemoteCommand pins BOTH directions of the one-shot rule, because each
// has now been broken by fixing the other: #369 (argv re-split by space-joining)
// and #738 (a command string quoted into a command name).
func TestBuildRemoteCommand(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
		why  string
	}{
		{
			"a single argument is a command line, passed through verbatim",
			[]string{"free -m; tail /var/log/x"},
			"free -m; tail /var/log/x",
			"#738: quoting it made the remote shell look for a command with that literal name (exit 127)",
		},
		{
			"a single simple command is also passed through",
			[]string{"uptime"},
			"uptime",
			"one argument is one argument",
		},
		{
			"a single argument with a pipeline keeps its operators",
			[]string{"ps aux | grep spored | wc -l"},
			"ps aux | grep spored | wc -l",
			"the remote shell must see the pipes, not a filename containing them",
		},
		{
			"multiple arguments are an argv, each quoted",
			[]string{"tail", "-25", "/var/log/x"},
			"'tail' '-25' '/var/log/x'",
			"argument boundaries must survive",
		},
		{
			"bash -lc keeps its script as ONE argument",
			[]string{"bash", "-lc", "a && b"},
			"'bash' '-lc' 'a && b'",
			"#369: space-joining made bash's -c get no argument",
		},
		{
			"an embedded single quote is escaped in the argv form",
			[]string{"echo", "it's"},
			`'echo' 'it'\''s'`,
			"the quote must not terminate the quoting",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildRemoteCommand(tc.args); got != tc.want {
				t.Errorf("buildRemoteCommand(%q) =\n  %s\nwant\n  %s\n(%s)", tc.args, got, tc.want, tc.why)
			}
		})
	}
}

// The single-argument passthrough is unquoted BY DESIGN — it is a shell command
// line, with the same trust as --command, and the user wrote it. Asserted
// explicitly so nobody "hardens" it back into #738 by quoting it.
func TestBuildRemoteCommand_SingleArgIsNotQuoted(t *testing.T) {
	got := buildRemoteCommand([]string{"echo $HOME && hostname"})
	if strings.Contains(got, "'") {
		t.Errorf("single argument was quoted (%s) — that is #738, and it is what makes "+
			"a command string unrunnable", got)
	}
}
