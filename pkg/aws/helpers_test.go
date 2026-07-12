package aws

import (
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func TestDetectArchitecture(t *testing.T) {
	tests := []struct {
		instanceType string
		want         string
	}{
		{"t4g.micro", "arm64"},
		{"m7g.large", "arm64"},
		{"c8g.xlarge", "arm64"},
		{"r8g.2xlarge", "arm64"},
		{"x2gd.large", "arm64"},
		{"g5g.xlarge", "arm64"},
		{"m7i.large", "x86_64"},
		{"c7i.xlarge", "x86_64"},
		{"p5.48xlarge", "x86_64"},
		{"t3.micro", "x86_64"},
		{"unknownformat", "x86_64"}, // no dot → x86_64 default
	}
	for _, tt := range tests {
		if got := DetectArchitecture(tt.instanceType); got != tt.want {
			t.Errorf("DetectArchitecture(%q) = %q, want %q", tt.instanceType, got, tt.want)
		}
	}
}

func TestEstimateVolumeSize(t *testing.T) {
	tests := []struct {
		instanceType string
		want         int32
	}{
		{"t3.micro", 18},     // 8 + 10
		{"m7i.large", 26},    // 16 + 10
		{"r7i.2xlarge", 42},  // 32 + 10
		{"p5.48xlarge", 778}, // 768 + 10
		{"g6.xlarge", 42},    // 32 + 10
		{"zz9.unknown", 20},  // default
	}
	for _, tt := range tests {
		if got := estimateVolumeSize(tt.instanceType); got != tt.want {
			t.Errorf("estimateVolumeSize(%q) = %d, want %d", tt.instanceType, got, tt.want)
		}
	}
}

func TestBuildBlockDevices(t *testing.T) {
	t.Run("default size", func(t *testing.T) {
		bd := buildBlockDevices(LaunchConfig{InstanceType: "m7i.large"}, "", 0)
		if len(bd) != 1 {
			t.Fatalf("expected 1 block device, got %d", len(bd))
		}
		if *bd[0].Ebs.VolumeSize != 20 {
			t.Errorf("default volume size = %d, want 20", *bd[0].Ebs.VolumeSize)
		}
		if *bd[0].Ebs.Encrypted {
			t.Error("default config should not be encrypted")
		}
		if *bd[0].DeviceName != "/dev/xvda" {
			t.Errorf("device name = %q, want /dev/xvda", *bd[0].DeviceName)
		}
	})

	// #284: the root mapping's DeviceName must match the AMI's RootDeviceName,
	// or the size/encryption override lands on a non-root device (Ubuntu/Rocky
	// register /dev/sda1). An empty name falls back to /dev/xvda (Amazon Linux).
	t.Run("root device from AMI (/dev/sda1) carries the size override", func(t *testing.T) {
		bd := buildBlockDevices(LaunchConfig{InstanceType: "m7i.large", RootVolumeSizeGiB: 200}, "/dev/sda1", 0)
		if *bd[0].DeviceName != "/dev/sda1" {
			t.Errorf("root device = %q, want /dev/sda1 (must match the AMI root device, #284)", *bd[0].DeviceName)
		}
		if *bd[0].Ebs.VolumeSize != 200 {
			t.Errorf("root volume size = %d, want 200 (override must apply to the AMI's real root)", *bd[0].Ebs.VolumeSize)
		}
	})

	t.Run("empty root device name falls back to /dev/xvda", func(t *testing.T) {
		bd := buildBlockDevices(LaunchConfig{InstanceType: "m7i.large"}, "", 0)
		if *bd[0].DeviceName != "/dev/xvda" {
			t.Errorf("root device = %q, want /dev/xvda fallback", *bd[0].DeviceName)
		}
	})

	t.Run("explicit size override", func(t *testing.T) {
		bd := buildBlockDevices(LaunchConfig{InstanceType: "m7i.large", RootVolumeSizeGiB: 100}, "", 0)
		if *bd[0].Ebs.VolumeSize != 100 {
			t.Errorf("volume size = %d, want 100", *bd[0].Ebs.VolumeSize)
		}
	})

	t.Run("hibernate sizes from RAM and encrypts", func(t *testing.T) {
		bd := buildBlockDevices(LaunchConfig{InstanceType: "r7i.2xlarge", Hibernate: true}, "", 0)
		if *bd[0].Ebs.VolumeSize != 42 { // estimateVolumeSize(r7i) = 32+10
			t.Errorf("hibernate volume size = %d, want 42", *bd[0].Ebs.VolumeSize)
		}
		if !*bd[0].Ebs.Encrypted {
			t.Error("hibernate config must be encrypted")
		}
	})

	t.Run("encryption with KMS key", func(t *testing.T) {
		bd := buildBlockDevices(LaunchConfig{InstanceType: "m7i.large", EBSEncrypted: true, EBSKMSKeyID: "arn:aws:kms:us-east-1:1:key/abc"}, "", 0)
		if !*bd[0].Ebs.Encrypted {
			t.Error("expected encrypted volume")
		}
		if bd[0].Ebs.KmsKeyId == nil || *bd[0].Ebs.KmsKeyId == "" {
			t.Error("expected KMS key to be set")
		}
	})

	t.Run("KMS key ignored without encryption", func(t *testing.T) {
		bd := buildBlockDevices(LaunchConfig{InstanceType: "m7i.large", EBSKMSKeyID: "arn:aws:kms:us-east-1:1:key/abc"}, "", 0)
		if bd[0].Ebs.KmsKeyId != nil {
			t.Error("KMS key should not be set when encryption is off")
		}
	})

	// #25: the AMI root-snapshot minimum is a hard floor on the volume size.
	t.Run("AMI minimum raises the default", func(t *testing.T) {
		bd := buildBlockDevices(LaunchConfig{InstanceType: "t4g.small"}, "", 80)
		if *bd[0].Ebs.VolumeSize != 80 {
			t.Errorf("volume size = %d, want 80 (AMI snapshot minimum)", *bd[0].Ebs.VolumeSize)
		}
	})

	t.Run("AMI minimum overrides a too-small explicit size", func(t *testing.T) {
		bd := buildBlockDevices(LaunchConfig{InstanceType: "t4g.small", RootVolumeSizeGiB: 20}, "", 40)
		if *bd[0].Ebs.VolumeSize != 40 {
			t.Errorf("volume size = %d, want 40 (snapshot floor beats too-small --volume-size)", *bd[0].Ebs.VolumeSize)
		}
	})

	t.Run("explicit size larger than AMI minimum is kept", func(t *testing.T) {
		bd := buildBlockDevices(LaunchConfig{InstanceType: "t4g.small", RootVolumeSizeGiB: 200}, "", 80)
		if *bd[0].Ebs.VolumeSize != 200 {
			t.Errorf("volume size = %d, want 200 (caller asked for more than the floor)", *bd[0].Ebs.VolumeSize)
		}
	})

	// #144: attached EBS data volumes from snapshots become extra block-device
	// mappings on /dev/sdf, /dev/sdg, … with DeleteOnTermination so they die
	// with the ephemeral instance.
	t.Run("attached volumes append snapshot-backed mappings", func(t *testing.T) {
		bd := buildBlockDevices(LaunchConfig{
			InstanceType: "r7g.2xlarge",
			AttachVolumes: []AttachVolumeSpec{
				{SnapshotID: "snap-aaa", MountPoint: "/opt/databases/kraken2", ReadOnly: true},
				{SnapshotID: "snap-bbb", MountPoint: "/data", SizeGiB: 200},
			},
		}, "", 0)
		if len(bd) != 3 {
			t.Fatalf("expected root + 2 attached = 3 mappings, got %d", len(bd))
		}
		if *bd[0].DeviceName != "/dev/xvda" {
			t.Errorf("root device = %q, want /dev/xvda", *bd[0].DeviceName)
		}
		if *bd[1].DeviceName != "/dev/sdf" || *bd[2].DeviceName != "/dev/sdg" {
			t.Errorf("attached device names = %q,%q, want /dev/sdf,/dev/sdg", *bd[1].DeviceName, *bd[2].DeviceName)
		}
		if bd[1].Ebs.SnapshotId == nil || *bd[1].Ebs.SnapshotId != "snap-aaa" {
			t.Errorf("first attached snapshot = %v, want snap-aaa", bd[1].Ebs.SnapshotId)
		}
		if !*bd[1].Ebs.DeleteOnTermination {
			t.Error("attached volume must set DeleteOnTermination so it dies with the instance")
		}
		if bd[1].Ebs.VolumeSize != nil {
			t.Error("snapshot-backed volume without an explicit size should leave VolumeSize unset (use the snapshot size)")
		}
		if bd[2].Ebs.VolumeSize == nil || *bd[2].Ebs.VolumeSize != 200 {
			t.Errorf("second attached size = %v, want 200", bd[2].Ebs.VolumeSize)
		}
	})

	t.Run("attached volumes inherit EBS encryption + KMS key", func(t *testing.T) {
		bd := buildBlockDevices(LaunchConfig{
			InstanceType:  "m7i.large",
			EBSEncrypted:  true,
			EBSKMSKeyID:   "arn:aws:kms:us-east-1:1:key/abc",
			AttachVolumes: []AttachVolumeSpec{{SnapshotID: "snap-ccc", MountPoint: "/ref"}},
		}, "", 0)
		if !*bd[1].Ebs.Encrypted {
			t.Error("attached volume should be encrypted when EBSEncrypted is set")
		}
		if bd[1].Ebs.KmsKeyId == nil || *bd[1].Ebs.KmsKeyId == "" {
			t.Error("attached volume should carry the KMS key")
		}
	})
}

func TestAttachDeviceName(t *testing.T) {
	cases := map[int]string{0: "/dev/sdf", 1: "/dev/sdg", 4: "/dev/sdj"}
	for i, want := range cases {
		if got := AttachDeviceName(i); got != want {
			t.Errorf("AttachDeviceName(%d) = %q, want %q", i, got, want)
		}
	}
}

func TestRootVolumeSizeFromMappings(t *testing.T) {
	ebs := func(name string, size int32) types.BlockDeviceMapping {
		return types.BlockDeviceMapping{
			DeviceName: aws.String(name),
			Ebs:        &types.EbsBlockDevice{VolumeSize: aws.Int32(size)},
		}
	}

	t.Run("matches root device by name", func(t *testing.T) {
		got := rootVolumeSizeFromMappings("/dev/xvda", []types.BlockDeviceMapping{
			ebs("/dev/xvda", 80),
			ebs("/dev/sdb", 500), // larger data volume must NOT win
		})
		if got != 80 {
			t.Errorf("got %d, want 80 (root device)", got)
		}
	})

	t.Run("falls back to largest when root name not matched", func(t *testing.T) {
		got := rootVolumeSizeFromMappings("/dev/xvda", []types.BlockDeviceMapping{
			ebs("/dev/sdf", 40),
			ebs("/dev/sdg", 120),
		})
		if got != 120 {
			t.Errorf("got %d, want 120 (largest fallback)", got)
		}
	})

	t.Run("ignores mappings without sized EBS", func(t *testing.T) {
		got := rootVolumeSizeFromMappings("", []types.BlockDeviceMapping{
			{DeviceName: aws.String("/dev/sdh")}, // no Ebs
			ebs("/dev/xvda", 30),
		})
		if got != 30 {
			t.Errorf("got %d, want 30", got)
		}
	})

	t.Run("no EBS mappings returns 0", func(t *testing.T) {
		if got := rootVolumeSizeFromMappings("/dev/xvda", nil); got != 0 {
			t.Errorf("got %d, want 0", got)
		}
	})
}

func TestValueOrEmpty(t *testing.T) {
	s := "hello"
	if got := valueOrEmpty(&s); got != "hello" {
		t.Errorf("valueOrEmpty(&\"hello\") = %q, want hello", got)
	}
	if got := valueOrEmpty(nil); got != "" {
		t.Errorf("valueOrEmpty(nil) = %q, want empty", got)
	}
}

func TestContainsAndFindSubstring(t *testing.T) {
	tests := []struct {
		s, substr string
		want      bool
	}{
		{"hello world", "hello", true},
		{"hello world", "world", true},
		{"hello world", "lo wo", true},
		{"hello", "hello", true},
		{"hello", "xyz", false},
		{"hi", "hello", false}, // substr longer than s
		{"", "", true},
	}
	for _, tt := range tests {
		if got := contains(tt.s, tt.substr); got != tt.want {
			t.Errorf("contains(%q, %q) = %v, want %v", tt.s, tt.substr, got, tt.want)
		}
	}
	// findSubstring directly
	if !findSubstring("abcdef", "cde") {
		t.Error("findSubstring should find 'cde' in 'abcdef'")
	}
	if findSubstring("abc", "xyz") {
		t.Error("findSubstring should not find 'xyz' in 'abc'")
	}
}

// --- IAM pure helpers ---

func TestGenerateRoleName_Deterministic(t *testing.T) {
	c := &Client{}
	cfg := IAMRoleConfig{Policies: []string{"s3:ReadOnly"}, ManagedPolicies: []string{"arn:aws:iam::aws:policy/AmazonS3ReadOnlyAccess"}}

	n1 := c.generateRoleName(cfg)
	n2 := c.generateRoleName(cfg)
	if n1 != n2 {
		t.Errorf("generateRoleName not deterministic: %q != %q", n1, n2)
	}
	if !strings.HasPrefix(n1, "spawn-instance-") {
		t.Errorf("role name %q missing expected prefix", n1)
	}
	// Different config → different name.
	other := c.generateRoleName(IAMRoleConfig{Policies: []string{"s3:FullAccess"}})
	if other == n1 {
		t.Error("different configs should produce different role names")
	}
}

func TestHashPolicies(t *testing.T) {
	c := &Client{}
	h := c.hashPolicies(IAMRoleConfig{Policies: []string{"s3:ReadOnly"}})
	if len(h) != 64 { // sha256 hex
		t.Errorf("hash length = %d, want 64", len(h))
	}
}

func TestBuildInlinePolicy(t *testing.T) {
	c := &Client{}
	policy := c.buildInlinePolicy([]string{"s3:ReadOnly"})

	if policy["Version"] != "2012-10-17" {
		t.Errorf("policy Version = %v, want 2012-10-17", policy["Version"])
	}
	stmts, ok := policy["Statement"].([]interface{})
	if !ok {
		t.Fatalf("Statement is not a slice: %T", policy["Statement"])
	}
	// Always includes the 5 spored self-management statements (read / tag-write /
	// destructive-action / dns-invoke / fsx-mount — #174 split tag-write into its
	// own conditioned statement; #173 added the DNS Function URL invoke grant; #221
	// added the FSx mount grant), plus the template's.
	if len(stmts) < 6 {
		t.Errorf("expected >= 6 statements (5 spored + template), got %d", len(stmts))
	}

	// Unknown template names are skipped (no panic, just the spored base).
	base := c.buildInlinePolicy([]string{"doesNotExist"})
	baseStmts := base["Statement"].([]interface{})
	if len(baseStmts) != 5 {
		t.Errorf("unknown template should yield only the 5 spored statements, got %d", len(baseStmts))
	}
}

func TestBuildIAMTags(t *testing.T) {
	c := &Client{}

	tags := c.buildIAMTags(map[string]string{"team": "research"})
	found := map[string]string{}
	for _, tag := range tags {
		found[*tag.Key] = *tag.Value
	}
	if found["spawn:managed"] != "true" {
		t.Error("expected spawn:managed=true tag")
	}
	if found["team"] != "research" {
		t.Error("expected user tag to be preserved")
	}
	if _, ok := found["spawn:created"]; !ok {
		t.Error("expected spawn:created timestamp tag")
	}

	// nil map must not panic and still gets spawn tags.
	nilTags := c.buildIAMTags(nil)
	if len(nilTags) < 2 {
		t.Errorf("expected spawn tags even for nil input, got %d", len(nilTags))
	}
}

// TestPropagatableSnapshotTags verifies the #161 filter: a volume created from a
// snapshot inherits the snapshot's CUSTOM tags but not its Name or spawn:* baseline.
func TestPropagatableSnapshotTags(t *testing.T) {
	in := []types.Tag{
		{Key: aws.String("Name"), Value: aws.String("kraken2-k2pluspf")},
		{Key: aws.String("spawn:managed"), Value: aws.String("true")},
		{Key: aws.String("spawn:source"), Value: aws.String("ebs-direct")},
		{Key: aws.String("project"), Value: aws.String("aws-microbiome-demo")},
		{Key: aws.String("db-version"), Value: aws.String("k2_20260226")},
	}
	out := propagatableSnapshotTags(in)
	got := map[string]string{}
	for _, tg := range out {
		got[*tg.Key] = *tg.Value
	}
	if _, ok := got["Name"]; ok {
		t.Error("Name must not propagate to the volume")
	}
	if _, ok := got["spawn:managed"]; ok {
		t.Error("spawn:* baseline must not propagate")
	}
	if got["project"] != "aws-microbiome-demo" || got["db-version"] != "k2_20260226" {
		t.Errorf("custom tags should propagate; got %v", got)
	}
	if len(got) != 2 {
		t.Errorf("expected exactly the 2 custom tags, got %d: %v", len(got), got)
	}
}
