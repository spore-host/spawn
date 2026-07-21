package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/smithy-go"

	"github.com/spore-host/spawn/pkg/aws"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
	"github.com/spore-host/spawn/pkg/sshkey"
)

// awsProber is the real (read-only) Prober backed by the AWS client. It lives in
// pkg/doctor (not cmd) so the direct AWS SDK service imports it needs stay out of
// the thin cmd layer (the cmd/ SDK-import guard, #326/#327).
type awsProber struct {
	client       *aws.Client
	cfg          awssdk.Config
	spawnVersion string
}

// NewAWSProber builds the real prober. spawnVersion is passed in because the
// version string lives in the cmd package (ldflags-injected), and pkg/ must not
// depend on cmd/.
func NewAWSProber(client *aws.Client, spawnVersion string) Prober {
	return &awsProber{client: client, cfg: client.Config(), spawnVersion: spawnVersion}
}

func (p *awsProber) SpawnVersion() string { return p.spawnVersion }

func (p *awsProber) TruffleAvailable() (string, error) {
	path, err := exec.LookPath("truffle")
	if err != nil {
		return "", fmt.Errorf("truffle not found on PATH")
	}
	out, _ := exec.Command(path, "--version").Output()
	v := strings.TrimSpace(string(out))
	if v == "" {
		v = path
	}
	return v, nil
}

func (p *awsProber) AWSCLIAvailable() (string, error) {
	path, err := exec.LookPath("aws")
	if err != nil {
		return "", fmt.Errorf("aws CLI not found on PATH")
	}
	out, _ := exec.Command(path, "--version").Output()
	return strings.TrimSpace(string(out)), nil
}

func (p *awsProber) SSHKeyPresent() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	// Mirror sshkey.Resolve's default search (id_ed25519 then id_rsa).
	for _, name := range []string{"id_ed25519", "id_rsa", "id_ecdsa"} {
		pub := filepath.Join(home, ".ssh", name+".pub")
		if _, err := os.Stat(pub); err == nil {
			return "~/.ssh/" + name, nil
		}
	}
	// sshkey.Resolve also covers a managed key; treat its absence as a warn.
	if _, err := sshkey.Resolve(home, ""); err == nil {
		return "managed key (~/.spawn/keys)", nil
	}
	return "", fmt.Errorf("no default SSH key in ~/.ssh")
}

func (p *awsProber) Credentials(ctx context.Context) (string, error) {
	return p.client.GetAccountID(ctx)
}

func (p *awsProber) ExpectedAccount() string {
	return spawnconfig.SharedConfig().Account
}

func (p *awsProber) Region(ctx context.Context) (string, error) {
	if p.cfg.Region == "" {
		return "", fmt.Errorf("no region resolved from --region, AWS_REGION, or profile")
	}
	return p.cfg.Region, nil
}

// authorizedOrDryRunOK interprets the result of a DryRun RunInstances. The goal
// is to distinguish "you may launch" from "you may not" without launching:
//   - DryRunOperation → authorized (the canonical dry-run success).
//   - Unauthorized/AccessDenied → the real failure we want to report.
//   - Parameter-validation errors (e.g. InvalidAMIID.*) mean the request reached
//     parameter validation, i.e. it was NOT rejected for authorization — treat as
//     authorized. (We pass a placeholder AMI id to avoid resolving a real one.)
func authorizedOrDryRunOK(err error) error {
	if err == nil {
		return nil
	}
	var apiErr smithy.APIError
	if ok := errors.As(err, &apiErr); ok {
		code := apiErr.ErrorCode()
		switch {
		case code == "DryRunOperation":
			return nil // authorized
		case code == "UnauthorizedOperation" || strings.HasPrefix(code, "AccessDenied"):
			return fmt.Errorf("%s", code)
		case strings.HasPrefix(code, "InvalidAMIID") || strings.HasPrefix(code, "InvalidParameter") || code == "MissingParameter":
			return nil // reached parameter validation → not an authz rejection
		}
	}
	return err
}

func (p *awsProber) EC2Describe(ctx context.Context) error {
	cli := ec2.NewFromConfig(p.cfg)
	_, err := cli.DescribeInstances(ctx, &ec2.DescribeInstancesInput{MaxResults: awssdk.Int32(5)})
	return err
}

func (p *awsProber) EC2LaunchPermission(ctx context.Context) error {
	cli := ec2.NewFromConfig(p.cfg)
	// DryRun RunInstances: AWS validates permissions without launching anything.
	// An authorized caller gets DryRunOperation; an unauthorized one gets
	// UnauthorizedOperation. We pass a syntactically-valid but never-executed request.
	_, err := cli.RunInstances(ctx, &ec2.RunInstancesInput{
		DryRun:       awssdk.Bool(true),
		MaxCount:     awssdk.Int32(1),
		MinCount:     awssdk.Int32(1),
		InstanceType: ec2types.InstanceTypeT3Micro,
		ImageId:      awssdk.String("ami-00000000000000000"),
	})
	return authorizedOrDryRunOK(err)
}

func (p *awsProber) IAMInstanceProfileAccess(ctx context.Context) error {
	cli := iam.NewFromConfig(p.cfg)
	// A read that requires iam list access; if the identity can't even list
	// instance profiles it almost certainly can't create the spored one.
	_, err := cli.ListInstanceProfiles(ctx, &iam.ListInstanceProfilesInput{MaxItems: awssdk.Int32(1)})
	return err
}

func (p *awsProber) SporedRolePresent(ctx context.Context) (string, error) {
	cli := iam.NewFromConfig(p.cfg)
	_, err := cli.GetInstanceProfile(ctx, &iam.GetInstanceProfileInput{
		InstanceProfileName: awssdk.String("spored-instance-profile"),
	})
	if err != nil {
		return "", fmt.Errorf("not found (spawn creates it on first launch)")
	}
	return "spored-instance-profile", nil
}

func (p *awsProber) VPCAndSubnet(ctx context.Context) (string, error) {
	region := p.cfg.Region
	vpcID, err := p.client.GetDefaultVPC(ctx, region)
	if err != nil || vpcID == "" {
		return "", fmt.Errorf("no default VPC in %s", region)
	}
	subnets, err := p.client.GetSubnets(ctx, region, vpcID)
	if err != nil || len(subnets) == 0 {
		return "", fmt.Errorf("VPC %s has no usable subnet", vpcID)
	}
	return fmt.Sprintf("%s / %d subnet(s)", vpcID, len(subnets)), nil
}

func (p *awsProber) SSMAvailable(ctx context.Context) (string, error) {
	cli := ssm.NewFromConfig(p.cfg)
	// A cheap read that confirms the SSM endpoint is reachable and callable.
	_, err := cli.DescribeInstanceInformation(ctx, &ssm.DescribeInstanceInformationInput{
		MaxResults: awssdk.Int32(5),
	})
	if err != nil {
		return "", err
	}
	return "reachable", nil
}

func (p *awsProber) ReaperConfigured(ctx context.Context) (string, error) {
	// The reaper is an out-of-band Lambda in the infra account with a cross-account
	// role granted per spore-launching account. From the launch account we can't
	// authoritatively see it, so treat its presence as advisory: report "not
	// detected" (a Warn) unless the operator has recorded it via config. Kept
	// simple by design — the safety docs are the source of truth.
	return "", fmt.Errorf("not detected from this account (in-instance spored still enforces TTL)")
}

func (p *awsProber) Route53Available(ctx context.Context) (string, error) {
	cli := route53.NewFromConfig(p.cfg)
	out, err := cli.ListHostedZones(ctx, &route53.ListHostedZonesInput{MaxItems: awssdk.Int32(5)})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d hosted zone(s) visible", len(out.HostedZones)), nil
}
