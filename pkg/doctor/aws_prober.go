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
	truffleaws "github.com/spore-host/truffle/pkg/aws"
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
	// This used to be a hardcoded "not detected" (spawn#624), on the reasoning that a
	// launch account cannot see a reaper that lives elsewhere. The effect was a check
	// that printed the SAME warning in every account — including accounts the reaper
	// does cover — so it carried no information, and a warning that always fires is
	// one users learn to scroll past. An account with a genuinely absent reaper read
	// exactly like one with a healthy one.
	//
	// Coverage does leave local evidence; see aws.DetectReaperCoverage.
	c := aws.DetectReaperCoverage(ctx, p.cfg)
	switch {
	case c.Covered:
		return c.How, nil
	case c.Determined:
		return "", fmt.Errorf("no reaper runs in this account and no %s role grants one access — "+
			"TTL is enforced only from inside the instance by spored, which cannot act on a STOPPED "+
			"instance and does nothing if it dies. Deploy one here with 'spawn reaper deploy' "+
			"(spawn#625); it lands in dry-run until you arm it", aws.ReaperCoverageRoleName)
	default:
		return "", fmt.Errorf("could not determine coverage (%s)", c.Why)
	}
}

// NitroCoverage summarises the resolved region's Nitro fleet by generation.
//
// The generation itself comes from truffle, which is the suite's capability
// authority — and it has to come from a table, because EC2's Hypervisor field
// distinguishes nitro from xen and carries no version. AWS publishes the version
// only in documentation. truffle's table is kept honest against AWS by its own
// `make nitro-census`.
//
// Why this is a doctor check and not a launch line. Coverage is an ENVIRONMENT
// fact and it varies a lot: us-east-1 offers 163 Nitro families (44 of them v6)
// against us-west-1's 70 (9 v6). That constrains what you can run in a region,
// which is the same shape as doctor's other checks — is there a usable subnet, is
// SSM reachable, does a reaper cover this account.
//
// It is deliberately NOT on `launch`. The capabilities that matter at launch time
// — EFA, cluster placement, hibernation — are already checked from their own
// authoritative API fields in preflightInstanceConstraints, which is better than
// inferring them from a generation. And an ordinary launch makes no
// GetCapabilities call at all, so a generation line would add an API round-trip
// to every launch for something most launches do not act on.
func (p *awsProber) NitroCoverage(ctx context.Context) (string, error) {
	// PAGINATED. DescribeInstanceTypes returns one page by default, and the first
	// version of this reported 85 Nitro types for us-east-1 against a real 163 —
	// numbers that reflected a page boundary rather than the region, and differed
	// between regions for the same reason. A count that looks plausible and is
	// wrong is worse than no count.
	// Counted by FAMILY, not by instance type. A family is what you choose
	// between — c7g versus c8g — while the sizes inside one are a scaling
	// decision, so "1149 Nitro instance types" is a number nobody can act on
	// where "163 Nitro families" is. It also matches how truffle keys its table.
	seen := map[string]bool{}
	byGen := map[int]int{}
	nitro, unclassified := 0, 0
	pager := ec2.NewDescribeInstanceTypesPaginator(ec2.NewFromConfig(p.cfg),
		&ec2.DescribeInstanceTypesInput{})
	for pager.HasMorePages() {
		out, err := pager.NextPage(ctx)
		if err != nil {
			return "", fmt.Errorf("could not list instance types: %w", err)
		}
		for _, it := range out.InstanceTypes {
			if it.Hypervisor != ec2types.InstanceTypeHypervisorNitro {
				continue
			}
			fam := truffleaws.InstanceFamily(string(it.InstanceType))
			if seen[fam] {
				continue
			}
			seen[fam] = true
			nitro++
			switch g := truffleaws.NitroGeneration(string(it.InstanceType)); g {
			case 0:
				// truffle's table does not classify this family. Counted rather
				// than guessed — a fabricated generation is indistinguishable
				// from a real one downstream, and an unclassified family is also
				// the signal that a new Nitro card may have shipped (truffle's
				// coverage gate).
				unclassified++
			default:
				byGen[g]++
			}
		}
	}
	if nitro == 0 {
		return "", errors.New("no Nitro instance families found in this region, which is unexpected — check the region is correct")
	}

	// Newest first: what you can reach matters more than what you cannot.
	var parts []string
	newest := 0
	for g := 6; g >= 2; g-- {
		if byGen[g] > 0 {
			parts = append(parts, fmt.Sprintf("v%d:%d", g, byGen[g]))
			if g > newest {
				newest = g
			}
		}
	}
	detail := fmt.Sprintf("%d Nitro families (%s)", nitro, strings.Join(parts, " "))
	if unclassified > 0 {
		detail += fmt.Sprintf(", %d unclassified", unclassified)
	}

	// Warn only when the region has nothing current. v4 is the floor worth
	// flagging: it is where ENA Express and RDMA arrive, so a region without it
	// cannot run the networking-sensitive workloads spawn is often used for.
	if newest < 4 {
		return "", fmt.Errorf("this region's newest Nitro generation is v%d — no ENA Express or RDMA here, and no current-generation instance types (%s)", newest, detail)
	}
	return detail, nil
}

func (p *awsProber) Route53Available(ctx context.Context) (string, error) {
	cli := route53.NewFromConfig(p.cfg)
	out, err := cli.ListHostedZones(ctx, &route53.ListHostedZonesInput{MaxItems: awssdk.Int32(5)})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d hosted zone(s) visible", len(out.HostedZones)), nil
}
