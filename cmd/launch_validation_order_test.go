package cmd

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestFlagValidationPrecedesResourceCreation is spawn#685.
//
// `spawn launch x --mpi --count 2` with no --job-array-name used to create a
// managed security group and THEN fail validation, leaving the group behind.
// validateMPIFlags ran ~286 lines after ensureSecurityGroup in the same
// function.
//
// The leftover was named `spawn-mpi-` — the prefix with an empty suffix — so
// every such launch shared one group. Since #659 that group allows ALL protocols
// between its members, which makes accidental sharing worse than it was. And
// nothing reaps them (#685's other half), so each failed attempt added a durable
// orphan: 19 security groups and 9 placement groups were found in a single
// region of one account.
//
// Asserted on source order because the property IS an ordering, and there is no
// runtime value that expresses "this happened before that".
func TestFlagValidationPrecedesResourceCreation(t *testing.T) {
	b, err := os.ReadFile("launch_single.go")
	if err != nil {
		t.Fatalf("read launch_single.go: %v", err)
	}
	src := string(b)

	start := strings.Index(src, "func launchWithProgress(")
	if start < 0 {
		t.Fatal("launchWithProgress not found; this gate would pass vacuously")
	}
	body := src[start:]

	validate := strings.Index(body, "validateMPIFlags(")
	if validate < 0 {
		t.Fatal("launchWithProgress no longer validates MPI flags at all")
	}

	// Every call that creates or mutates an AWS resource must come after it.
	creators := []string{
		"ensureSecurityGroup(",
		"ensureIAMProfile(",
		"resolveMPIPlacement(",
	}
	for _, c := range creators {
		at := strings.Index(body, c)
		if at < 0 {
			continue
		}
		if at < validate {
			t.Errorf("%s is called at offset %d, BEFORE validateMPIFlags at %d.\n\n"+
				"Pure flag validation must precede the first AWS mutation, or a rejected "+
				"launch leaves resources behind — which is how `spawn-mpi-` orphans "+
				"accumulated (#685).", strings.TrimSuffix(c, "("), at, validate)
		}
	}
}

// TestMPISecurityGroupNameHasASuffix: the defensive half. Even with the ordering
// fixed, a caller that passes an empty cluster name must be refused rather than
// handed the shared bare-prefix group.
func TestMPISecurityGroupNameHasASuffix(t *testing.T) {
	b, err := os.ReadFile("../pkg/aws/securitygroup.go")
	if err != nil {
		t.Fatalf("read securitygroup.go: %v", err)
	}
	src := string(b)

	if !regexp.MustCompile(`groupName == ""\s*\|\|\s*strings\.HasSuffix\(groupName, "-"\)`).MatchString(src) {
		t.Error("CreateOrGetMPISecurityGroup does not refuse a suffix-less name. " +
			"\"spawn-mpi-\" + \"\" is the bare prefix, and since #659 that group allows all " +
			"protocols between its members — so unrelated clusters sharing it is worse " +
			"than a naming wart (#685).")
	}
}
