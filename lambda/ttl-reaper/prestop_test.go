package main

import (
	"os"
	"strings"
	"testing"
)

// readReaperSource returns main.go's text. Tests run with the package directory as
// the working directory, so the relative path is stable.
func readReaperSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	return string(b)
}

// The pre-stop hook must never run as root.
//
// This is the invariant, not the behaviour: the reaper's own header says the hook
// runs "as spawn:local-username, per #63". Both the hook body and the username are
// instance tags, and the spored role can write them on any spawn:managed instance
// in the account, so root execution here is root execution on someone else's
// instance (GHSA-8q3p-qcv8-2pr4).
//
// Written against buildPreStopCommand rather than tryGracefulPreStop because
// ssmFor returns a concrete *ssm.Client and cannot be faked — and because the
// guard belongs to the command construction, not to the SSM plumbing (CLAUDE.md
// rule 2). A test driven through the caller would also have to get past a
// five-condition early return to reach it.
func TestBuildPreStopCommandNeverRunsAsRoot(t *testing.T) {
	hostileUsernames := map[string]string{
		"empty — the tag is absent, or the spored role deleted it": "",
		"su argument injection, the historic bug":                  `x -c 'id > /tmp/pwned' #`,
		"command substitution":                                     "$(id)",
		"backticks":                                                "`id`",
		"semicolon":                                                "ubuntu; id",
		"newline":                                                  "ubuntu\nid",
		"root itself":                                              "root",
		"leading dash, which su reads as an option":                "-c",
		"shell metacharacter in an otherwise plausible name": "ubuntu|id",
		"uppercase, which is not a POSIX login name":         "Ubuntu",
		"over the 32-character limit":                        strings.Repeat("a", 33),
	}

	for name, user := range hostileUsernames {
		t.Run(name, func(t *testing.T) {
			cmd, err := buildPreStopCommand("aws s3 sync /out s3://b/", user)
			if err == nil {
				t.Fatalf("username %q was accepted and produced %q — the reaper would hand this to "+
					"SSM AWS-RunShellScript, which runs as root", user, cmd)
			}
			if cmd != "" {
				t.Errorf("rejected %q but still returned a command %q; the caller checks only the "+
					"error, so a non-empty command here is one refactor away from being executed",
					user, cmd)
			}
		})
	}

	// "root" must be rejected because it is not reachable as a legitimate
	// spawn:local-username, but note WHY it fails: ValidateUsername's charset
	// allows it, and it is the length/charset check that does not. Guard against a
	// future loosening by asserting the outcome, not the mechanism.
	if _, err := buildPreStopCommand("x", "root"); err == nil {
		t.Error(`a spawn:local-username of "root" is accepted — the privilege drop becomes a no-op`)
	}
}

// The happy path must still drop privilege, and must quote the hook.
func TestBuildPreStopCommandDropsPrivilegeAndQuotesTheHook(t *testing.T) {
	// A hook containing a single quote is the case naive quoting gets wrong.
	hook := `aws s3 sync /out s3://b/ --exclude "it's"`
	cmd, err := buildPreStopCommand(hook, "ubuntu")
	if err != nil {
		t.Fatalf("a valid username was rejected: %v", err)
	}
	if !strings.HasPrefix(cmd, "su - ") {
		t.Errorf("command does not drop privilege: %q", cmd)
	}
	if !strings.Contains(cmd, "'ubuntu'") {
		t.Errorf("username is not quoted in %q", cmd)
	}
	// The hook must appear only in quoted form. If the raw string with its bare
	// apostrophe survived, the quoting did not happen.
	if strings.Contains(cmd, hook) {
		t.Errorf("hook body appears unquoted in %q", cmd)
	}
	if !strings.Contains(cmd, `'\''`) {
		t.Errorf("the apostrophe in the hook was not escaped: %q", cmd)
	}
}

// The reaper must not keep a second, unvalidated path to the same place.
//
// The fix is only worth anything if buildPreStopCommand is the ONLY thing that
// constructs an su invocation from a tag. A future `su - %s` added elsewhere in
// this file would reintroduce the bug while every test above still passed.
func TestOnlyBuildPreStopCommandConstructsAnSuInvocation(t *testing.T) {
	src := readReaperSource(t)

	const marker = `"su - %s`
	n := strings.Count(src, marker)
	if n == 0 {
		t.Fatalf("found no %s format string in main.go — this gate is anchored on a string that no "+
			"longer exists and is checking nothing", marker)
	}
	if n > 1 {
		t.Errorf("found %d su-invocation format strings in main.go; there must be exactly one, inside "+
			"buildPreStopCommand, so the username validation cannot be bypassed", n)
	}
}

// The README must not contradict the code about the graceful tier.
//
// It did, in the most misleading direction available: "it does **not** run the
// in-instance pre-stop hook", written before #187 added exactly that and never
// updated. A reader auditing the reaper's blast radius would have concluded there
// was no SSM execution path at all. CLAUDE.md rule 5's corollary — gate the
// agreement, because the drift is the root cause and the behaviour is the symptom.
func TestReaperREADMEDescribesTheGracefulTierItActuallyHas(t *testing.T) {
	b, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	readme := string(b)

	if !strings.Contains(readReaperSource(t), "REAPER_GRACEFUL") {
		t.Skip("the graceful tier has been removed from main.go; this gate no longer applies")
	}

	// The flat denial, which is what went stale. Anchored on the sentence rather
	// than on "pre-stop" alone, because the README legitimately mentions the hook
	// several times.
	if strings.Contains(readme, "it does **not** run the in-instance pre-stop hook.") {
		t.Error("README still states flatly that the reaper does not run the pre-stop hook, but " +
			"REAPER_GRACEFUL makes it do so over SSM (#187)")
	}

	for _, want := range []string{"REAPER_GRACEFUL", "spawn:local-username"} {
		if !strings.Contains(readme, want) {
			t.Errorf("README does not mention %s. The graceful tier runs a tag's contents through SSM "+
				"as root unless it can drop privilege, which is the reaper's largest blast radius and "+
				"has to be documented where someone auditing it will look", want)
		}
	}
}
