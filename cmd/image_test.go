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
