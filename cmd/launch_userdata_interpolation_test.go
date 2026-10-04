package cmd

import (
	"regexp"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
)

// emptyInterpolationPatterns are the signatures of a template field that was
// never set by the caller. Each one is a real, shipped bug or its near neighbour.
//
// This is the class spawn#684 belonged to: MPIConfig.BinariesBucket was never
// assigned, so the template rendered `s3:///mpi-keys/...`, EC2's parameter
// validation rejected it, and cloud-init's scripts-user module aborted — taking
// the rest of MPI setup with it. Nothing failed at build time, nothing failed in
// CI, and the suite was green while --mpi could not work at all.
var emptyInterpolationPatterns = []struct {
	name string
	re   *regexp.Regexp
	why  string
}{
	{
		name: "empty S3 bucket",
		re:   regexp.MustCompile(`s3://(/|\s|$)`),
		why:  "an unset bucket renders s3:/// — spawn#684, which aborted cloud-init",
	},
	{
		name: "empty ARN segment",
		re:   regexp.MustCompile(`arn:aws:[a-z0-9-]*:::?(/\*)?(\s|"|$)`),
		why:  "an unset account/region/resource renders a malformed ARN that IAM rejects",
	},
	{
		name: "empty mount or path argument",
		re:   regexp.MustCompile(`(?m)^\s*(mkdir -p|mount -t [a-z0-9]+ -o [^ ]+)\s*$`),
		why:  "an unset mount point renders a path-less command that silently does nothing",
	},
	{
		name: "unresolved Go template",
		re:   regexp.MustCompile(`\{\{|\}\}|%!\w\(`),
		why:  "a template directive or a botched Sprintf reached the instance verbatim",
	},
	{
		name: "literal <no value>",
		re:   regexp.MustCompile(`<no value>`),
		why:  "text/template rendered a missing field as the literal string '<no value>'",
	},
}

// TestMemberUserDataHasNoEmptyInterpolations renders user-data through the
// PRODUCTION builder and scans it for unset-field signatures.
//
// Rendering through buildJobArrayMemberConfig is the whole point. The test that
// should have caught spawn#684 — TestGenerateMPIUserData_RegionSubstitution —
// instead set BinariesBucket itself and asserted the bucket appeared. It
// therefore proved the template COULD interpolate a bucket while never checking
// that anything did, and passed for years against a code path that could not
// run. A fixture built by the test can only confirm the test author's beliefs; a
// fixture built by the shipping code cannot.
func TestMemberUserDataHasNoEmptyInterpolations(t *testing.T) {
	prevMPI, prevEFA, prevCmd, prevSkip := mpiEnabled, efaEnabled, mpiCommand, mpiSkipInstall
	t.Cleanup(func() {
		mpiEnabled, efaEnabled, mpiCommand, mpiSkipInstall = prevMPI, prevEFA, prevCmd, prevSkip
	})

	cases := []struct {
		name           string
		mpi, efa, skip bool
		command        string
		members        int
	}{
		{name: "mpi", mpi: true, members: 2, command: "hostname --short"},
		{name: "mpi+efa", mpi: true, efa: true, members: 4, command: "./solver --np 8"},
		{name: "mpi, no command", mpi: true, members: 2},
		{name: "mpi, skip install", mpi: true, skip: true, members: 2, command: "true"},
		{name: "plain array", members: 3, command: "echo hi"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mpiEnabled, efaEnabled, mpiCommand, mpiSkipInstall = tc.mpi, tc.efa, tc.command, tc.skip

			base := &aws.LaunchConfig{
				InstanceType: "c6i.large",
				Region:       "us-east-1",
				UserData:     encodeUserData("#!/bin/bash\necho base bootstrap\n"),
			}
			mp := memberParams{name: "arr", size: tc.members, mpi: tc.mpi, command: tc.command}

			for i := 0; i < tc.members; i++ {
				cfg, err := buildJobArrayMemberConfig(base, mp, "arr-20261004-abcdef", i, nil)
				if err != nil {
					t.Fatalf("member %d: buildJobArrayMemberConfig: %v", i, err)
				}
				script := string(decodeUserDataOnce(t, cfg.UserData))

				for _, p := range emptyInterpolationPatterns {
					if loc := p.re.FindStringIndex(script); loc != nil {
						t.Errorf("member %d: %s — %s\n  matched %q at offset %d\n  context: %s",
							i, p.name, p.why, script[loc[0]:loc[1]], loc[0],
							contextAround(script, loc[0]))
					}
				}
			}
		})
	}
}

// contextAround returns the line containing off, for a readable failure.
func contextAround(s string, off int) string {
	start := strings.LastIndexByte(s[:off], '\n') + 1
	end := off + strings.IndexByte(s[off:], '\n')
	if end < off {
		end = len(s)
	}
	line := s[start:end]
	if len(line) > 160 {
		line = line[:160] + "…"
	}
	return line
}
