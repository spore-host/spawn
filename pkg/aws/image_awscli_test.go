package aws

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/spore-host/spawn/pkg/egressproxy"
)

// #1515 phase 7: the #1494 regional mirror of the AWS CLI is retired, so a
// Linux bootstrap on an image with no aws CLI downloads it from
// awscli.amazonaws.com. For a --no-public-ip launch with no NAT that is an
// internet need, met only through the opt-in egress proxy; Amazon's Amazon
// Linux ships the CLI and needs nothing.

func imageXML(name, desc, ownerAlias string) string {
	x := `<item><imageId>ami-0123456789abcdef0</imageId><name>` + name + `</name><description>` + desc + `</description>` +
		`<rootDeviceName>/dev/xvda</rootDeviceName><platformDetails>Linux/UNIX</platformDetails>`
	if ownerAlias != "" {
		x += `<imageOwnerAlias>` + ownerAlias + `</imageOwnerAlias>`
	}
	return x + `</item>`
}

var (
	al2023ImageXML = imageXML("al2023-ami-2023.9.20261020.0-kernel-6.1-x86_64", "Amazon Linux 2023 AMI 2023.9.20261020.0 x86_64 HVM kernel-6.1", "amazon")
	ubuntuImageXML = imageXML("ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-20261015",
		"Canonical, Ubuntu, 24.04, amd64 noble image", "")
)

func TestImageShipsAWSCLI(t *testing.T) {
	for name, tc := range map[string]struct {
		name, desc, alias string
		ships             bool
	}{
		"AL2023":                  {"al2023-ami-2023.9.20261020.0-kernel-6.1-x86_64", "Amazon Linux 2023 AMI", "amazon", true},
		"AL2":                     {"amzn2-ami-kernel-5.10-hvm-2.0.20261015.0-x86_64-gp2", "Amazon Linux 2 Kernel 5.10 AMI", "amazon", true},
		"the AL2023 NVIDIA DLAMI": {"Deep Learning Base OSS Nvidia Driver GPU AMI (Amazon Linux 2023) 20261020", "Supported EC2 instances: G4dn, G5, G6", "amazon", true},
		"EKS-optimized AL2023":    {"amazon-eks-node-al2023-x86_64-standard-1.33-v20261015", "EKS-optimized Kubernetes node based on Amazon Linux 2023", "amazon", true},
		"Canonical's Ubuntu":      {"ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-20261015", "Canonical, Ubuntu, 24.04", "", false},
		"Amazon's Ubuntu DLAMI":   {"Deep Learning Base OSS Nvidia Driver GPU AMI (Ubuntu 22.04) 20261020", "Supported EC2 instances: G4dn", "amazon", false},
		"an AL name not Amazon's": {"al2023-ami-2023.9.20261020.0-kernel-6.1-x86_64", "Amazon Linux 2023 AMI", "", false},
		"RHEL":                    {"RHEL-9.6.0_HVM-20261015-x86_64-0-Hourly2-GP3", "Provided by Red Hat, Inc.", "", false},
		"a spawn-made image":      {"spawn-my-env-20261020", "built from al2023", "", false},
		"Amazon's Windows":        {"Windows_Server-2025-English-Full-Base-2026.10.15", "Microsoft Windows Server 2025", "amazon", false},
	} {
		img := types.Image{Name: aws.String(tc.name), Description: aws.String(tc.desc)}
		if tc.alias != "" {
			img.ImageOwnerAlias = aws.String(tc.alias)
		}
		if got := imageShipsAWSCLI(img); got != tc.ships {
			t.Errorf("%s: imageShipsAWSCLI = %v, want %v", name, got, tc.ships)
		}
	}
}

// withPublicSubnet adds a subnet routed to an internet gateway to f's VPC,
// where the egress proxy can run.
func withPublicSubnet(f *fakeEC2) *fakeEC2 {
	f.subnets = append(f.subnets, fakeSubnet{id: "subnet-pub", vpc: "vpc-1", az: "us-west-2b", free: 200})
	f.routeTablesXML += `<item><routeTableId>rtb-pub</routeTableId><vpcId>vpc-1</vpcId><routeSet>` +
		`<item><destinationCidrBlock>0.0.0.0/0</destinationCidrBlock><gatewayId>igw-1</gatewayId><state>active</state></item>` +
		`</routeSet><associationSet><item><routeTableAssociationId>rtbassoc-2</routeTableAssociationId>` +
		`<routeTableId>rtb-pub</routeTableId><subnetId>subnet-pub</subnetId><main>false</main></item></associationSet></item>`
	return f
}

func imageCfg(egress string) LaunchConfig {
	cfg := privateCfg
	cfg.AMI = "ami-0123456789abcdef0"
	cfg.BootstrapEgress = egress
	return cfg
}

// The gate: a no-NAT --no-public-ip launch of Ubuntu, which has no aws CLI,
// is refused without the opt-in, naming the AWS CLI download and the flag;
// with it, the need is met through the proxy and recorded as such. AL2023
// needs nothing either way.
func TestPreflightNoPublicIPAWSCLIDownload(t *testing.T) {
	if !egressproxy.Allowed(awsCLIDownloadHost) {
		t.Fatalf("%s is not on egressproxy.Allowlist: the proxy could not meet the need", awsCLIDownloadHost)
	}
	run := func(image, egress string) (PrivateCheck, error) {
		f := withPublicSubnet(privateFake(allEndpointsXML(), ""))
		f.imagesXML = image
		c := newFakeEC2Client(t, f)
		cfg := imageCfg(egress)
		return c.PreflightNoPublicIP(context.Background(), &cfg, nil, io.Discard)
	}

	check, err := run(ubuntuImageXML, "")
	if !errors.Is(err, ErrPrivateNetworkIncomplete) {
		t.Fatalf("Ubuntu without NAT or opt-in: err = %v, want the refusal", err)
	}
	for _, want := range []string{"AWS CLI download", "(" + awsCLIDownloadHost + ")", "Pass " + BootstrapEgressFlag, "pass " + BootstrapEgressFlag + " for the internet needs marked with it"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q:\n%v", want, err)
		}
	}
	if len(check.ViaEgressProxy) != 0 {
		t.Errorf("not opted in, yet ViaEgressProxy = %q", check.ViaEgressProxy)
	}

	check, err = run(ubuntuImageXML, BootstrapEgressProxy)
	if err != nil {
		t.Fatalf("Ubuntu opted in to the proxy: %v", err)
	}
	if len(check.ViaEgressProxy) != 1 || InternetNeedHost(check.ViaEgressProxy[0]) != awsCLIDownloadHost {
		t.Errorf("ViaEgressProxy = %q, want the AWS CLI download alone", check.ViaEgressProxy)
	}
	met := strings.Join(check.Met, "\n")
	if !strings.Contains(met, "AWS CLI download") || !strings.Contains(met, "through the spawn egress proxy in vpc-1") {
		t.Errorf("met = %q, want the AWS CLI download through the proxy", check.Met)
	}
	// Opted in, it still needs a proxy address before it may launch.
	cfg := imageCfg(BootstrapEgressProxy)
	if err := check.requireEgressProxy(cfg); !errors.Is(err, ErrNoEgressProxy) {
		t.Errorf("requireEgressProxy with no proxy address = %v, want ErrNoEgressProxy", err)
	}

	for _, egress := range []string{"", BootstrapEgressProxy} {
		check, err = run(al2023ImageXML, egress)
		if err != nil {
			t.Errorf("AL2023 (egress %q): %v", egress, err)
		}
		if len(check.ViaEgressProxy) != 0 || strings.Contains(strings.Join(check.Met, "\n"), awsCLIDownloadHost) {
			t.Errorf("AL2023 (egress %q) counts an AWS CLI download: met %q, via proxy %q", egress, check.Met, check.ViaEgressProxy)
		}
	}
}

// An image spawn cannot read counts as one with no aws CLI, and says so.
func TestPreflightNoPublicIPUnreadableImageNeedsTheDownload(t *testing.T) {
	f := privateFake(allEndpointsXML(), "")
	f.imagesXML = "" // DescribeImages refused
	c := newFakeEC2Client(t, f)
	cfg := imageCfg("")
	var out bytes.Buffer
	_, err := c.PreflightNoPublicIP(context.Background(), &cfg, nil, &out)
	if !errors.Is(err, ErrPrivateNetworkIncomplete) || !strings.Contains(err.Error(), awsCLIDownloadHost) {
		t.Fatalf("err = %v, want the refusal naming %s", err, awsCLIDownloadHost)
	}
	if !strings.Contains(out.String(), "could not read image ami-0123456789abcdef0 (ec2:DescribeImages") {
		t.Errorf("the preflight does not say it could not read the image:\n%s", out.String())
	}
}

// With a NAT route everything is met: the image is not even read. A repeat
// check reads it once.
func TestPreflightNoPublicIPReadsTheImageOnlyWithoutNAT(t *testing.T) {
	nat := privateFake("", natRouteXML)
	nat.imagesXML = ubuntuImageXML
	c := newFakeEC2Client(t, nat)
	cfg := imageCfg("")
	if _, err := c.PreflightNoPublicIP(context.Background(), &cfg, nil, io.Discard); err != nil {
		t.Fatalf("Ubuntu behind a NAT route: %v", err)
	}
	if n := countAction(nat, "DescribeImages"); n != 0 {
		t.Errorf("%d DescribeImages calls behind a NAT route, want 0", n)
	}

	f := privateFake(allEndpointsXML(), "")
	c = newFakeEC2Client(t, f)
	for i := 0; i < 3; i++ {
		cfg := imageCfg("")
		if _, err := c.PreflightNoPublicIP(context.Background(), &cfg, nil, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	if n := countAction(f, "DescribeImages"); n != 1 {
		t.Errorf("%d DescribeImages calls for one AMI checked three times, want 1", n)
	}
}

// Through Launch: the Ubuntu refusal comes before RunInstances.
func TestLaunch_NoPublicIPUbuntuWithoutNATIsRefused(t *testing.T) {
	f := privateFake(allEndpointsXML(), "")
	f.imagesXML = ubuntuImageXML
	c := newFakeEC2Client(t, f)
	lc := LaunchConfig{NoPublicIP: true, SubnetID: "subnet-1", InstanceType: "m8a.4xlarge", Region: pnRegion,
		AMI: "ami-0123456789abcdef0", PricePerHour: 1}
	if _, err := c.Launch(context.Background(), lc); !errors.Is(err, ErrPrivateNetworkIncomplete) || !strings.Contains(err.Error(), awsCLIDownloadHost) {
		t.Fatalf("err = %v, want the refusal naming the AWS CLI download", err)
	}
	if f.request("RunInstances") != nil {
		t.Error("RunInstances was called for a refused launch")
	}
}

// --- spawn create-ami images (#1515): judged by the image they were built from ---

// imageAPI is a DescribeImages stand-in over images by ID in one region,
// recording every (region, ID) it is asked for.
type imageAPI struct {
	region string
	images map[string]types.Image // "<region>|<id>"
	calls  *[]string
}

func (a imageAPI) DescribeImages(_ context.Context, in *ec2.DescribeImagesInput, _ ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error) {
	id := in.ImageIds[0]
	*a.calls = append(*a.calls, a.region+"|"+id)
	img, ok := a.images[a.region+"|"+id]
	if !ok {
		return nil, fmt.Errorf("InvalidAMIID.NotFound: %s", id)
	}
	return &ec2.DescribeImagesOutput{Images: []types.Image{img}}, nil
}

func amazonAL2023(id string) types.Image {
	return types.Image{ImageId: aws.String(id), ImageOwnerAlias: aws.String("amazon"),
		Name: aws.String("al2023-ami-2023.9.20261020.0-kernel-6.1-x86_64"), Description: aws.String("Amazon Linux 2023 AMI")}
}

func canonicalUbuntu(id string) types.Image {
	return types.Image{ImageId: aws.String(id), Name: aws.String("ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-20261015")}
}

// spawnBuilt is an image `spawn create-ami` made from an instance of base,
// with the tags cmd/create_ami.go writes; region "" omits spawn:source-region.
func spawnBuilt(id, base, region string) types.Image {
	tags := []types.Tag{
		{Key: aws.String("spawn:managed"), Value: aws.String("true")},
		{Key: aws.String("spawn:created-from"), Value: aws.String("i-0abc")},
	}
	if base != "" {
		tags = append(tags, types.Tag{Key: aws.String(createAMIBaseTag), Value: aws.String(base)})
	}
	if region != "" {
		tags = append(tags, types.Tag{Key: aws.String(createAMIRegionTag), Value: aws.String(region)})
	}
	return types.Image{ImageId: aws.String(id), Name: aws.String("spawn-my-env-20261020"), Tags: tags}
}

func TestSpawnBuiltImageIsJudgedByItsBase(t *testing.T) {
	for name, tc := range map[string]struct {
		images    map[string]types.Image
		needs     bool
		wantCalls []string
		why       string
	}{
		"built from AL2023": {
			images:    map[string]types.Image{"us-west-2|ami-0000000000000a001": spawnBuilt("ami-0000000000000a001", "ami-0000000000000a002", ""), "us-west-2|ami-0000000000000a002": amazonAL2023("ami-0000000000000a002")},
			wantCalls: []string{"us-west-2|ami-0000000000000a001", "us-west-2|ami-0000000000000a002"},
		},
		"built from AL2023 in its source region": {
			images:    map[string]types.Image{"us-west-2|ami-0000000000000a001": spawnBuilt("ami-0000000000000a001", "ami-0000000000000a002", "eu-west-1"), "eu-west-1|ami-0000000000000a002": amazonAL2023("ami-0000000000000a002")},
			wantCalls: []string{"us-west-2|ami-0000000000000a001", "eu-west-1|ami-0000000000000a002"},
		},
		"two spawn-built hops to AL2023": {
			images: map[string]types.Image{"us-west-2|ami-0000000000000a001": spawnBuilt("ami-0000000000000a001", "ami-0000000000000a004", ""),
				"us-west-2|ami-0000000000000a004": spawnBuilt("ami-0000000000000a004", "ami-0000000000000a002", ""), "us-west-2|ami-0000000000000a002": amazonAL2023("ami-0000000000000a002")},
			wantCalls: []string{"us-west-2|ami-0000000000000a001", "us-west-2|ami-0000000000000a004", "us-west-2|ami-0000000000000a002"},
		},
		"built from Ubuntu": {
			images:    map[string]types.Image{"us-west-2|ami-0000000000000a001": spawnBuilt("ami-0000000000000a001", "ami-0000000000000a003", ""), "us-west-2|ami-0000000000000a003": canonicalUbuntu("ami-0000000000000a003")},
			needs:     true,
			why:       "is a spawn-built image whose base ami-0000000000000a003 is not Amazon's Amazon Linux",
			wantCalls: []string{"us-west-2|ami-0000000000000a001", "us-west-2|ami-0000000000000a003"},
		},
		"no base tag": {
			images:    map[string]types.Image{"us-west-2|ami-0000000000000a001": spawnBuilt("ami-0000000000000a001", "", "")},
			needs:     true,
			why:       "is not Amazon's Amazon Linux",
			wantCalls: []string{"us-west-2|ami-0000000000000a001"},
		},
		"a base tag without spawn:created-from is not create-ami's": {
			images: map[string]types.Image{"us-west-2|ami-0000000000000a001": {ImageId: aws.String("ami-0000000000000a001"), Name: aws.String("custom"),
				Tags: []types.Tag{{Key: aws.String(createAMIBaseTag), Value: aws.String("ami-0000000000000a002")}}}, "us-west-2|ami-0000000000000a002": amazonAL2023("ami-0000000000000a002")},
			needs:     true,
			wantCalls: []string{"us-west-2|ami-0000000000000a001"},
		},
		"the base cannot be read (deregistered)": {
			images:    map[string]types.Image{"us-west-2|ami-0000000000000a001": spawnBuilt("ami-0000000000000a001", "ami-0000000000000a007", "")},
			needs:     true,
			why:       "is a spawn-built image whose base ami-0000000000000a007 could not be read",
			wantCalls: []string{"us-west-2|ami-0000000000000a001", "us-west-2|ami-0000000000000a007"},
		},
		"a chain deeper than the bound": {
			images: map[string]types.Image{"us-west-2|ami-0000000000000a001": spawnBuilt("ami-0000000000000a001", "ami-0000000000000a005", ""),
				"us-west-2|ami-0000000000000a005": spawnBuilt("ami-0000000000000a005", "ami-0000000000000a006", ""), "us-west-2|ami-0000000000000a006": spawnBuilt("ami-0000000000000a006", "ami-0000000000000a002", ""),
				"us-west-2|ami-0000000000000a002": amazonAL2023("ami-0000000000000a002")},
			needs:     true,
			why:       "more than 3 deep",
			wantCalls: []string{"us-west-2|ami-0000000000000a001", "us-west-2|ami-0000000000000a005", "us-west-2|ami-0000000000000a006"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			var calls []string
			api := func(region string) describeImagesAPI {
				return imageAPI{region: region, images: tc.images, calls: &calls}
			}
			v := describeAWSCLINeed(context.Background(), api, "us-west-2", "ami-0000000000000a001")
			if (v.need != "") != tc.needs {
				t.Errorf("need = %q, want a need %v", v.need, tc.needs)
			}
			if tc.why != "" && !strings.Contains(v.need, tc.why) {
				t.Errorf("need = %q, want it to say %q", v.need, tc.why)
			}
			if v.need != "" {
				for _, w := range []string{"a NAT route", BootstrapEgressFlag} {
					if !strings.Contains(v.need, w) {
						t.Errorf("need %q does not name %q", v.need, w)
					}
				}
				if InternetNeedHost(v.need) != awsCLIDownloadHost {
					t.Errorf("need %q names host %q, want %s", v.need, InternetNeedHost(v.need), awsCLIDownloadHost)
				}
			}
			if strings.Join(calls, " ") != strings.Join(tc.wantCalls, " ") {
				t.Errorf("DescribeImages calls %v, want %v", calls, tc.wantCalls)
			}
		})
	}
}

// Through the wire: a spawn-built AL2023 image launches with no NAT and no
// opt-in, reading the image and its base once each across repeated checks.
func TestPreflightNoPublicIPSpawnBuiltALImageNeedsNothing(t *testing.T) {
	f := privateFake(allEndpointsXML(), "")
	f.imagesXML = ""
	f.imagesByID = map[string]string{
		"ami-0123456789abcdef0": `<item><imageId>ami-0123456789abcdef0</imageId><name>spawn-my-env-20261020</name>` +
			`<tagSet><item><key>spawn:created-from</key><value>i-0abc</value></item>` +
			`<item><key>spawn:base-ami</key><value>ami-0aaaaaaaaaaaaaaaa</value></item></tagSet></item>`,
		"ami-0aaaaaaaaaaaaaaaa": strings.Replace(al2023ImageXML, "ami-0123456789abcdef0", "ami-0aaaaaaaaaaaaaaaa", 1),
	}
	c := newFakeEC2Client(t, f)
	for i := 0; i < 2; i++ {
		cfg := imageCfg("")
		check, err := c.PreflightNoPublicIP(context.Background(), &cfg, nil, io.Discard)
		if err != nil {
			t.Fatalf("a spawn-built AL2023 image was refused: %v", err)
		}
		if len(check.ViaEgressProxy) != 0 {
			t.Errorf("ViaEgressProxy = %q", check.ViaEgressProxy)
		}
	}
	if n := countAction(f, "DescribeImages"); n != 2 {
		t.Errorf("%d DescribeImages calls, want 2 (the image and its base, once)", n)
	}

	// Its base Ubuntu instead: refused, naming the flag and NAT.
	f.imagesByID["ami-0aaaaaaaaaaaaaaaa"] = strings.Replace(ubuntuImageXML, "ami-0123456789abcdef0", "ami-0aaaaaaaaaaaaaaaa", 1)
	c = newFakeEC2Client(t, f)
	cfg := imageCfg("")
	_, err := c.PreflightNoPublicIP(context.Background(), &cfg, nil, io.Discard)
	if !errors.Is(err, ErrPrivateNetworkIncomplete) || !strings.Contains(err.Error(), "base ami-0aaaaaaaaaaaaaaaa is not Amazon's Amazon Linux") ||
		!strings.Contains(err.Error(), "a NAT route or "+BootstrapEgressFlag+" reaches it") {
		t.Errorf("err = %v, want the refusal naming the base, NAT and %s", err, BootstrapEgressFlag)
	}
}
