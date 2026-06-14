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

// TestWaitFlagParsing verifies the --wait flag is async by default (0), defaults
// to 60 when given bare (NoOptDefVal), and accepts an explicit minute count.
func TestWaitFlagParsing(t *testing.T) {
	wf := imageImportCmd.Flags().Lookup("wait")
	if wf == nil {
		t.Fatal("--wait flag not registered")
	}
	if wf.DefValue != "0" {
		t.Errorf("--wait default = %q, want 0 (async)", wf.DefValue)
	}
	if wf.NoOptDefVal != "60" {
		t.Errorf("bare --wait should default to 60, got %q", wf.NoOptDefVal)
	}

	for _, tc := range []struct {
		args []string
		want int
	}{
		{[]string{}, 0},
		{[]string{"--wait"}, 60},
		{[]string{"--wait=10"}, 10},
	} {
		imageImportWaitMin = 0
		// Parse against a throwaway flagset copy isn't trivial; parse the real one
		// then reset. cobra stores into imageImportWaitMin via IntVar.
		if err := imageImportCmd.Flags().Parse(tc.args); err != nil {
			t.Fatalf("parse %v: %v", tc.args, err)
		}
		if imageImportWaitMin != tc.want {
			t.Errorf("args %v → wait=%d, want %d", tc.args, imageImportWaitMin, tc.want)
		}
	}
	imageImportWaitMin = 0
}
