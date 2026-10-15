package aws

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spore-host/spawn/pkg/testutil"
)

// #924: `spawn burst` auto-detect took the newest self-owned spawn-* AMI with no
// check of architecture or OS. The selector must only return an image the
// instance type can boot with the Linux bootstrap: the type's architecture, not
// Windows, and a GPU image only for a GPU type. Otherwise "" so the caller falls
// back to the standard AL2023 resolution.

func img(id, created string, arch ec2types.ArchitectureValues, platform ec2types.PlatformValues, tags map[string]string) ec2types.Image {
	im := ec2types.Image{
		ImageId:      aws.String(id),
		CreationDate: aws.String(created),
		Architecture: arch,
		Platform:     platform,
	}
	for k, v := range tags {
		im.Tags = append(im.Tags, ec2types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return im
}

func TestPickSpawnAMI(t *testing.T) {
	const (
		x86 = ec2types.ArchitectureValuesX8664
		arm = ec2types.ArchitectureValuesArm64
	)
	old := "2026-01-01T00:00:00.000Z"
	mid := "2026-05-01T00:00:00.000Z"
	newest := "2026-09-01T00:00:00.000Z"

	cases := []struct {
		name   string
		images []ec2types.Image
		arch   string
		gpu    bool
		want   string
	}{
		{"newest matching arch wins over a newer other-arch image",
			[]ec2types.Image{img("ami-x86-old", old, x86, "", nil), img("ami-x86", mid, x86, "", nil), img("ami-arm", newest, arm, "", nil)},
			"x86_64", false, "ami-x86"},
		{"arm64 type gets the arm64 image",
			[]ec2types.Image{img("ami-x86", newest, x86, "", nil), img("ami-arm", old, arm, "", nil)},
			"arm64", false, "ami-arm"},
		{"no image of the type's architecture falls back",
			[]ec2types.Image{img("ami-arm", newest, arm, "", nil)},
			"x86_64", false, ""},
		{"a Windows image is never chosen",
			[]ec2types.Image{img("ami-win", newest, x86, ec2types.PlatformValuesWindows, nil), img("ami-lin", old, x86, "", nil)},
			"x86_64", false, "ami-lin"},
		{"an image tagged spawn:os=windows is never chosen",
			[]ec2types.Image{img("ami-win", newest, x86, "", map[string]string{"spawn:os": "windows"})},
			"x86_64", false, ""},
		{"spawn:os=linux is fine",
			[]ec2types.Image{img("ami-lin", newest, x86, "", map[string]string{"spawn:os": "linux"})},
			"x86_64", false, "ami-lin"},
		{"a GPU type takes only a spawn:gpu=true image",
			[]ec2types.Image{img("ami-cpu", newest, x86, "", map[string]string{"spawn:gpu": "false"}), img("ami-untagged", newest, x86, "", nil), img("ami-gpu", old, x86, "", map[string]string{"spawn:gpu": "true"})},
			"x86_64", true, "ami-gpu"},
		{"a GPU type with no GPU image falls back",
			[]ec2types.Image{img("ami-untagged", newest, x86, "", nil)},
			"x86_64", true, ""},
		{"a CPU type skips a GPU image",
			[]ec2types.Image{img("ami-gpu", newest, x86, "", map[string]string{"spawn:gpu": "true"}), img("ami-cpu", old, x86, "", map[string]string{"spawn:gpu": "false"})},
			"x86_64", false, "ami-cpu"},
		{"nothing at all", nil, "x86_64", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pickSpawnAMI(tc.images, tc.arch, tc.gpu); got != tc.want {
				t.Errorf("pickSpawnAMI = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestGetSelfOwnedSpawnAMIMatchesTheInstanceType runs the real DescribeImages
// path against Substrate: a newer arm64 spawn image must not be handed to an
// x86 type, and an arm64 type gets it.
func TestGetSelfOwnedSpawnAMIMatchesTheInstanceType(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	ec2Client := env.EC2Client()

	register := func(name string, arch ec2types.ArchitectureValues) string {
		out, err := ec2Client.RegisterImage(ctx, &ec2.RegisterImageInput{
			Name:           aws.String(name),
			Architecture:   arch,
			RootDeviceName: aws.String("/dev/xvda"),
		})
		if err != nil {
			t.Fatalf("RegisterImage %s: %v", name, err)
		}
		return aws.ToString(out.ImageId)
	}
	x86 := register("spawn-burst-x86", ec2types.ArchitectureValuesX8664)
	arm := register("spawn-burst-arm", ec2types.ArchitectureValuesArm64)

	client := NewClientFromConfig(env.AWSConfig)
	got, err := client.GetSelfOwnedSpawnAMI(ctx, "us-east-1", "t3.micro")
	if err != nil {
		t.Fatalf("GetSelfOwnedSpawnAMI(t3.micro): %v", err)
	}
	if got != x86 {
		t.Errorf("t3.micro (x86_64) got %q, want the x86_64 image %q (arm64 is %q)", got, x86, arm)
	}
	got, err = client.GetSelfOwnedSpawnAMI(ctx, "us-east-1", "t4g.micro")
	if err != nil {
		t.Fatalf("GetSelfOwnedSpawnAMI(t4g.micro): %v", err)
	}
	if got != arm {
		t.Errorf("t4g.micro (arm64) got %q, want the arm64 image %q", got, arm)
	}
}
