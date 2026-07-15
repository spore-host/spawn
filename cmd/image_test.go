package cmd

import (
	"strings"
	"testing"
)

// TestEC2LaunchRearmCommand pins the re-arm the warm-AMI build runs before
// imaging (#153): it must use EC2Launch's `reset` (re-enables setAdminAccount so
// GetPasswordData works on launches from the warm AMI) and must NOT generalize
// the image (no `sysprep`), since the warm AMI is a non-generalized
// single-instance image (#98).
func TestEC2LaunchRearmCommand(t *testing.T) {
	cmd := ec2LaunchRearmCommand
	if !strings.Contains(cmd, "EC2Launch.exe") {
		t.Errorf("re-arm command should invoke EC2Launch.exe; got %q", cmd)
	}
	if !strings.Contains(cmd, " reset") {
		t.Errorf("re-arm command must use `reset` to re-enable setAdminAccount; got %q", cmd)
	}
	if !strings.Contains(cmd, "-c") {
		t.Errorf("re-arm command should pass -c to clear run-once state + logs; got %q", cmd)
	}
	if strings.Contains(cmd, "sysprep") {
		t.Errorf("re-arm must NOT sysprep/generalize the warm AMI; got %q", cmd)
	}
	if !strings.Contains(cmd, `$Env:ProgramFiles\Amazon\EC2Launch`) {
		t.Errorf("re-arm command should use the EC2Launch v2 install path; got %q", cmd)
	}
}

func TestValidateImageImportFlags(t *testing.T) {
	cases := []struct {
		name           string
		iso, ami, buck string
		wantErrSub     string // "" = expect success
	}{
		{name: "missing iso", iso: "", ami: "win11", buck: "", wantErrSub: "--iso"},
		{name: "missing name", iso: "s3://b/x.ISO", ami: "", buck: "", wantErrSub: "--name"},
		{name: "s3 uri ok, no infra arn needed", iso: "s3://b/x.ISO", ami: "win11", buck: "", wantErrSub: ""},
		{name: "s3 uri lowercase iso rejected", iso: "s3://b/x.iso", ami: "win11", buck: "", wantErrSub: ".ISO"},
		{name: "local iso without bucket ok (managed bucket)", iso: "./win11.iso", ami: "win11", buck: "", wantErrSub: ""},
		{name: "local iso with bucket ok", iso: "./win11.iso", ami: "win11", buck: "my-bucket", wantErrSub: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateImageImportFlags(tc.iso, tc.ami, tc.buck)
			if tc.wantErrSub == "" {
				if err != nil {
					t.Fatalf("expected success, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErrSub) {
				t.Fatalf("expected error containing %q, got %v", tc.wantErrSub, err)
			}
		})
	}
}

// TestWaitFlagParsing verifies `image import --wait` is now a boolean (matching
// launch/ami/pipeline, #317) and that the minute count moved to --wait-timeout
// (default 60).
func TestWaitFlagParsing(t *testing.T) {
	wf := imageImportCmd.Flags().Lookup("wait")
	if wf == nil {
		t.Fatal("--wait flag not registered")
	}
	if wf.Value.Type() != "bool" {
		t.Errorf("--wait type = %q, want bool", wf.Value.Type())
	}
	if wf.DefValue != "false" {
		t.Errorf("--wait default = %q, want false", wf.DefValue)
	}

	wt := imageImportCmd.Flags().Lookup("wait-timeout")
	if wt == nil {
		t.Fatal("--wait-timeout flag not registered")
	}
	if wt.Value.Type() != "int" {
		t.Errorf("--wait-timeout type = %q, want int", wt.Value.Type())
	}
	if wt.DefValue != "60" {
		t.Errorf("--wait-timeout default = %q, want 60", wt.DefValue)
	}

	for _, tc := range []struct {
		args        []string
		wantWait    bool
		wantTimeout int
	}{
		{[]string{}, false, 60},
		{[]string{"--wait"}, true, 60},
		{[]string{"--wait", "--wait-timeout=10"}, true, 10},
	} {
		imageImportWait = false
		imageImportWaitTimeout = 60
		if err := imageImportCmd.Flags().Parse(tc.args); err != nil {
			t.Fatalf("parse %v: %v", tc.args, err)
		}
		if imageImportWait != tc.wantWait || imageImportWaitTimeout != tc.wantTimeout {
			t.Errorf("args %v → wait=%v timeout=%d, want wait=%v timeout=%d",
				tc.args, imageImportWait, imageImportWaitTimeout, tc.wantWait, tc.wantTimeout)
		}
	}
	imageImportWait = false
	imageImportWaitTimeout = 60
}
