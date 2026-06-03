package aws

import (
	"strings"
	"testing"
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
		bd := buildBlockDevices(LaunchConfig{InstanceType: "m7i.large"}, 0)
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

	t.Run("explicit size override", func(t *testing.T) {
		bd := buildBlockDevices(LaunchConfig{InstanceType: "m7i.large", RootVolumeSizeGiB: 100}, 0)
		if *bd[0].Ebs.VolumeSize != 100 {
			t.Errorf("volume size = %d, want 100", *bd[0].Ebs.VolumeSize)
		}
	})

	t.Run("hibernate sizes from RAM and encrypts", func(t *testing.T) {
		bd := buildBlockDevices(LaunchConfig{InstanceType: "r7i.2xlarge", Hibernate: true}, 0)
		if *bd[0].Ebs.VolumeSize != 42 { // estimateVolumeSize(r7i) = 32+10
			t.Errorf("hibernate volume size = %d, want 42", *bd[0].Ebs.VolumeSize)
		}
		if !*bd[0].Ebs.Encrypted {
			t.Error("hibernate config must be encrypted")
		}
	})

	t.Run("encryption with KMS key", func(t *testing.T) {
		bd := buildBlockDevices(LaunchConfig{InstanceType: "m7i.large", EBSEncrypted: true, EBSKMSKeyID: "arn:aws:kms:us-east-1:1:key/abc"}, 0)
		if !*bd[0].Ebs.Encrypted {
			t.Error("expected encrypted volume")
		}
		if bd[0].Ebs.KmsKeyId == nil || *bd[0].Ebs.KmsKeyId == "" {
			t.Error("expected KMS key to be set")
		}
	})

	t.Run("KMS key ignored without encryption", func(t *testing.T) {
		bd := buildBlockDevices(LaunchConfig{InstanceType: "m7i.large", EBSKMSKeyID: "arn:aws:kms:us-east-1:1:key/abc"}, 0)
		if bd[0].Ebs.KmsKeyId != nil {
			t.Error("KMS key should not be set when encryption is off")
		}
	})

	// #25: the AMI root-snapshot minimum is a hard floor on the volume size.
	t.Run("AMI minimum raises the default", func(t *testing.T) {
		bd := buildBlockDevices(LaunchConfig{InstanceType: "t4g.small"}, 80)
		if *bd[0].Ebs.VolumeSize != 80 {
			t.Errorf("volume size = %d, want 80 (AMI snapshot minimum)", *bd[0].Ebs.VolumeSize)
		}
	})

	t.Run("AMI minimum overrides a too-small explicit size", func(t *testing.T) {
		bd := buildBlockDevices(LaunchConfig{InstanceType: "t4g.small", RootVolumeSizeGiB: 20}, 40)
		if *bd[0].Ebs.VolumeSize != 40 {
			t.Errorf("volume size = %d, want 40 (snapshot floor beats too-small --volume-size)", *bd[0].Ebs.VolumeSize)
		}
	})

	t.Run("explicit size larger than AMI minimum is kept", func(t *testing.T) {
		bd := buildBlockDevices(LaunchConfig{InstanceType: "t4g.small", RootVolumeSizeGiB: 200}, 80)
		if *bd[0].Ebs.VolumeSize != 200 {
			t.Errorf("volume size = %d, want 200 (caller asked for more than the floor)", *bd[0].Ebs.VolumeSize)
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
	// Always includes the 2 spored self-management statements, plus the template's.
	if len(stmts) < 3 {
		t.Errorf("expected >= 3 statements (2 spored + template), got %d", len(stmts))
	}

	// Unknown template names are skipped (no panic, just the spored base).
	base := c.buildInlinePolicy([]string{"doesNotExist"})
	baseStmts := base["Statement"].([]interface{})
	if len(baseStmts) != 2 {
		t.Errorf("unknown template should yield only 2 spored statements, got %d", len(baseStmts))
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
