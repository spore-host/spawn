package aws

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
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
