package cmd

import (
	"context"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
)

// TestResolveTargetOS_FlagOverride verifies the --os flag wins without touching
// AWS (the IsWindowsAMI call is only reached when the flag is unset). A nil
// client is safe here because the flag branches return first.
func TestResolveTargetOS_FlagOverride(t *testing.T) {
	cases := []struct {
		flag string
		want string
	}{
		{"windows", "windows"},
		{"Windows", "windows"},
		{" linux ", "linux"},
		{"LINUX", "linux"},
	}
	for _, tc := range cases {
		got := resolveTargetOS(context.Background(), nil, "us-east-1", "ami-123", tc.flag)
		if got != tc.want {
			t.Errorf("resolveTargetOS(flag=%q) = %q, want %q", tc.flag, got, tc.want)
		}
	}
}

func TestWindowsLifecycleGuard(t *testing.T) {
	// Linux is unaffected.
	if err := windowsLifecycleGuard(&aws.LaunchConfig{TargetOS: "linux"}); err != nil {
		t.Errorf("linux must not be guarded: %v", err)
	}
	// Windows with neither timeout must error.
	if err := windowsLifecycleGuard(&aws.LaunchConfig{TargetOS: "windows"}); err == nil {
		t.Error("windows without a timeout must error")
	}
	// Windows with --ttl is allowed.
	if err := windowsLifecycleGuard(&aws.LaunchConfig{TargetOS: "windows", TTL: "8h"}); err != nil {
		t.Errorf("windows with --ttl must be allowed: %v", err)
	}
	// Windows with --idle-timeout is now allowed too (spored enforces idle on
	// Windows as of #77 Stage 3).
	if err := windowsLifecycleGuard(&aws.LaunchConfig{TargetOS: "windows", IdleTimeout: "1h"}); err != nil {
		t.Errorf("windows with --idle-timeout must be allowed: %v", err)
	}
}

func TestEncodeUserDataForOS(t *testing.T) {
	script := "hello"
	win := encodeUserDataForOS(script, "windows")
	lin := encodeUserDataForOS(script, "linux")
	// Windows is plain base64 (EC2Launch doesn't gunzip); Linux is gzip+base64.
	if win == lin {
		t.Fatal("windows and linux encodings should differ (plain vs gzip base64)")
	}
	// Plain base64 of "hello" is deterministic and short.
	if win != "aGVsbG8=" {
		t.Errorf("windows encoding = %q, want plain base64 aGVsbG8=", win)
	}
}

func TestBuildWindowsUserData(t *testing.T) {
	key := "ssh-rsa AAAAB3Nz... spawn-key-test"
	out, err := buildWindowsUserData(key)
	if err != nil {
		t.Fatalf("buildWindowsUserData: %v", err)
	}
	if !strings.HasPrefix(out, "<powershell>") {
		t.Error("windows user-data must be a <powershell> block")
	}
	if !strings.Contains(out, "administrators_authorized_keys") {
		t.Error("must install the key into administrators_authorized_keys")
	}
	if !strings.Contains(out, key) {
		t.Error("must embed the provided public key")
	}
	if strings.Contains(out, "#!/bin/bash") {
		t.Error("windows user-data must not contain bash")
	}
}

func TestBuildWindowsUserData_RejectsHerestringBreakout(t *testing.T) {
	if _, err := buildWindowsUserData(`ssh-rsa AAAA"@ evil`); err == nil {
		t.Error("must reject a key that could break out of the PowerShell here-string")
	}
}
