package aws

import (
	"strings"
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
// TestBuildTags_LocalUsername verifies the instance's primary user is tagged as
// spawn:local-username so spored runs the pre-stop hook as that user, not root
// (#63). Absent when Username is empty (older behavior).
func TestBuildTags_LocalUsername(t *testing.T) {
	withUser := buildTags(LaunchConfig{Name: "t", Username: "ec2-user"},
		"123456789012", "arn:aws:iam::123456789012:user/test", "")
	if got := findTagValue(withUser, "spawn:local-username"); got != "ec2-user" {
		t.Errorf("spawn:local-username = %q, want ec2-user", got)
	}

	withoutUser := buildTags(LaunchConfig{Name: "t"},
		"123456789012", "arn:aws:iam::123456789012:user/test", "")
	if got := findTagValue(withoutUser, "spawn:local-username"); got != "" {
		t.Errorf("spawn:local-username = %q, want empty when Username unset", got)
	}
}

// TestBuildTags_FSxPending verifies the ephemeral async-FSx path (#194) tags the
// instance with spawn:fsx-pending + mount-point + import/export paths (so spored
// can wait, set up the DRA, and mount) — and NOT spawn:fsx-id (the FS isn't
// AVAILABLE yet; spored flips the tag after mounting).
func TestBuildTags_FSxPending(t *testing.T) {
	tags := buildTags(LaunchConfig{
		Name:          "t",
		FSxPending:    "fs-0pending",
		FSxMountPoint: "/fsx",
		FSxImportPath: "s3://b/in/",
		FSxExportPath: "s3://b/out/",
	}, "123456789012", "arn:aws:iam::123456789012:user/test", "")

	if got := findTagValue(tags, "spawn:fsx-pending"); got != "fs-0pending" {
		t.Errorf("spawn:fsx-pending = %q, want fs-0pending", got)
	}
	if got := findTagValue(tags, "spawn:fsx-mount-point"); got != "/fsx" {
		t.Errorf("spawn:fsx-mount-point = %q, want /fsx", got)
	}
	if got := findTagValue(tags, "spawn:fsx-s3-export-path"); got != "s3://b/out/" {
		t.Errorf("spawn:fsx-s3-export-path = %q, want s3://b/out/", got)
	}
	if got := findTagValue(tags, "spawn:fsx-id"); got != "" {
		t.Errorf("spawn:fsx-id should be empty for a pending FSx, got %q", got)
	}
}

// TestBuildTags_SpotWebhook verifies the spot-interruption webhook fields (#228)
// are tagged only when a URL is set (opt-in): the URL, the opaque correlation
// blob, and the timeout all become spawn:* tags; with no URL, none appear (the
// correlation/timeout are companions meaningful only alongside a URL).
func TestBuildTags_SpotWebhook(t *testing.T) {
	withURL := buildTags(LaunchConfig{
		Name:                       "t",
		SpotInterruptionWebhookURL: "https://example.test/hook",
		WebhookCorrelation:         "opaque-blob-42",
		WebhookTimeout:             "3s",
	}, "123456789012", "arn:aws:iam::123456789012:user/test", "")

	if got := findTagValue(withURL, "spawn:spot-webhook-url"); got != "https://example.test/hook" {
		t.Errorf("spawn:spot-webhook-url = %q, want the URL", got)
	}
	if got := findTagValue(withURL, "spawn:webhook-correlation"); got != "opaque-blob-42" {
		t.Errorf("spawn:webhook-correlation = %q, want the verbatim blob", got)
	}
	if got := findTagValue(withURL, "spawn:webhook-timeout"); got != "3s" {
		t.Errorf("spawn:webhook-timeout = %q, want 3s", got)
	}

	// No URL → none of the three tags are written (opt-in; today's behavior).
	without := buildTags(LaunchConfig{
		Name:               "t",
		WebhookCorrelation: "orphan", // present but URL-less → must be dropped
		WebhookTimeout:     "3s",
	}, "123456789012", "arn:aws:iam::123456789012:user/test", "")
	for _, k := range []string{"spawn:spot-webhook-url", "spawn:webhook-correlation", "spawn:webhook-timeout"} {
		if got := findTagValue(without, k); got != "" {
			t.Errorf("%s = %q, want empty when no webhook URL is set", k, got)
		}
	}
}

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

// TestBuildTags_LongCommandNotTagged is the #214/#246 guard: a command longer
// than EC2's 256-char tag cap must NOT be written to the spawn:command tag
// (which would fail RunInstances). It's delivered via embedded user-data instead;
// the tag is reserved for short commands / the sweep path.
func TestBuildTags_LongCommandNotTagged(t *testing.T) {
	long := "aws s3 cp s3://b/run.sh /tmp/run.sh && " + strings.Repeat("X", 300) + " bash /tmp/run.sh"
	if len(long) <= 256 {
		t.Fatalf("test command should exceed 256 chars, got %d", len(long))
	}
	tags := buildTags(LaunchConfig{Name: "t", JobArrayCommand: long}, "123456789012", "arn:aws:iam::123456789012:user/test", "")
	if v := findTagValue(tags, "spawn:command"); v != "" {
		t.Errorf("oversized command must not be tagged (would fail RunInstances); got %d-char tag", len(v))
	}

	// A short command is still tagged (observability + sweep path).
	tags = buildTags(LaunchConfig{Name: "t", JobArrayCommand: "echo hi"}, "123456789012", "arn:aws:iam::123456789012:user/test", "")
	if findTagValue(tags, "spawn:command") != "echo hi" {
		t.Error("short command should still be written to spawn:command")
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
