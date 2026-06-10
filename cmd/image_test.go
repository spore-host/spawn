package cmd

import (
	"strings"
	"testing"
)

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
