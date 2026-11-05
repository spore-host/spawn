package aws

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// The Linux bootstrap installs the pinned AWS CLI from awscli.amazonaws.com on
// an image that has none (spawn_need_aws, #1490). spawn does not mirror
// third-party artifacts (#1515; the #1494 regional mirror is retired), so for
// a --no-public-ip launch that download is an internet need: met by a NAT
// route, or by the spawn egress proxy (--bootstrap-egress proxy), whose
// allowlist carries awscli.amazonaws.com. It is a need only when the image
// may have no aws CLI.
//
// How spawn tells, from the one DescribeImages call a launch already makes
// (ec2:DescribeImages is in every launch's IAM): an image ships the AWS CLI
// only when Amazon publishes it (ImageOwnerAlias "amazon", which no other
// account can carry) AND its name or description says Amazon Linux, as
// AL2023 (al2023-ami-*), AL2 (amzn2-ami-*), the Amazon Linux DLAMIs and
// ECS/EKS-optimized images do. Every other Linux image counts as needing the
// download: stock Ubuntu, RHEL, Debian and SUSE ship no aws CLI, and what a
// custom or marketplace image has installed is not visible from its metadata,
// so spawn does not guess. An image spawn cannot read at all counts as
// needing it too: refusing a launch the user can opt in past is the safe
// side, where letting one through would leave an instance whose bootstrap
// stops before spored starts.

// awsCLIDownloadHost is the host the Linux bootstrap downloads the AWS CLI
// from on an image with none.
const awsCLIDownloadHost = "awscli.amazonaws.com"

// awsCLINeed is the internet need of a Linux launch of image ami, for why.
func awsCLINeed(ami, why string) string {
	return fmt.Sprintf("the Linux bootstrap's AWS CLI download, as %s %s (%s)", orImage(ami), why, awsCLIDownloadHost)
}

func orImage(ami string) string {
	if ami == "" {
		return "the image"
	}
	return "image " + ami
}

// imageShipsAWSCLI reports whether img is an image the AWS CLI comes on:
// Amazon's own Amazon Linux.
func imageShipsAWSCLI(img types.Image) bool {
	if aws.ToString(img.ImageOwnerAlias) != "amazon" {
		return false
	}
	name := strings.ToLower(aws.ToString(img.Name))
	desc := strings.ToLower(aws.ToString(img.Description))
	if strings.Contains(name, "ubuntu") || strings.Contains(desc, "ubuntu") {
		return false
	}
	for _, p := range []string{"al2023-ami-", "amzn2-ami-", "amzn-ami-"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return strings.Contains(name, "amazon linux") || strings.Contains(desc, "amazon linux")
}

// awsCLIImageVerdict is what imageAWSCLINeed found for one AMI.
type awsCLIImageVerdict struct {
	need string // "" when the image ships the AWS CLI
	note string // a line for the preflight output, when spawn could not read the image
}

// imageAWSCLINeed is the AWS CLI internet need of a launch of cfg, or ""
// when it has none: a Windows launch (its own need is privateInternetNeeds')
// or an image that ships the CLI. Cached per region and AMI on the client.
func (c *Client) imageAWSCLINeed(ctx context.Context, cfg LaunchConfig) awsCLIImageVerdict {
	if strings.EqualFold(cfg.TargetOS, "windows") {
		return awsCLIImageVerdict{}
	}
	key := cfg.Region + "|" + cfg.AMI
	if v, ok := c.imageCLI.Load(key); ok {
		return v.(awsCLIImageVerdict)
	}
	v := describeAWSCLINeed(ctx, c.regionalEC2(cfg.Region), cfg.AMI)
	c.imageCLI.Store(key, v)
	return v
}

func describeAWSCLINeed(ctx context.Context, api describeImagesAPI, ami string) awsCLIImageVerdict {
	if ami == "" {
		return awsCLIImageVerdict{need: awsCLINeed(ami, "is not named yet, so spawn cannot tell whether it has an aws CLI")}
	}
	out, err := api.DescribeImages(ctx, &ec2.DescribeImagesInput{ImageIds: []string{ami}})
	if err != nil || len(out.Images) == 0 {
		why := "not found"
		if err != nil {
			why = err.Error()
		}
		return awsCLIImageVerdict{
			need: awsCLINeed(ami, "could not be read, so spawn cannot tell whether it has an aws CLI"),
			note: fmt.Sprintf("could not read %s (ec2:DescribeImages: %s), so it is counted as an image with no aws CLI", orImage(ami), why),
		}
	}
	img := out.Images[0]
	switch {
	case strings.EqualFold(string(img.Platform), "windows"):
		return awsCLIImageVerdict{}
	case imageShipsAWSCLI(img):
		return awsCLIImageVerdict{}
	}
	return awsCLIImageVerdict{need: awsCLINeed(ami, "is not Amazon's Amazon Linux and may have no aws CLI (stock Ubuntu has none)")}
}
