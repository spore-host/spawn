package aws

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// spawnAMINamePattern is the name filter for the account's own spawn images.
const spawnAMINamePattern = "spawn-*"

// GetSelfOwnedSpawnAMI returns the newest available AMI that this account owns,
// whose name matches spawn-*, and that can run instanceType: the same CPU
// architecture as the type, a Linux image, and, where the image carries
// spawn:gpu (as `spawn create-ami` writes it), the type's GPU-ness. A GPU type
// only takes an image tagged spawn:gpu=true, since nothing else says the image
// has a driver. It returns "" with no error when no image qualifies, so the
// caller can fall back to GetRecommendedAMI.
//
// `spawn burst` used to take the newest spawn-* image of any architecture or OS
// (#924), so an arm64 or Windows image could be put on an x86 Linux type.
func (c *Client) GetSelfOwnedSpawnAMI(ctx context.Context, region, instanceType string) (string, error) {
	arch := c.resolveArchitecture(ctx, region, instanceType)
	gpu := DetectGPUInstance(instanceType)

	ec2Client := c.regionalEC2(region)
	var images []ec2types.Image
	var next *string
	for {
		out, err := ec2Client.DescribeImages(ctx, &ec2.DescribeImagesInput{
			Owners: []string{"self"},
			Filters: []ec2types.Filter{
				{Name: aws.String("name"), Values: []string{spawnAMINamePattern}},
				{Name: aws.String("state"), Values: []string{"available"}},
			},
			NextToken: next,
		})
		if err != nil {
			return "", fmt.Errorf("describe self-owned spawn AMIs: %w", err)
		}
		images = append(images, out.Images...)
		if out.NextToken == nil || *out.NextToken == "" {
			break
		}
		next = out.NextToken
	}
	return pickSpawnAMI(images, arch, gpu), nil
}

// pickSpawnAMI returns the newest image in images that fits an instance of
// architecture arch ("x86_64" or "arm64") with or without a GPU, or "".
func pickSpawnAMI(images []ec2types.Image, arch string, gpu bool) string {
	var best *ec2types.Image
	for i := range images {
		img := &images[i]
		if img.ImageId == nil || !spawnAMIFits(img, arch, gpu) {
			continue
		}
		if best == nil || aws.ToString(img.CreationDate) > aws.ToString(best.CreationDate) {
			best = img
		}
	}
	if best == nil {
		return ""
	}
	return *best.ImageId
}

func spawnAMIFits(img *ec2types.Image, arch string, gpu bool) bool {
	if string(img.Architecture) != arch {
		return false
	}
	if strings.EqualFold(string(img.Platform), "windows") {
		return false
	}
	tags := map[string]string{}
	for _, t := range img.Tags {
		tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	if os, ok := tags["spawn:os"]; ok && !strings.EqualFold(os, "linux") {
		return false
	}
	imgGPU, tagged := tags["spawn:gpu"]
	if gpu {
		return tagged && imgGPU == "true"
	}
	return !tagged || imgGPU != "true"
}
