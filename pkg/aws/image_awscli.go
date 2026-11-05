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
// so spawn does not guess. The one exception is an image `spawn create-ami`
// made, which records the image it was built from: it counts as Amazon Linux
// when that base does (describeAWSCLINeed). An image spawn cannot read at all counts as
// needing it too: refusing a launch the user can opt in past is the safe
// side, where letting one through would leave an instance whose bootstrap
// stops before spored starts.

// awsCLIDownloadHost is the host the Linux bootstrap downloads the AWS CLI
// from on an image with none.
const awsCLIDownloadHost = "awscli.amazonaws.com"

// awsCLINeed is the internet need of a Linux launch of image ami, for why.
// It names both ways to meet it; the host stays last, in parentheses, for
// [InternetNeedHost].
func awsCLINeed(ami, why string) string {
	return fmt.Sprintf("the Linux bootstrap's AWS CLI download, as %s %s; a NAT route or %s reaches it (%s)",
		orImage(ami), why, BootstrapEgressFlag, awsCLIDownloadHost)
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
	note string // a line for the preflight output, when spawn could not read an image
}

// Tags `spawn create-ami` writes on the image it makes (cmd/create_ami.go):
// the AMI the source instance was launched from, and that instance's region.
const (
	createAMIBaseTag   = "spawn:base-ami"
	createAMIRegionTag = "spawn:source-region"
	createAMIFromTag   = "spawn:created-from"
)

// maxBaseAMIHops bounds how far a chain of spawn-built images (one made from
// an instance launched from another) is followed back to its base.
const maxBaseAMIHops = 3

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
	v := describeAWSCLINeed(ctx, func(region string) describeImagesAPI { return c.regionalEC2(region) }, cfg.Region, cfg.AMI)
	c.imageCLI.Store(key, v)
	return v
}

// describeAWSCLINeed decides for ami in region. An image `spawn create-ami`
// made (tagged spawn:base-ami and spawn:created-from) ships the AWS CLI when
// the image it was built from does: an instance of Amazon's Amazon Linux had
// the CLI, and the snapshot keeps it. That base is read with the same rule,
// in spawn:source-region when the tag names one (an image `spawn ami copy`
// moved keeps its tags), one more ec2:DescribeImages call per hop. A missing
// or unreadable base keeps the image counted as needing the download.
func describeAWSCLINeed(ctx context.Context, api func(region string) describeImagesAPI, region, ami string) awsCLIImageVerdict {
	if ami == "" {
		return awsCLIImageVerdict{need: awsCLINeed(ami, "is not named yet, so spawn cannot tell whether it has an aws CLI")}
	}
	top := ami
	for hop := 0; ; hop++ {
		img, err := describeOneImage(ctx, api(region), ami)
		if err != nil {
			what := "could not be read"
			if ami != top {
				what = "is a spawn-built image whose base " + ami + " could not be read"
			}
			return awsCLIImageVerdict{
				need: awsCLINeed(top, what+", so spawn cannot tell whether it has an aws CLI"),
				note: fmt.Sprintf("could not read image %s (ec2:DescribeImages: %v), so %s is counted as an image with no aws CLI", ami, err, orImage(top)),
			}
		}
		switch {
		case strings.EqualFold(string(img.Platform), "windows"), imageShipsAWSCLI(img):
			return awsCLIImageVerdict{}
		}
		base, baseRegion := spawnBuiltBase(img)
		switch {
		case base == "" && ami == top:
			return awsCLIImageVerdict{need: awsCLINeed(top, "is not Amazon's Amazon Linux and may have no aws CLI (stock Ubuntu has none)")}
		case base == "":
			return awsCLIImageVerdict{need: awsCLINeed(top, "is a spawn-built image whose base "+ami+" is not Amazon's Amazon Linux, so it may have no aws CLI")}
		case hop+1 >= maxBaseAMIHops:
			return awsCLIImageVerdict{need: awsCLINeed(top, fmt.Sprintf("is built from spawn-built images more than %d deep, so spawn did not follow it to its base", maxBaseAMIHops))}
		}
		ami = base
		if baseRegion != "" {
			region = baseRegion
		}
	}
}

// spawnBuiltBase is the image img was built from, and its region, when `spawn
// create-ami` made img; "" otherwise.
func spawnBuiltBase(img types.Image) (ami, region string) {
	tags := map[string]string{}
	for _, t := range img.Tags {
		tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	base := tags[createAMIBaseTag]
	if tags[createAMIFromTag] == "" || !strings.HasPrefix(base, "ami-") || base == aws.ToString(img.ImageId) {
		return "", ""
	}
	r := tags[createAMIRegionTag]
	if !validRegion(r) {
		r = ""
	}
	return base, r
}

func describeOneImage(ctx context.Context, api describeImagesAPI, ami string) (types.Image, error) {
	out, err := api.DescribeImages(ctx, &ec2.DescribeImagesInput{ImageIds: []string{ami}})
	if err != nil {
		return types.Image{}, err
	}
	if len(out.Images) == 0 {
		return types.Image{}, fmt.Errorf("not found")
	}
	return out.Images[0], nil
}
