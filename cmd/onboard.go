package cmd

// `spawn onboard` — the CLI path for bringing your AWS account onto the spore.host
// portal (the equivalent of the web CloudFormation quick-create). It:
//   1. resolves the caller's account via STS,
//   2. creates the `spore-portal-onboard` cross-account role that trusts the
//      portal phone-home Lambda role under a high-entropy per-account ExternalId
//      (confused-deputy guard), with the EC2 launch + SSM + scoped iam:PassRole
//      permissions the portal needs. On a re-run the existing role's ExternalId
//      is kept (read from its trust policy) and its policies are rewritten to
//      the current documents (#1138), and
//   3. SigV4-POSTs {roleArn, externalId, region} to the phone-home Function URL
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
	neturl "net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/spf13/cobra"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
	"github.com/spore-host/spawn/pkg/dns"
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
This is the CLI equivalent of the web CloudFormation quick-create.

Re-running is safe: an existing role keeps the ExternalId it already trusts
(pass --external-id to change it), and its trust and inline policies are
rewritten to the current documents. With -o json, stdout carries only the JSON
result; progress goes to stderr.`,
	RunE: runOnboard,
}

// Seams for the hermetic tests: the IAM calls, the caller-identity lookup and
// the phone-home HTTP client. Production values talk to AWS; the tests swap in
// recording fakes so nothing reaches IAM or the real portal endpoint.
var (
	onboardIAMClient = func(cfg aws.Config) onboardIAMAPI { return iam.NewFromConfig(cfg) }
	onboardCaller    = func(ctx context.Context, cfg aws.Config) (string, string, error) {
		return spawnaws.NewClientFromConfig(cfg).GetCallerIdentityInfo(ctx)
	}
	onboardHTTPClient = func() *http.Client { return http.DefaultClient }
)

// onboardIAMAPI is the slice of the IAM client spawn onboard uses.
type onboardIAMAPI interface {
	GetRole(ctx context.Context, in *iam.GetRoleInput, optFns ...func(*iam.Options)) (*iam.GetRoleOutput, error)
	CreateRole(ctx context.Context, in *iam.CreateRoleInput, optFns ...func(*iam.Options)) (*iam.CreateRoleOutput, error)
	UpdateAssumeRolePolicy(ctx context.Context, in *iam.UpdateAssumeRolePolicyInput, optFns ...func(*iam.Options)) (*iam.UpdateAssumeRolePolicyOutput, error)
	PutRolePolicy(ctx context.Context, in *iam.PutRolePolicyInput, optFns ...func(*iam.Options)) (*iam.PutRolePolicyOutput, error)
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

	jsonOut := onboardJSONOutput || getOutputFormat() == "json"
	// Progress goes to stderr under -o json, so stdout carries exactly one JSON
	// document and nothing else (#1138).
	progress := cmd.OutOrStdout()
	if jsonOut {
		progress = cmd.ErrOrStderr()
	}

	// Who are we onboarding?
	accountID, callerARN, err := onboardCaller(ctx, cfg)
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

	fmt.Fprintf(progress, "Onboarding account %s (region %s)…\n", accountID, region)
	// The ExternalId is decided inside ensurePortalOnboardRole: an existing role
	// already trusts one, and a re-run must keep presenting THAT one (#1138).
	roleARN, externalID, err := ensurePortalOnboardRole(ctx, onboardIAMClient(cfg), progress, accountID, phoneHomeRole, onboardExternalID, sporedRole)
	if err != nil {
		return fmt.Errorf("ensure onboarding role: %w", err)
	}
	fmt.Fprintf(progress, "  ✓ Role: %s\n", roleARN)

	// Register with the portal (unless skipped).
	phoneHomeURL := onboardPhoneHomeURL
	if phoneHomeURL == "" {
		phoneHomeURL = spawnconfig.GetPortalPhoneHomeURL()
	}
	if onboardSkipPhoneHome || phoneHomeURL == "" {
		if !onboardSkipPhoneHome {
			fmt.Fprintln(progress, "  ! No phone-home URL (set --phone-home-url or SPORE_PORTAL_PHONE_HOME_URL) — skipping auto-registration.")
			fmt.Fprintln(progress, "    Register manually in the portal with the role ARN + ExternalId below.")
		}
	} else {
		if err := phoneHome(ctx, cfg, phoneHomeURL, region, roleARN, externalID); err != nil {
			return fmt.Errorf("register with portal (phone-home): %w", err)
		}
		fmt.Fprintln(progress, "  ✓ Registered with the portal")
	}

	if jsonOut {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]string{
			"accountId":  accountID,
			"roleArn":    roleARN,
			"externalId": externalID,
			"region":     region,
			"caller":     callerARN,
		})
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "\nDone. Account %s is onboarded.\n", accountID)
	fmt.Fprintf(out, "  Role ARN:    %s\n", roleARN)
	fmt.Fprintf(out, "  ExternalId:  %s\n", externalID)
	fmt.Fprintln(out, "Keep the ExternalId safe — it's the guard the portal presents when assuming the role.")
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

// portalTrustPolicy is the onboarding role's trust policy: the portal
// phone-home Lambda role, under this account's ExternalId.
func portalTrustPolicy(phoneHomeRole, externalID string) string {
	return fmt.Sprintf(`{
		"Version": "2012-10-17",
		"Statement": [{
			"Effect": "Allow",
			"Principal": {"AWS": %q},
			"Action": "sts:AssumeRole",
			"Condition": {"StringEquals": {"sts:ExternalId": %q}}
		}]
	}`, phoneHomeRole, externalID)
}

// trustedExternalIDs returns every sts:ExternalId value an IAM trust policy
// document conditions on (any operator, string or list form). GetRole returns
// the document URL-encoded; both forms are accepted.
func trustedExternalIDs(doc string) ([]string, error) {
	if dec, err := neturl.QueryUnescape(doc); err == nil {
		doc = dec
	}
	var parsed struct {
		Statement json.RawMessage `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(doc), &parsed); err != nil {
		return nil, fmt.Errorf("parse trust policy: %w", err)
	}
	var stmts []struct {
		Condition map[string]map[string]json.RawMessage `json:"Condition"`
	}
	if err := json.Unmarshal(parsed.Statement, &stmts); err != nil {
		// A single statement may be an object rather than a list.
		var one struct {
			Condition map[string]map[string]json.RawMessage `json:"Condition"`
		}
		if err2 := json.Unmarshal(parsed.Statement, &one); err2 != nil {
			return nil, fmt.Errorf("parse trust policy statements: %w", err)
		}
		stmts = append(stmts, one)
	}
	seen := map[string]bool{}
	var ids []string
	for _, st := range stmts {
		for _, byKey := range st.Condition {
			for k, raw := range byKey {
				if !strings.EqualFold(k, "sts:ExternalId") {
					continue
				}
				var vals []string
				var one string
				if err := json.Unmarshal(raw, &one); err == nil {
					vals = []string{one}
				} else if err := json.Unmarshal(raw, &vals); err != nil {
					return nil, fmt.Errorf("parse sts:ExternalId condition: %w", err)
				}
				for _, v := range vals {
					if !seen[v] {
						seen[v] = true
						ids = append(ids, v)
					}
				}
			}
		}
	}
	return ids, nil
}

// ensurePortalOnboardRole makes the spore-portal-onboard role match what this
// version of spawn grants, and returns its ARN and the ExternalId it trusts.
//
// On a first run it creates the role under externalID, generating one when
// none was supplied. On a re-run (#1138) the role already trusts an ExternalId,
// and the portal must be told THAT one — so without --external-id it is read
// from the existing trust policy (refusing if it cannot be determined), and with
// --external-id the trust policy is updated to it. Either way the trust policy
// and both inline policies are rewritten to the current documents, so a role
// created by an older spawn (before #817's scoping) does not keep its old grants.
func ensurePortalOnboardRole(ctx context.Context, client onboardIAMAPI, progress io.Writer, accountID, phoneHomeRole, requestedExternalID, sporedRole string) (string, string, error) {
	var roleARN, externalID string

	existing, err := client.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(portalOnboardRoleName)})
	switch {
	case err == nil:
		role := existing.Role
		if !hasTag(role.Tags, "Purpose", "spore-portal-onboard") {
			return "", "", fmt.Errorf("a role named %s exists but is not tagged Purpose=spore-portal-onboard, so spawn did not create it — refusing to rewrite its trust or policies", portalOnboardRoleName)
		}
		roleARN = aws.ToString(role.Arn)
		trusted, perr := trustedExternalIDs(aws.ToString(role.AssumeRolePolicyDocument))
		switch {
		case requestedExternalID != "":
			externalID = requestedExternalID
		case perr != nil:
			return "", "", fmt.Errorf("role %s exists but its trust policy could not be read (%v) — re-run with --external-id set to the ExternalId the portal holds", portalOnboardRoleName, perr)
		case len(trusted) != 1:
			return "", "", fmt.Errorf("role %s exists and its trust policy names %d ExternalIds, not one — re-run with --external-id set to the ExternalId the portal holds", portalOnboardRoleName, len(trusted))
		default:
			externalID = trusted[0]
		}
		if _, err := client.UpdateAssumeRolePolicy(ctx, &iam.UpdateAssumeRolePolicyInput{
			RoleName:       aws.String(portalOnboardRoleName),
			PolicyDocument: aws.String(portalTrustPolicy(phoneHomeRole, externalID)),
		}); err != nil {
			return "", "", fmt.Errorf("update trust policy: %w", err)
		}
		if len(trusted) == 1 && trusted[0] == externalID {
			fmt.Fprintf(progress, "  ! Role %s already exists — keeping its ExternalId and refreshing its policies.\n", portalOnboardRoleName)
		} else {
			fmt.Fprintf(progress, "  ! Role %s already exists — its trust policy now requires the supplied ExternalId; the portal must be re-registered with it.\n", portalOnboardRoleName)
		}
	default:
		var notFound *iamtypes.NoSuchEntityException
		if !errors.As(err, &notFound) {
			return "", "", fmt.Errorf("get role: %w", err)
		}
		externalID = requestedExternalID
		if externalID == "" {
			if externalID, err = generateExternalID(); err != nil {
				return "", "", fmt.Errorf("generate external id: %w", err)
			}
		}
		created, err := client.CreateRole(ctx, &iam.CreateRoleInput{
			RoleName:                 aws.String(portalOnboardRoleName),
			AssumeRolePolicyDocument: aws.String(portalTrustPolicy(phoneHomeRole, externalID)),
			Description:              aws.String("Allows the spore.host portal to launch + manage EC2 in this account"),
			Tags: []iamtypes.Tag{
				{Key: aws.String("spawn:managed"), Value: aws.String("false")},
				{Key: aws.String("Purpose"), Value: aws.String("spore-portal-onboard")},
			},
		})
		if err != nil {
			return "", "", fmt.Errorf("create role: %w", err)
		}
		roleARN = aws.ToString(created.Role.Arn)
	}

	if err := putPortalOnboardPolicies(ctx, client, accountID, sporedRole); err != nil {
		return "", "", err
	}
	return roleARN, externalID, nil
}

func hasTag(tags []iamtypes.Tag, key, value string) bool {
	for _, t := range tags {
		if aws.ToString(t.Key) == key && aws.ToString(t.Value) == value {
			return true
		}
	}
	return false
}

// putPortalOnboardPolicies writes the role's two inline policies. PutRolePolicy
// replaces a policy of the same name, so this is what reconciles a re-run.
func putPortalOnboardPolicies(ctx context.Context, client onboardIAMAPI, accountID, sporedRole string) error {
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
		return fmt.Errorf("attach launch policy: %w", err)
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
		return fmt.Errorf("attach passrole policy: %w", err)
	}
	return nil
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
	// Sign for the endpoint's region, read from the Function URL host — not the
	// onboarding region, which is only data in the body. The portal's Function
	// URL lives in one region; signing for --region made every other --region
	// fail the signature check (#1138). Same rule as pkg/dns.
	signRegion := dns.SigningRegion(url, "")
	if err := v4.NewSigner().SignHTTP(ctx, creds, httpReq, hex.EncodeToString(sum[:]), "lambda", signRegion, time.Now()); err != nil {
		return fmt.Errorf("sign phone-home request: %w", err)
	}

	resp, err := onboardHTTPClient().Do(httpReq)
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
	onboardCmd.Flags().StringVar(&onboardExternalID, "external-id", "", "ExternalId the role must trust (default: the existing role's, else a new one)")
	onboardCmd.Flags().BoolVar(&onboardSkipPhoneHome, "skip-phone-home", false, "Create the role but don't auto-register with the portal")
	onboardCmd.Flags().BoolVar(&onboardJSONOutput, "json", false, "Output the result as JSON")
	// Canonical JSON output is the root -o/--output json; keep --json as a
	// deprecated alias for consistency with the rest of the CLI (spawn#40).
	_ = onboardCmd.Flags().MarkDeprecated("json", "use --output json instead")
}
