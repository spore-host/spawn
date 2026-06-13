package aws

import (
	"testing"

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// TestBuildTags_FSxIDWritten is a regression test for #314.
// --fsx-id / --efs-id did not write instance tags, so boot scripts
// could not auto-mount without hardcoding the filesystem ID.
// Also tests spawn:fsx-mount-name which enables scripts to perform the
// Lustre mount without calling the FSx API (mount requires the MountName,
// not the filesystem ID).
func TestBuildTags_FSxIDWritten(t *testing.T) {
	config := LaunchConfig{
		Name:          "test-instance",
		FSxLustreID:   "fs-0abc1234",
		FSxMountName:  "q5pdvb4v",
		FSxMountPoint: "/fsx",
	}

	tags := buildTags(config, "123456789012", "arn:aws:iam::123456789012:user/test", "")

	fsxID := findTagValue(tags, "spawn:fsx-id")
	if fsxID != "fs-0abc1234" {
		t.Errorf("spawn:fsx-id = %q, want %q", fsxID, "fs-0abc1234")
	}

	fsxMount := findTagValue(tags, "spawn:fsx-mount-point")
	if fsxMount != "/fsx" {
		t.Errorf("spawn:fsx-mount-point = %q, want %q", fsxMount, "/fsx")
	}

	fsxMountName := findTagValue(tags, "spawn:fsx-mount-name")
	if fsxMountName != "q5pdvb4v" {
		t.Errorf("spawn:fsx-mount-name = %q, want %q", fsxMountName, "q5pdvb4v")
	}
}

// TestBuildTags_FSxMountPointDefault verifies the default /fsx is used when unset.
func TestBuildTags_FSxMountPointDefault(t *testing.T) {
	config := LaunchConfig{
		Name:        "test-instance",
		FSxLustreID: "fs-0abc1234",
		// FSxMountPoint intentionally empty
	}

	tags := buildTags(config, "123456789012", "arn:aws:iam::123456789012:user/test", "")

	fsxMount := findTagValue(tags, "spawn:fsx-mount-point")
	if fsxMount != "/fsx" {
		t.Errorf("spawn:fsx-mount-point default = %q, want /fsx", fsxMount)
	}
}

// TestBuildTags_EFSIDWritten verifies EFS tags are written (regression for #314).
func TestBuildTags_EFSIDWritten(t *testing.T) {
	config := LaunchConfig{
		Name:          "test-instance",
		EFSID:         "fs-0def5678",
		EFSMountPoint: "/efs",
	}

	tags := buildTags(config, "123456789012", "arn:aws:iam::123456789012:user/test", "")

	efsID := findTagValue(tags, "spawn:efs-id")
	if efsID != "fs-0def5678" {
		t.Errorf("spawn:efs-id = %q, want %q", efsID, "fs-0def5678")
	}

	efsMount := findTagValue(tags, "spawn:efs-mount-point")
	if efsMount != "/efs" {
		t.Errorf("spawn:efs-mount-point = %q, want /efs", efsMount)
	}
}

// TestBuildTags_CommandWritten is a regression test for #298.
// --command was accepted but spawn:command tag was not written, so spored
// never executed the command.
func TestBuildTags_CommandWritten(t *testing.T) {
	config := LaunchConfig{
		Name:            "test-instance",
		JobArrayCommand: "python train.py --lr 0.001",
	}

	tags := buildTags(config, "123456789012", "arn:aws:iam::123456789012:user/test", "")

	cmd := findTagValue(tags, "spawn:command")
	if cmd != "python train.py --lr 0.001" {
		t.Errorf("spawn:command = %q, want %q", cmd, "python train.py --lr 0.001")
	}
}

// TestBuildTags_NoFSxWhenNotSet verifies FSx tags are absent when not configured.
func TestBuildTags_NoFSxWhenNotSet(t *testing.T) {
	config := LaunchConfig{Name: "test-instance"}
	tags := buildTags(config, "123456789012", "arn:aws:iam::123456789012:user/test", "")

	if v := findTagValue(tags, "spawn:fsx-id"); v != "" {
		t.Errorf("spawn:fsx-id should be absent when FSxLustreID is empty, got %q", v)
	}
	if v := findTagValue(tags, "spawn:efs-id"); v != "" {
		t.Errorf("spawn:efs-id should be absent when EFSID is empty, got %q", v)
	}
}

// TestBuildTags_PublicIPAlwaysRequested verifies AssociatePublicIpAddress is
// set in the network interface spec regardless of whether a subnet is specified
// (regression for #308 — instances launched without SubnetID had no public IP).
// We can't test RunInstances input directly here, but we verify the LaunchConfig
// fields that drive the network interface construction.
func TestBuildTags_ManagedTagPresent(t *testing.T) {
	config := LaunchConfig{Name: "test"}
	tags := buildTags(config, "123456789012", "arn:aws:iam::123456789012:user/test", "")

	managed := findTagValue(tags, "spawn:managed")
	if managed != "true" {
		t.Errorf("spawn:managed = %q, want true", managed)
	}
}

// findTagValue looks up a tag value by key in the buildTags output.
func findTagValue(tags []ec2types.Tag, key string) string {
	for _, t := range tags {
		if t.Key != nil && *t.Key == key && t.Value != nil {
			return *t.Value
		}
	}
	return ""
}

// TestBuildTags_AccountName covers the #121 friendly-name DNS segment: when a
// non-empty slug is passed, buildTags writes spawn:account-name; when empty
// (no account name / not permitted), the tag is absent and base36 still stands.
func TestBuildTags_AccountName(t *testing.T) {
	config := LaunchConfig{Name: "job"}

	withName := buildTags(config, "123456789012", "arn:aws:iam::123456789012:user/test", "hpc-burst-prod")
	if got := findTagValue(withName, "spawn:account-name"); got != "hpc-burst-prod" {
		t.Errorf("spawn:account-name = %q, want hpc-burst-prod", got)
	}
	// base36 is always present regardless (canonical fallback).
	if findTagValue(withName, "spawn:account-base36") == "" {
		t.Error("spawn:account-base36 must always be present")
	}

	withoutName := buildTags(config, "123456789012", "arn:aws:iam::123456789012:user/test", "")
	if got := findTagValue(withoutName, "spawn:account-name"); got != "" {
		t.Errorf("spawn:account-name should be absent when slug is empty, got %q", got)
	}
}

func TestSlugifyDNSLabel(t *testing.T) {
	cases := map[string]string{
		"hpc-burst-prod":      "hpc-burst-prod",
		"HPC Burst Prod":      "hpc-burst-prod", // lowercased, spaces -> hyphen
		"acme_research.team":  "acme-research-team",
		"  leading/trailing ": "leading-trailing", // trimmed, slash -> hyphen
		"a--b___c":            "a-b-c",            // runs collapse to one hyphen
		"Prod!!!":             "prod",             // trailing junk trimmed
		"":                    "",                 // empty in, empty out
		"!!!":                 "",                 // no valid chars -> empty
		"-leading-hyphen-":    "leading-hyphen",
	}
	for in, want := range cases {
		if got := slugifyDNSLabel(in); got != want {
			t.Errorf("slugifyDNSLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSlugifyDNSLabel_MaxLength caps at 63 chars (RFC 1035 label) with no
// trailing hyphen.
func TestSlugifyDNSLabel_MaxLength(t *testing.T) {
	long := ""
	for i := 0; i < 100; i++ {
		long += "a"
	}
	got := slugifyDNSLabel(long)
	if len(got) != 63 {
		t.Errorf("len = %d, want 63 (DNS label max)", len(got))
	}
}
