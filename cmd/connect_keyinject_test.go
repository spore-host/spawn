package cmd

import (
	"strings"
	"testing"
)

func TestAuthorizedKeyInjectionScript_Wellformed(t *testing.T) {
	key := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIabc spawn-key-ec2-user"
	script, err := authorizedKeyInjectionScript("ec2-user", key)
	if err != nil {
		t.Fatalf("authorizedKeyInjectionScript: %v", err)
	}

	for _, want := range []string{
		`u="ec2-user"`,                             // login user quoted
		"getent passwd",                            // resolves home dir, not hardcoded
		"mkdir -p \"$home/.ssh\"",                  // creates .ssh
		"chmod 700 \"$home/.ssh\"",                 // dir perms
		"chmod 600 \"$home/.ssh/authorized_keys\"", // file perms
		"grep -qF",                                 // idempotent append
		key,                                        // the actual key
		`chown -R "$u":"$u"`,                       // ownership
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q\n---\n%s", want, script)
		}
	}
}

// TestAuthorizedKeyInjectionScript_Idempotent asserts the append is guarded by a
// grep so reconnecting doesn't pile duplicate keys into authorized_keys.
func TestAuthorizedKeyInjectionScript_Idempotent(t *testing.T) {
	script, err := authorizedKeyInjectionScript("ec2-user", "ssh-ed25519 AAAA x")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(script, `grep -qF "$key" "$home/.ssh/authorized_keys" || echo`) {
		t.Error("append is not guarded by a grep — would duplicate the key on repeat connects")
	}
}

func TestAuthorizedKeyInjectionScript_RejectsUnsafeUser(t *testing.T) {
	for _, bad := range []string{"a; rm -rf /", "a b", "a$x", "a'b", "a`b`"} {
		if _, err := authorizedKeyInjectionScript(bad, "ssh-ed25519 AAAA x"); err == nil {
			t.Errorf("expected rejection of unsafe user %q", bad)
		}
	}
}

func TestAuthorizedKeyInjectionScript_RejectsUnsafeKey(t *testing.T) {
	// A key containing a single quote could break out of the single-quoted
	// literal; a newline could inject extra commands. Both must be rejected.
	for _, bad := range []string{"ssh-ed25519 AAAA'; rm -rf / #", "ssh-ed25519 AAAA\necho pwned"} {
		if _, err := authorizedKeyInjectionScript("ec2-user", bad); err == nil {
			t.Errorf("expected rejection of unsafe key %q", bad)
		}
	}
}
