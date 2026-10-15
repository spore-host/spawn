package cmd

// `spawn onboard` — the CLI path for bringing your AWS account onto the spore.host
// portal (the equivalent of the web CloudFormation quick-create). It:
//   1. resolves the caller's account via STS,
//   2. generates a high-entropy per-account ExternalId (confused-deputy guard),
//   3. creates (idempotently) the `spore-portal-onboard` cross-account role that
//      trusts the portal phone-home Lambda role under that ExternalId, with the
//      EC2 launch + SSM + scoped iam:PassRole permissions the portal needs, and
//   4. SigV4-POSTs {roleArn, externalId, region} to the phone-home Function URL
//      so the portal auto-registers the account — no copy-paste.
//
// The phone-home endpoint is AuthType: AWS_IAM, so the signed POST lets the
// handler derive THIS account as the verified caller and bind the registration
// to it (mirrors the dns-updater model; see pkg/dns/client.go for the signing).

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/spf13/cobra"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
)

const portalOnboardRoleName = "spore-portal-onboard"

var (
	onboardRegion        string
	onboardPhoneHomeURL  string
	onboardPhoneHomeRole string
	onboardSporedRole    string
	onboardExternalID    string
	onboardSkipPhoneHome bool
	onboardJSONOutput    bool
)

var onboardCmd = &cobra.Command{
	Use:   "onboard",
	Short: "Bring this AWS account onto the spore.host portal (BYOA)",
	Long: `Create the cross-account role the spore.host portal uses to launch and
manage EC2 in this account, and register it with the portal.

Run with credentials for the account you want to onboard (SSO, profile, or keys).
This is the CLI equivalent of the web CloudFormation quick-create.`,
	RunE: runOnboard,
}

func runOnboard(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	cfg, err := loadAWSConfig(ctx, onboardRegion)
	if err != nil {
		return fmt.Errorf("load AWS config: %w", err)
	}
	if onboardRegion != "" {
		cfg.Region = onboardRegion
	}
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}

	// Who are we onboarding?
	accountID, callerARN, err := spawnaws.NewClientFromConfig(cfg).GetCallerIdentityInfo(ctx)
	if err != nil {
		return fmt.Errorf("get caller identity: %w", err)
	}

	// The phone-home Lambda role this account's onboarding role will trust.
	phoneHomeRole := onboardPhoneHomeRole
	if phoneHomeRole == "" {
		phoneHomeRole = spawnconfig.GetPortalPhoneHomeRoleARN()
	}
	if phoneHomeRole == "" {
		return fmt.Errorf("no phone-home Lambda role ARN — set --phone-home-role or SPORE_PORTAL_PHONE_HOME_ROLE_ARN")
	}

	// Per-account ExternalId (confused-deputy guard). Reuse if supplied (re-runs).
	externalID := onboardExternalID
	if externalID == "" {
		externalID, err = generateExternalID()
		if err != nil {
			return fmt.Errorf("generate external id: %w", err)
		}
	}

	// iam:PassRole names a ROLE. This defaulted to "spored-instance-profile" — an
	// instance PROFILE name in a role ARN slot — so the grant could never match
	// any role and the portal's RunInstances was denied (#817). The role is
	// aws.SporedRoleName; the profile of the same family is a different object.
	//
	// Reinterpreting the flag's value as a role name is safe: the previous
	// behaviour granted PassRole on an ARN that cannot exist, so anyone who set
	// it was already getting a denied launch.
	sporedRole := onboardSporedRole
	if sporedRole == "" {
		sporedRole = spawnaws.SporedRoleName
	}

	fmt.Printf("Onboarding account %s (region %s)…\n", accountID, region)
	roleARN, err := ensurePortalOnboardRole(ctx, cfg, accountID, phoneHomeRole, externalID, sporedRole)
	if err != nil {
		return fmt.Errorf("ensure onboarding role: %w", err)
	}
	fmt.Printf("  ✓ Role: %s\n", roleARN)

	// Register with the portal (unless skipped).
	phoneHomeURL := onboardPhoneHomeURL
	if phoneHomeURL == "" {
		phoneHomeURL = spawnconfig.GetPortalPhoneHomeURL()
	}
	if onboardSkipPhoneHome || phoneHomeURL == "" {
		if !onboardSkipPhoneHome {
			fmt.Println("  ! No phone-home URL (set --phone-home-url or SPORE_PORTAL_PHONE_HOME_URL) — skipping auto-registration.")
			fmt.Println("    Register manually in the portal with the role ARN + ExternalId below.")
		}
	} else {
		if err := phoneHome(ctx, cfg, phoneHomeURL, region, roleARN, externalID); err != nil {
			return fmt.Errorf("register with portal (phone-home): %w", err)
		}
		fmt.Println("  ✓ Registered with the portal")
	}

	if onboardJSONOutput || getOutputFormat() == "json" {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]string{
			"accountId":  accountID,
			"roleArn":    roleARN,
			"externalId": externalID,
			"region":     region,
			"caller":     callerARN,
		})
	}
	fmt.Printf("\nDone. Account %s is onboarded.\n", accountID)
	fmt.Printf("  Role ARN:    %s\n", roleARN)
	fmt.Printf("  ExternalId:  %s\n", externalID)
	fmt.Println("Keep the ExternalId safe — it's the guard the portal presents when assuming the role.")
	return nil
}

// generateExternalID returns a 32-hex-char high-entropy ExternalId.
func generateExternalID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ensurePortalOnboardRole creates the spore-portal-onboard role (idempotently)
// trusting phoneHomeRole under externalID, with the portal launch permissions.
// Returns the role ARN. If the role already exists, returns it unchanged (re-run
// safe) — note an existing role keeps its original trust/ExternalId.
func ensurePortalOnboardRole(ctx context.Context, cfg aws.Config, accountID, phoneHomeRole, externalID, sporedRole string) (string, error) {
	client := iam.NewFromConfig(cfg)

	if existing, err := client.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(portalOnboardRoleName)}); err == nil {
		fmt.Printf("  ! Role %s already exists — leaving its trust policy unchanged.\n", portalOnboardRoleName)
		return *existing.Role.Arn, nil
	} else {
		var notFound *iamtypes.NoSuchEntityException
		if !errors.As(err, &notFound) {
			return "", fmt.Errorf("get role: %w", err)
		}
	}

	trustPolicy := fmt.Sprintf(`{
		"Version": "2012-10-17",
		"Statement": [{
			"Effect": "Allow",
			"Principal": {"AWS": %q},
			"Action": "sts:AssumeRole",
			"Condition": {"StringEquals": {"sts:ExternalId": %q}}
		}]
	}`, phoneHomeRole, externalID)

	created, err := client.CreateRole(ctx, &iam.CreateRoleInput{
		RoleName:                 aws.String(portalOnboardRoleName),
		AssumeRolePolicyDocument: aws.String(trustPolicy),
		Description:              aws.String("Allows the spore.host portal to launch + manage EC2 in this account"),
		Tags: []iamtypes.Tag{
			{Key: aws.String("spawn:managed"), Value: aws.String("false")},
			{Key: aws.String("Purpose"), Value: aws.String("spore-portal-onboard")},
		},
	})
	if err != nil {
		return "", fmt.Errorf("create role: %w", err)
	}

	// EC2 launch + lifecycle + SSM. Every action that can affect an instance is
	// scoped to spawn:managed; the unconditioned grants are read-only or create-only.
	//
	// Three statements here were tightened in #817. Each had the same shape: an
	// action that can reach an EXISTING instance, granted on "*" with no
	// condition, sitting beside destructive actions that were carefully
	// conditioned — so the condition provided no containment.
	//
	//  1. ec2:CreateTags was unconditioned, which is the #174 defect
	//     (pkg/aws/iam.go:912 documents it: "a compromised spore could tag ANY
	//     instance spawn:managed=true and then terminate it"). That fix landed in
	//     the spored policies and never reached here. It is split in two below
	//     because this role LAUNCHES: tagging at RunInstances time cannot satisfy
	//     a resource-tag condition, since the tag is what is being created — so
	//     that case uses ec2:CreateAction instead.
	//
	//  2. ssm:StartSession had no tag condition at all, so a portal compromise
	//     yielded an interactive root shell on arbitrary NON-spawn production
	//     instances in every onboarded account. It is split from the SSM document
	//     grant because StartSession authorizes against both the instance and the
	//     document, and a document carries no spawn:managed tag — one statement
	//     covering both would condition the document too and deny everything.
	//
	//     The condition key is ssm:resourceTag, NOT ec2:ResourceTag. A condition
	//     key belongs to the service of the ACTION: ec2:ResourceTag is absent from
	//     the request context of an ssm:* call, so a StringEquals on it can never
	//     match and the statement becomes a silent implicit DENY. That trap is
	//     #665, and deployment/cloudformation/ttl-reaper-cross-account-role.yaml
	//     documents it at length.
	launchPolicy := `{
		"Version": "2012-10-17",
		"Statement": [
			{"Effect":"Allow","Action":["ec2:RunInstances","ec2:DescribeInstances","ec2:DescribeInstanceStatus","ec2:DescribeImages","ec2:DescribeSubnets","ec2:DescribeSecurityGroups","ec2:DescribeVpcs","ec2:DescribeKeyPairs"],"Resource":"*"},
			{"Effect":"Allow","Action":["ec2:CreateTags"],"Resource":"*","Condition":{"StringEquals":{"ec2:CreateAction":"RunInstances"}}},
			{"Effect":"Allow","Action":["ec2:CreateTags","ec2:DeleteTags"],"Resource":"*","Condition":{"StringEquals":{"ec2:ResourceTag/spawn:managed":"true"}}},
			{"Effect":"Allow","Action":["ec2:TerminateInstances","ec2:StopInstances","ec2:StartInstances","ec2:CancelSpotInstanceRequests"],"Resource":"*","Condition":{"StringEquals":{"aws:ResourceTag/spawn:managed":"true"}}},
			{"Effect":"Allow","Action":["ec2:DescribeSpotPriceHistory","servicequotas:GetServiceQuota","servicequotas:ListServiceQuotas","pricing:GetProducts"],"Resource":"*"},
			{"Effect":"Allow","Action":["ssm:StartSession"],"Resource":"arn:aws:ec2:*:*:instance/*","Condition":{"StringEquals":{"ssm:resourceTag/spawn:managed":"true"}}},
			{"Effect":"Allow","Action":["ssm:StartSession"],"Resource":"arn:aws:ssm:*::document/SSM-SessionManagerRunShell"},
			{"Effect":"Allow","Action":["ssm:TerminateSession","ssm:ResumeSession"],"Resource":"arn:aws:ssm:*:*:session/*"}
		]
	}`
	if _, err := client.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
		RoleName:       aws.String(portalOnboardRoleName),
		PolicyName:     aws.String("PortalEC2Launch"),
		PolicyDocument: aws.String(launchPolicy),
	}); err != nil {
		return "", fmt.Errorf("attach launch policy: %w", err)
	}

	// iam:PassRole scoped to the single spored ROLE + PassedToService ec2.
	passPolicy := fmt.Sprintf(`{
		"Version": "2012-10-17",
		"Statement": [{
			"Effect":"Allow","Action":"iam:PassRole",
			"Resource":"arn:aws:iam::%s:role/%s",
			"Condition":{"StringEquals":{"iam:PassedToService":"ec2.amazonaws.com"}}
		}]
	}`, accountID, sporedRole)
	if _, err := client.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
		RoleName:       aws.String(portalOnboardRoleName),
		PolicyName:     aws.String("PortalPassSporedProfile"),
		PolicyDocument: aws.String(passPolicy),
	}); err != nil {
		return "", fmt.Errorf("attach passrole policy: %w", err)
	}

	return *created.Role.Arn, nil
}

// phoneHome SigV4-signs (lambda service) and POSTs the registration to the
// AuthType: AWS_IAM Function URL, so the handler sees this account as the
// verified caller. Mirrors pkg/dns/client.go's signing.
func phoneHome(ctx context.Context, cfg aws.Config, url, region, roleARN, externalID string) error {
	body, err := json.Marshal(map[string]string{
		"roleArn":    roleARN,
		"externalId": externalID,
		"region":     region,
	})
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	creds, err := cfg.Credentials.Retrieve(ctx)
	if err != nil {
		return fmt.Errorf("retrieve credentials for signing: %w", err)
	}
	if creds.AccessKeyID == "" {
		return fmt.Errorf("resolved AWS credentials are empty — the AWS_IAM phone-home URL will reject an unsigned request")
	}
	sum := sha256.Sum256(body)
	if err := v4.NewSigner().SignHTTP(ctx, creds, httpReq, hex.EncodeToString(sum[:]), "lambda", region, time.Now()); err != nil {
		return fmt.Errorf("sign phone-home request: %w", err)
	}

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("POST phone-home: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := strings.TrimSpace(string(respBody))
		if len(snippet) > 300 {
			snippet = snippet[:300] + "…"
		}
		return fmt.Errorf("phone-home returned HTTP %d: %s", resp.StatusCode, snippet)
	}
	return nil
}

func init() {
	rootCmd.AddCommand(onboardCmd)
	onboardCmd.Flags().StringVar(&onboardRegion, "region", "", "AWS region to onboard (default: profile/SDK region)")
	onboardCmd.Flags().StringVar(&onboardPhoneHomeURL, "phone-home-url", "", "Portal phone-home Function URL (or SPORE_PORTAL_PHONE_HOME_URL)")
	onboardCmd.Flags().StringVar(&onboardPhoneHomeRole, "phone-home-role", "", "Portal phone-home Lambda role ARN to trust (or SPORE_PORTAL_PHONE_HOME_ROLE_ARN)")
	onboardCmd.Flags().StringVar(&onboardSporedRole, "spored-role", "", "Role name to allow iam:PassRole for (default "+spawnaws.SporedRoleName+")")
	// --spored-profile was the old name and took a PROFILE name, which could never
	// match an iam:PassRole resource ARN (#817). Kept as a deprecated alias bound
	// to the same variable so existing invocations keep working — and now actually
	// work, since the value is finally used as a role name.
	onboardCmd.Flags().StringVar(&onboardSporedRole, "spored-profile", "", "Deprecated: use --spored-role")
	if err := onboardCmd.Flags().MarkDeprecated("spored-profile", "use --spored-role; iam:PassRole names a role, not an instance profile"); err != nil {
		panic(err)
	}
	onboardCmd.Flags().StringVar(&onboardExternalID, "external-id", "", "Reuse a specific ExternalId (default: generate a new one)")
	onboardCmd.Flags().BoolVar(&onboardSkipPhoneHome, "skip-phone-home", false, "Create the role but don't auto-register with the portal")
	onboardCmd.Flags().BoolVar(&onboardJSONOutput, "json", false, "Output the result as JSON")
	// Canonical JSON output is the root -o/--output json; keep --json as a
	// deprecated alias for consistency with the rest of the CLI (spawn#40).
	_ = onboardCmd.Flags().MarkDeprecated("json", "use --output json instead")
}
